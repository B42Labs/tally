package pricing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/b42labs/tally/internal/engine/store/sqlcgen"
)

// ErrVersionConflict is what Import returns for a version the database already
// holds under other prices. An invoice names the version it was rated from and
// has to stay reproducible from it, so a stored version keeps pricing what it
// priced when the invoice was written.
var ErrVersionConflict = errors.New(
	"this version is already imported and prices something else, and a corrected price belongs in a new version")

// ErrNoModel is what ForPeriod returns for a period that begins before every
// stored version. Nothing prices it, and rating it against no model would bill
// every metered resource at zero rather than say what is missing.
var ErrNoModel = errors.New("no pricing model is valid for this period")

// ErrVersionNotFound is what ByVersion returns for a version no stored model
// carries. A correction rates with the version its finalized run recorded, and
// a version the database does not hold prices nothing, so the correction is
// refused before it opens a run rather than rated against no model.
var ErrVersionNotFound = errors.New("no pricing model is stored under this version")

// The unique key over valid_from, as Postgres names it and as it reports a
// write that breaks it. Matching the constraint rather than the SQLSTATE alone
// is what keeps another unique violation from being reported as two versions
// claiming one instant.
const (
	uniqueViolation     = "23505"
	validFromConstraint = "pricing_models_valid_from_key"
)

// Import stores one version of the pricing model and reports whether the
// database already held it. It only ever inserts: the document a version was
// rated from is the document that stays stored under it, and a price that
// changes is imported under a new version instead of over an old one.
//
// A version the database does not hold is written and Import returns false. A
// version it holds is read back and compared against m: equal models make the
// import a replay, which returns true and leaves the row alone, and unequal
// ones an error wrapping ErrVersionConflict. A second version claiming the
// valid_from of a stored one is refused too, because one instant is priced by
// one version. That collision is its own error rather than a version conflict:
// the version is new, it is the instant it starts at that is taken.
func Import(ctx context.Context, q *sqlcgen.Queries, m Model, doc []byte) (bool, error) {
	inserted, err := q.InsertPricingModel(ctx, sqlcgen.InsertPricingModelParams{
		Version:   m.Version,
		ValidFrom: pgtype.Timestamptz{Time: m.ValidFrom, Valid: true},
		Currency:  m.Currency,
		Document:  doc,
	})
	if err != nil {
		// The insert passes over a collision on the version, so valid_from is
		// the only unique key a write still breaks.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == validFromConstraint {
			return false, fmt.Errorf(
				"importing pricing model %s: another version already starts at valid_from %s, and one instant is priced by one version",
				m.Version, m.ValidFrom.UTC().Format(time.RFC3339))
		}
		return false, fmt.Errorf("importing pricing model %s: %w", m.Version, err)
	}
	if inserted > 0 {
		return false, nil
	}

	// Nothing was written, so the version is stored already. What tells a
	// replay from a conflict is the document under it, read back through the
	// same schema and the same parse the file went through.
	stored, err := q.GetPricingModel(ctx, m.Version)
	if err != nil {
		return false, fmt.Errorf("reading the stored pricing model %s: %w", m.Version, err)
	}
	storedModel, err := ParseDocument(stored.Document)
	if err != nil {
		return false, fmt.Errorf("reading the stored pricing model %s: %w", m.Version, err)
	}
	if !storedModel.Equal(m) {
		return false, fmt.Errorf("importing pricing model %s: %w", m.Version, ErrVersionConflict)
	}
	return true, nil
}

// ForPeriod returns the version that prices the period beginning at
// periodFrom: the newest one whose valid_from is at or before that instant. A
// version that becomes valid later prices later periods only, so importing the
// prices of April does not reprice March.
//
// A period that begins before every stored version yields an error wrapping
// ErrNoModel.
func ForPeriod(ctx context.Context, q *sqlcgen.Queries, periodFrom time.Time) (Model, error) {
	row, err := q.PricingModelForPeriod(ctx, pgtype.Timestamptz{Time: periodFrom, Valid: true})
	if err != nil {
		start := periodFrom.UTC().Format(time.RFC3339)
		if errors.Is(err, pgx.ErrNoRows) {
			return Model{}, fmt.Errorf("selecting the pricing model for the period beginning %s: %w", start, ErrNoModel)
		}
		return Model{}, fmt.Errorf("selecting the pricing model for the period beginning %s: %w", start, err)
	}

	model, err := ParseDocument(row.Document)
	if err != nil {
		return Model{}, fmt.Errorf("reading the stored pricing model %s: %w", row.Version, err)
	}
	return model, nil
}

// ByVersion returns the stored model of one version. It is what a correction
// rates with: a correction rates a finalized month with the version the
// finalized run recorded, whatever was imported since, so it corrects the
// usage of that month and leaves its prices where they were (D6).
//
// A version no stored model carries yields an error wrapping
// ErrVersionNotFound.
func ByVersion(ctx context.Context, q *sqlcgen.Queries, version string) (Model, error) {
	row, err := q.GetPricingModel(ctx, version)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Model{}, fmt.Errorf("reading the pricing model %s: %w", version, ErrVersionNotFound)
		}
		return Model{}, fmt.Errorf("reading the pricing model %s: %w", version, err)
	}

	model, err := ParseDocument(row.Document)
	if err != nil {
		return Model{}, fmt.Errorf("reading the stored pricing model %s: %w", version, err)
	}
	return model, nil
}
