package pricing_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/b42labs/tally/internal/engine/pricing"
	"github.com/b42labs/tally/internal/engine/store/sqlcgen"
	"github.com/b42labs/tally/internal/engine/store/storetest"
)

// importBase is the version the import tests store first. Two dimensions give
// a reordering something to reorder, and the counter price spelled "0.5" gives
// a respelling something to respell.
const importBase = `version: "2026-04"
valid_from: "2026-04-01T00:00:00Z"
currency: "EUR"
pricing:
  openstack:
    instance:
      dimensions:
        - metric: "vcpus"
          type: "time_gauge"
          price_per_unit_hour: "0.02"
        - metric: "egress_gb"
          type: "counter"
          price_per_unit: "0.5"
`

// importRespelled is importBase with the top-level keys in another order and
// the counter price written as a number. It is a different file and a different
// document, and it prices exactly what importBase prices.
const importRespelled = `currency: "EUR"
pricing:
  openstack:
    instance:
      dimensions:
        - metric: "vcpus"
          type: "time_gauge"
          price_per_unit_hour: "0.02"
        - metric: "egress_gb"
          type: "counter"
          price_per_unit: 0.50
valid_from: "2026-04-01T00:00:00Z"
version: "2026-04"
`

// importChangedPrice raises the price of one dimension of importBase.
const importChangedPrice = `version: "2026-04"
valid_from: "2026-04-01T00:00:00Z"
currency: "EUR"
pricing:
  openstack:
    instance:
      dimensions:
        - metric: "vcpus"
          type: "time_gauge"
          price_per_unit_hour: "0.03"
        - metric: "egress_gb"
          type: "counter"
          price_per_unit: "0.5"
`

// importAddedDimension prices one metric of importBase more.
const importAddedDimension = `version: "2026-04"
valid_from: "2026-04-01T00:00:00Z"
currency: "EUR"
pricing:
  openstack:
    instance:
      dimensions:
        - metric: "vcpus"
          type: "time_gauge"
          price_per_unit_hour: "0.02"
        - metric: "ram_gb"
          type: "time_gauge"
          price_per_unit_hour: "0.005"
        - metric: "egress_gb"
          type: "counter"
          price_per_unit: "0.5"
`

// importReordered lists the dimensions of importBase the other way around,
// which reorders the rated records they produce.
const importReordered = `version: "2026-04"
valid_from: "2026-04-01T00:00:00Z"
currency: "EUR"
pricing:
  openstack:
    instance:
      dimensions:
        - metric: "egress_gb"
          type: "counter"
          price_per_unit: "0.5"
        - metric: "vcpus"
          type: "time_gauge"
          price_per_unit_hour: "0.02"
`

// importSameValidFrom is a version of its own starting at the instant
// importBase starts at, which is the collision the unique key on valid_from
// refuses.
const importSameValidFrom = `version: "2026-04-fix"
valid_from: "2026-04-01T00:00:00Z"
currency: "EUR"
pricing:
  openstack:
    instance:
      dimensions:
        - metric: "vcpus"
          type: "time_gauge"
          price_per_unit_hour: "0.03"
`

// monthlyModel is the smallest model that prices anything. The selection tests
// import several of them, so the version, the instant and the price are filled
// in per month.
const monthlyModel = `version: %q
valid_from: %q
currency: "EUR"
pricing:
  openstack:
    instance:
      dimensions:
        - metric: "vcpus"
          type: "time_gauge"
          price_per_unit_hour: %q
`

// month is one version built from monthlyModel.
type month struct {
	version   string
	validFrom string
	price     string
}

// storedRow is a pricing_models row as a test reads it back, past the package
// under test, to see what an import wrote.
type storedRow struct {
	version    string
	validFrom  time.Time
	currency   string
	document   []byte
	importedAt time.Time
}

// parseModel parses a model a test imports and hands back the canonical
// document beside it, which is what Import stores.
func parseModel(t *testing.T, document string) (pricing.Model, []byte) {
	t.Helper()

	model, doc, err := pricing.Parse([]byte(document))
	if err != nil {
		t.Fatalf("Parse() error = %v, want nil", err)
	}
	return model, doc
}

// importMonths imports every month into a database that holds none of them and
// returns the models by version.
func importMonths(t *testing.T, q *sqlcgen.Queries, months ...month) map[string]pricing.Model {
	t.Helper()

	models := make(map[string]pricing.Model, len(months))
	for _, m := range months {
		model, doc := parseModel(t, fmt.Sprintf(monthlyModel, m.version, m.validFrom, m.price))
		already, err := pricing.Import(t.Context(), q, model, doc)
		if err != nil {
			t.Fatalf("Import(%s) error = %v, want nil", m.version, err)
		}
		if already {
			t.Fatalf("Import(%s) alreadyImported = true, want false for a version the database does not hold", m.version)
		}
		models[m.version] = model
	}
	return models
}

// readRow reads one stored version.
func readRow(t *testing.T, db storetest.DB, version string) storedRow {
	t.Helper()

	var row storedRow
	if err := db.Store.Pool().QueryRow(t.Context(),
		`SELECT version, valid_from, currency, document, imported_at FROM pricing_models WHERE version = $1`,
		version,
	).Scan(&row.version, &row.validFrom, &row.currency, &row.document, &row.importedAt); err != nil {
		t.Fatalf("reading the stored pricing model %s: %v", version, err)
	}
	return row
}

// assertUnchanged fails when the row of want is no longer what want holds. A
// refused import has to leave the stored version exactly as it was, down to
// the instant it was imported at.
func assertUnchanged(t *testing.T, db storetest.DB, want storedRow) {
	t.Helper()

	got := readRow(t, db, want.version)
	if !got.validFrom.Equal(want.validFrom) || got.currency != want.currency ||
		!bytes.Equal(got.document, want.document) || !got.importedAt.Equal(want.importedAt) {
		t.Errorf("the stored row of %s = (%s, %s, %s, %s), want it unchanged at (%s, %s, %s, %s)",
			want.version, got.validFrom, got.currency, got.document, got.importedAt,
			want.validFrom, want.currency, want.document, want.importedAt)
	}
}

// countModels is the number of stored versions.
func countModels(t *testing.T, db storetest.DB) int {
	t.Helper()

	var count int
	if err := db.Store.Pool().QueryRow(t.Context(), `SELECT count(*) FROM pricing_models`).Scan(&count); err != nil {
		t.Fatalf("counting the stored pricing models: %v", err)
	}
	return count
}

// sameDocument reports whether two documents hold the same JSON. Postgres
// stores jsonb as a parsed value and writes it back in its own key order, so
// the comparison goes through a decode rather than over the bytes.
func sameDocument(t *testing.T, got, want []byte) bool {
	t.Helper()

	return reflect.DeepEqual(decodeDocument(t, got), decodeDocument(t, want))
}

// decodeDocument decodes a document into the generic values the comparison
// walks. Numbers keep the text they are written with, so a price is never
// compared as a float.
func decodeDocument(t *testing.T, doc []byte) any {
	t.Helper()

	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.UseNumber()

	var value any
	if err := dec.Decode(&value); err != nil {
		t.Fatalf("decoding the document %s: %v", doc, err)
	}
	return value
}

func TestImport(t *testing.T) {
	db := storetest.NewDB(t)
	q := sqlcgen.New(db.Store.Pool())

	model, doc := parseModel(t, importBase)

	already, err := pricing.Import(t.Context(), q, model, doc)
	if err != nil {
		t.Fatalf("Import() error = %v, want nil", err)
	}
	if already {
		t.Error("Import() alreadyImported = true, want false for a version the database does not hold")
	}

	first := readRow(t, db, model.Version)
	if !first.validFrom.Equal(model.ValidFrom) {
		t.Errorf("valid_from = %s, want %s", first.validFrom, model.ValidFrom)
	}
	if first.currency != model.Currency {
		t.Errorf("currency = %q, want %q", first.currency, model.Currency)
	}
	if !sameDocument(t, first.document, doc) {
		t.Errorf("document = %s, want %s", first.document, doc)
	}
	if count := countModels(t, db); count != 1 {
		t.Errorf("stored versions = %d, want 1", count)
	}

	// The version has to come back as the model it went in as: that round trip
	// is what an import of an existing version is decided by.
	stored, err := q.GetPricingModel(t.Context(), model.Version)
	if err != nil {
		t.Fatalf("GetPricingModel() error = %v, want nil", err)
	}
	storedModel, err := pricing.ParseDocument(stored.Document)
	if err != nil {
		t.Fatalf("ParseDocument() error = %v, want nil", err)
	}
	if !storedModel.Equal(model) {
		t.Error("the stored model does not equal the imported one, want them equal")
	}

	t.Run("the same file again is a replay", func(t *testing.T) {
		already, err := pricing.Import(t.Context(), q, model, doc)
		if err != nil {
			t.Fatalf("Import() error = %v, want nil", err)
		}
		if !already {
			t.Error("Import() alreadyImported = false, want true for the version the database holds")
		}
		if count := countModels(t, db); count != 1 {
			t.Errorf("stored versions = %d, want 1", count)
		}
		assertUnchanged(t, db, first)
	})

	t.Run("a respelled file of the same prices is a replay", func(t *testing.T) {
		respelled, respelledDoc := parseModel(t, importRespelled)
		if bytes.Equal(respelledDoc, doc) {
			t.Fatalf("the respelled document equals the stored one, want the test to import other bytes: %s", respelledDoc)
		}

		already, err := pricing.Import(t.Context(), q, respelled, respelledDoc)
		if err != nil {
			t.Fatalf("Import() error = %v, want nil", err)
		}
		if !already {
			t.Error("Import() alreadyImported = false, want true for a version that only respells its prices")
		}
		if count := countModels(t, db); count != 1 {
			t.Errorf("stored versions = %d, want 1", count)
		}
		assertUnchanged(t, db, first)
	})

	t.Run("a changed version is refused", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			document string
		}{
			{"a changed price", importChangedPrice},
			{"an added dimension", importAddedDimension},
			{"reordered dimensions", importReordered},
		} {
			t.Run(tc.name, func(t *testing.T) {
				changed, changedDoc := parseModel(t, tc.document)

				already, err := pricing.Import(t.Context(), q, changed, changedDoc)
				if !errors.Is(err, pricing.ErrVersionConflict) {
					t.Fatalf("Import() error = %v, want one matching ErrVersionConflict", err)
				}
				if !strings.Contains(err.Error(), changed.Version) {
					t.Errorf("Import() error = %q, want it to name the version %q", err, changed.Version)
				}
				if already {
					t.Error("Import() alreadyImported = true, want false beside a refused import")
				}
				assertUnchanged(t, db, first)
			})
		}
	})

	t.Run("a second version cannot start where a stored one starts", func(t *testing.T) {
		other, otherDoc := parseModel(t, importSameValidFrom)

		_, err := pricing.Import(t.Context(), q, other, otherDoc)
		if err == nil {
			t.Fatal("Import() error = nil, want the collision on valid_from reported")
		}
		if !strings.Contains(err.Error(), "valid_from") {
			t.Errorf("Import() error = %q, want it to name valid_from", err)
		}
		// The instant is what the operator has to move, and naming it is also
		// what tells the mapped collision from the driver's own text.
		if want := other.ValidFrom.Format(time.RFC3339); !strings.Contains(err.Error(), want) {
			t.Errorf("Import() error = %q, want it to name the instant %s the two versions share", err, want)
		}
		// The version is new, it is the instant it starts at that is taken, so
		// nothing about it conflicts with the prices of another version.
		if errors.Is(err, pricing.ErrVersionConflict) {
			t.Error("Import() error matches ErrVersionConflict, want the collision reported as its own failure")
		}
		if _, err := q.GetPricingModel(t.Context(), other.Version); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("GetPricingModel(%s) error = %v, want pgx.ErrNoRows for a version that was refused",
				other.Version, err)
		}
	})

	t.Run("reports a database it cannot reach", func(t *testing.T) {
		// A pool of its own: closing the one the test runs on would take the
		// remaining reads and the cleanup with it.
		pool, err := pgxpool.New(t.Context(), db.URL)
		if err != nil {
			t.Fatalf("opening a second pool: %v", err)
		}
		pool.Close()

		_, err = pricing.Import(t.Context(), sqlcgen.New(pool), model, doc)
		if err == nil {
			t.Fatal("Import() error = nil, want the closed pool reported")
		}
		if errors.Is(err, pricing.ErrVersionConflict) || errors.Is(err, pricing.ErrNoModel) {
			t.Errorf("Import() error = %v, want the database failure rather than one of the package's own", err)
		}
		if !strings.Contains(err.Error(), model.Version) {
			t.Errorf("Import() error = %q, want it to name the version %q", err, model.Version)
		}
	})

	t.Run("the committed example model round-trips", func(t *testing.T) {
		example, exampleDoc := parseModel(t, string(readFile(t, examplePath)))

		already, err := pricing.Import(t.Context(), q, example, exampleDoc)
		if err != nil {
			t.Fatalf("Import() error = %v, want nil", err)
		}
		if already {
			t.Error("Import() alreadyImported = true, want false for a version the database does not hold")
		}

		// A file an operator actually holds, imported twice: reimporting one
		// is the routine an unchanged version has to survive.
		again, err := pricing.Import(t.Context(), q, example, exampleDoc)
		if err != nil {
			t.Fatalf("Import() error = %v, want nil", err)
		}
		if !again {
			t.Error("Import() alreadyImported = false, want true for the version the database holds")
		}
	})
}

func TestForPeriod(t *testing.T) {
	db := storetest.NewDB(t)
	q := sqlcgen.New(db.Store.Pool())

	models := importMonths(t, q,
		month{"2026-01", "2026-01-01T00:00:00Z", "0.01"},
		month{"2026-03", "2026-03-01T00:00:00Z", "0.03"},
		month{"2026-05", "2026-05-01T00:00:00Z", "0.05"},
	)

	for _, tc := range []struct {
		periodFrom time.Time
		want       string
	}{
		// A period the newest version starts before is priced by that version
		// and never by a later one.
		{time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), "2026-01"},
		// The instant a version becomes valid belongs to that version.
		{time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), "2026-03"},
		{time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC), "2026-03"},
		{time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), "2026-05"},
		{time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), "2026-05"},
	} {
		t.Run(tc.periodFrom.Format("2006-01"), func(t *testing.T) {
			got, err := pricing.ForPeriod(t.Context(), q, tc.periodFrom)
			if err != nil {
				t.Fatalf("ForPeriod() error = %v, want nil", err)
			}
			if got.Version != tc.want {
				t.Fatalf("ForPeriod() version = %q, want %q", got.Version, tc.want)
			}
			if !got.Equal(models[tc.want]) {
				t.Error("ForPeriod() does not equal the imported model, want them equal")
			}
		})
	}

	t.Run("a period before every version has no model", func(t *testing.T) {
		start := time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC)

		got, err := pricing.ForPeriod(t.Context(), q, start)
		if !errors.Is(err, pricing.ErrNoModel) {
			t.Fatalf("ForPeriod() error = %v, want one matching ErrNoModel", err)
		}
		if want := start.Format(time.RFC3339); !strings.Contains(err.Error(), want) {
			t.Errorf("ForPeriod() error = %q, want it to name the period start %s", err, want)
		}
		if got.Version != "" {
			t.Errorf("ForPeriod() version = %q, want the zero model beside the error", got.Version)
		}
	})

	// Last, because it leaves a version behind that no longer reads. A stored
	// document stops satisfying the schema when the schema is tightened under
	// versions that were written before it, and what an operator needs then is
	// the version to look at rather than a bare pointer into a document.
	t.Run("names the version of a stored document it cannot read", func(t *testing.T) {
		const version = "2026-05"
		if _, err := db.Store.Pool().Exec(t.Context(),
			`UPDATE pricing_models SET document = $2::jsonb WHERE version = $1`,
			// Empty on purpose: what names the version in either error is the
			// wrapping, not something the document still spells.
			version, `{}`,
		); err != nil {
			t.Fatalf("overwriting the stored document of %s: %v", version, err)
		}

		start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
		if _, err := pricing.ForPeriod(t.Context(), q, start); err == nil {
			t.Error("ForPeriod() error = nil, want the unreadable document reported")
		} else if !strings.Contains(err.Error(), version) {
			t.Errorf("ForPeriod() error = %q, want it to name the version %s", err, version)
		}

		// Import reads the stored document too, to tell a replay from a
		// conflict, and one it cannot read is neither.
		model, doc := parseModel(t, fmt.Sprintf(monthlyModel, version, "2026-05-01T00:00:00Z", "0.05"))
		already, err := pricing.Import(t.Context(), q, model, doc)
		if err == nil {
			t.Error("Import() error = nil, want the unreadable document reported")
		} else if !strings.Contains(err.Error(), version) {
			t.Errorf("Import() error = %q, want it to name the version %s", err, version)
		}
		if already {
			t.Error("Import() alreadyImported = true, want false beside a document that does not read")
		}
	})
}

func TestListPricingModels(t *testing.T) {
	db := storetest.NewDB(t)
	q := sqlcgen.New(db.Store.Pool())

	// Imported out of order on purpose: the listing is chronological by
	// valid_from, not by the order the versions were imported in.
	importMonths(t, q,
		month{"2026-05", "2026-05-01T00:00:00Z", "0.05"},
		month{"2026-01", "2026-01-01T00:00:00Z", "0.01"},
		month{"2026-03", "2026-03-01T00:00:00Z", "0.03"},
	)

	rows, err := q.ListPricingModels(t.Context())
	if err != nil {
		t.Fatalf("ListPricingModels() error = %v, want nil", err)
	}

	got := make([]string, 0, len(rows))
	for _, row := range rows {
		got = append(got, row.Version)
	}
	if want := []string{"2026-01", "2026-03", "2026-05"}; !slices.Equal(got, want) {
		t.Errorf("ListPricingModels() = %v, want %v", got, want)
	}
}
