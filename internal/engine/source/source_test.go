package source_test

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/b42labs/tally/internal/core/event"
	"github.com/b42labs/tally/internal/engine/source"
	"github.com/b42labs/tally/internal/reporting/store/storetest"
)

// The fixture every seeded row shares. Two clouds are enough to tell a filtered
// read from an unfiltered one.
const (
	platform     = "openstack"
	cloud        = "os-prod-eu1"
	otherCloud   = "os-prod-us1"
	projectID    = "tenant-a"
	relationType = "infrastructure_tenant"
)

// activePayload is a payload the envelope decodes whole: a state and a size
// whose fields the metering fold reads back verbatim.
const activePayload = `{"state":"active","size":{"vcpus":2,"flavor":"m1.small"}}`

// insufficientPrivilege is the SQLSTATE Postgres reports for a statement the
// connected role has no privilege for.
const insufficientPrivilege = "42501"

// The billing period the reads below are taken over, and the instants the seeds
// place rows around it.
var (
	periodFrom = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodTo   = time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	longBefore   = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	beforePeriod = time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	insidePeriod = time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	afterPeriod  = time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
)

// TestCandidates pins the predicate the period's work list is built from. The
// projection keeps a row after the resource is gone, so what decides is how the
// row's lifetime lies against the period rather than whether the resource is
// still there.
func TestCandidates(t *testing.T) {
	db := storetest.NewDB(t)

	t.Run("lists nothing while the projection is empty", func(t *testing.T) {
		snap := openSnapshot(t, db.URL)

		got, err := snap.Candidates(t.Context(), []string{cloud}, periodFrom, periodTo)
		if err != nil {
			t.Fatalf("Candidates() error = %v, want nil", err)
		}
		wantEmpty(t, "Candidates()", got)
	})

	otherCloudResource := source.Resource{
		Cloud: otherCloud, Platform: platform, ResourceType: "instance", ResourceID: "g-other-cloud",
	}
	for _, row := range []struct {
		resource  source.Resource
		createdAt *time.Time
		deletedAt *time.Time
	}{
		// Deliberately not in the order the query returns them: the resource
		// types and ids sort the other way round.
		{resource("volume", "b-deleted-after"), at(beforePeriod), at(afterPeriod)},
		{resource("instance", "b-created-inside"), at(insidePeriod), nil},
		{resource("volume", "a-deleted-inside"), at(longBefore), at(time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC))},
		{resource("instance", "a-alive-before"), at(beforePeriod), nil},
		// A row the projection wrote from an update it saw before any create.
		{resource("instance", "c-created-null"), nil, nil},
		// Deleted at the first instant of the period, which the period still
		// bills: the predicate is deleted_at >= period_from.
		{resource("instance", "d-deleted-at-period-start"), at(longBefore), at(periodFrom)},
		// Created at the first instant after the period, so it produced nothing
		// inside it.
		{resource("instance", "e-created-at-period-end"), at(periodTo), nil},
		{resource("instance", "f-deleted-before"), at(longBefore), at(time.Date(2026, 2, 28, 0, 0, 0, 0, time.UTC))},
		{otherCloudResource, at(beforePeriod), nil},
	} {
		seedResource(t, db.Store.Pool(), row.resource, row.createdAt, row.deletedAt)
	}

	// Ordered by cloud, resource type, resource id, which is what the metering
	// loop walks.
	wantEU1 := []source.Resource{
		resource("instance", "a-alive-before"),
		resource("instance", "b-created-inside"),
		resource("instance", "c-created-null"),
		resource("instance", "d-deleted-at-period-start"),
		resource("volume", "a-deleted-inside"),
		resource("volume", "b-deleted-after"),
	}

	t.Run("selects every resource that lived during the period", func(t *testing.T) {
		snap := openSnapshot(t, db.URL)

		got, err := snap.Candidates(t.Context(), []string{cloud}, periodFrom, periodTo)
		if err != nil {
			t.Fatalf("Candidates() error = %v, want nil", err)
		}
		if !reflect.DeepEqual(got, wantEU1) {
			t.Errorf("Candidates() = %+v, want %+v", got, wantEU1)
		}
	})

	t.Run("meters every cloud when no cloud is named", func(t *testing.T) {
		want := append(slices.Clone(wantEU1), otherCloudResource)

		for name, clouds := range map[string][]string{"nil": nil, "empty": {}} {
			t.Run(name, func(t *testing.T) {
				snap := openSnapshot(t, db.URL)

				got, err := snap.Candidates(t.Context(), clouds, periodFrom, periodTo)
				if err != nil {
					t.Fatalf("Candidates() error = %v, want nil", err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("Candidates() = %+v, want %+v", got, want)
				}
			})
		}
	})
}

// TestHistory pins what one candidate's fold is handed: the events of that
// resource up to the period end, in the order the fold depends on, decoded the
// way the projection decodes the same rows.
func TestHistory(t *testing.T) {
	db := storetest.NewDB(t)
	pool := db.Store.Pool()

	metered := resource("instance", "i-1")
	tie := time.Date(2026, 3, 7, 8, 0, 0, 0, time.UTC)
	created := time.Date(2026, 3, 5, 10, 0, 0, 0, time.UTC)
	updated := time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC)

	seedEvent(t, pool, metered, "ev-1", "instance.create", created, created.Add(5*time.Second), activePayload)
	seedEvent(t, pool, metered, "ev-2", "instance.update", updated, updated.Add(9*time.Second), nil)
	// Two events at the same instant, inserted with the later received_at
	// first and named so that the event id alone would order them the other way
	// round.
	seedEvent(t, pool, metered, "ev-a-tie", "instance.update", tie, tie.Add(30*time.Second), activePayload)
	seedEvent(t, pool, metered, "ev-b-tie", "instance.update", tie, tie.Add(10*time.Second), activePayload)
	// The period end is exclusive, so neither of these belongs to the run.
	seedEvent(t, pool, metered, "ev-at-period-end", "instance.update", periodTo, periodTo, activePayload)
	seedEvent(t, pool, metered, "ev-after-period", "instance.delete", afterPeriod, afterPeriod, nil)
	// Another resource of the same cloud: its history is not this one's.
	seedEvent(t, pool, resource("instance", "i-2"), "ev-other", "instance.create", created, created, activePayload)

	active := "active"
	want := []event.Stored{
		{
			Event: event.Event{
				EventID: "ev-1", Timestamp: created, EventType: "instance.create",
				Platform: platform, Cloud: cloud, ResourceType: "instance", ResourceID: "i-1",
				ProjectID: projectID, Source: event.SourceCollector,
				Payload: event.PayloadEnvelope{
					State: &active,
					Size:  map[string]any{"vcpus": float64(2), "flavor": "m1.small"},
				},
			},
			ReceivedAt: created.Add(5 * time.Second),
		},
		{
			// The NULL payload column, which carries no envelope at all.
			Event: event.Event{
				EventID: "ev-2", Timestamp: updated, EventType: "instance.update",
				Platform: platform, Cloud: cloud, ResourceType: "instance", ResourceID: "i-1",
				ProjectID: projectID, Source: event.SourceCollector,
			},
			ReceivedAt: updated.Add(9 * time.Second),
		},
		{
			Event: event.Event{
				EventID: "ev-b-tie", Timestamp: tie, EventType: "instance.update",
				Platform: platform, Cloud: cloud, ResourceType: "instance", ResourceID: "i-1",
				ProjectID: projectID, Source: event.SourceCollector,
				Payload: event.PayloadEnvelope{
					State: &active,
					Size:  map[string]any{"vcpus": float64(2), "flavor": "m1.small"},
				},
			},
			ReceivedAt: tie.Add(10 * time.Second),
		},
		{
			Event: event.Event{
				EventID: "ev-a-tie", Timestamp: tie, EventType: "instance.update",
				Platform: platform, Cloud: cloud, ResourceType: "instance", ResourceID: "i-1",
				ProjectID: projectID, Source: event.SourceCollector,
				Payload: event.PayloadEnvelope{
					State: &active,
					Size:  map[string]any{"vcpus": float64(2), "flavor": "m1.small"},
				},
			},
			ReceivedAt: tie.Add(30 * time.Second),
		},
	}

	t.Run("loads the resource's events in fold order", func(t *testing.T) {
		snap := openSnapshot(t, db.URL)

		got, err := snap.History(t.Context(), metered, periodTo)
		if err != nil {
			t.Fatalf("History() error = %v, want nil", err)
		}

		gotIDs := make([]string, 0, len(got))
		for _, stored := range got {
			gotIDs = append(gotIDs, stored.EventID)
		}
		wantIDs := []string{"ev-1", "ev-2", "ev-b-tie", "ev-a-tie"}
		if !slices.Equal(gotIDs, wantIDs) {
			t.Fatalf("History() event ids = %v, want %v", gotIDs, wantIDs)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("History() = %+v, want %+v", got, want)
		}
		for _, stored := range got {
			if stored.Timestamp.Location() != time.UTC {
				t.Errorf("History() event %s timestamp zone = %v, want UTC",
					stored.EventID, stored.Timestamp.Location())
			}
			if stored.ReceivedAt.Location() != time.UTC {
				t.Errorf("History() event %s received_at zone = %v, want UTC",
					stored.EventID, stored.ReceivedAt.Location())
			}
		}
	})

	t.Run("loads no events for a resource that has none", func(t *testing.T) {
		snap := openSnapshot(t, db.URL)

		got, err := snap.History(t.Context(), resource("instance", "i-without-events"), periodTo)
		if err != nil {
			t.Fatalf("History() error = %v, want nil", err)
		}
		wantEmpty(t, "History()", got)
	})

	t.Run("names the event whose payload it cannot decode", func(t *testing.T) {
		// A payload that is valid JSON but not an object. The column takes it,
		// the envelope does not, and a fold started on the rest of the history
		// would meter a resource whose state nobody read.
		bad := resource("instance", "i-bad-payload")
		seedEvent(t, pool, bad, "ev-bad", "instance.update", insidePeriod, insidePeriod, `[1,2]`)

		snap := openSnapshot(t, db.URL)

		_, err := snap.History(t.Context(), bad, periodTo)
		if err == nil {
			t.Fatal("History() error = nil, want the undecodable payload reported")
		}
		if want := "decoding the payload of event ev-bad:"; !strings.Contains(err.Error(), want) {
			t.Errorf("History() error = %q, want it to contain %q", err, want)
		}
	})
}

// TestSnapshot pins what makes the transaction a snapshot: a run reads one
// version of the reporting data however long it takes, and it knows which
// version that was.
func TestSnapshot(t *testing.T) {
	db := storetest.NewDB(t)
	pool := db.Store.Pool()

	t.Run("records the time it was taken in UTC", func(t *testing.T) {
		snap := openSnapshot(t, db.URL)

		var now time.Time
		if err := pool.QueryRow(t.Context(), "SELECT now()").Scan(&now); err != nil {
			t.Fatalf("reading the database clock: %v", err)
		}
		if snap.At.IsZero() {
			t.Error("Snapshot().At is the zero time, want the database clock")
		}
		if snap.At.Location() != time.UTC {
			t.Errorf("Snapshot().At zone = %v, want UTC", snap.At.Location())
		}
		if snap.At.After(now) {
			t.Errorf("Snapshot().At = %v, want it at or before %v, read after the snapshot opened",
				snap.At, now)
		}
	})

	t.Run("hides the rows written after it opened", func(t *testing.T) {
		metered := resource("instance", "iso-1")
		seedResource(t, pool, metered, at(beforePeriod), nil)
		seedEvent(t, pool, metered, "iso-ev-1", "instance.create", insidePeriod, insidePeriod, activePayload)

		snap := openSnapshot(t, db.URL)

		// A second connection writes what a collector would write mid-run.
		late := resource("instance", "iso-2")
		seedResource(t, pool, late, at(insidePeriod), nil)
		seedEvent(t, pool, metered, "iso-ev-2", "instance.update",
			insidePeriod.Add(time.Hour), insidePeriod.Add(time.Hour), activePayload)

		history, err := snap.History(t.Context(), metered, periodTo)
		if err != nil {
			t.Fatalf("History() error = %v, want nil", err)
		}
		if ids := eventIDs(history); !slices.Equal(ids, []string{"iso-ev-1"}) {
			t.Errorf("History() event ids = %v, want only the event seeded before the snapshot", ids)
		}
		candidates, err := snap.Candidates(t.Context(), []string{cloud}, periodFrom, periodTo)
		if err != nil {
			t.Fatalf("Candidates() error = %v, want nil", err)
		}
		if slices.Contains(candidates, late) {
			t.Errorf("Candidates() = %+v, want the row written after the snapshot left out", candidates)
		}

		if err := snap.Close(t.Context()); err != nil {
			t.Fatalf("Close() error = %v, want nil", err)
		}

		fresh := openSnapshot(t, db.URL)
		history, err = fresh.History(t.Context(), metered, periodTo)
		if err != nil {
			t.Fatalf("History() error = %v, want nil", err)
		}
		if ids := eventIDs(history); !slices.Equal(ids, []string{"iso-ev-1", "iso-ev-2"}) {
			t.Errorf("History() event ids = %v, want both events", ids)
		}
		candidates, err = fresh.Candidates(t.Context(), []string{cloud}, periodFrom, periodTo)
		if err != nil {
			t.Fatalf("Candidates() error = %v, want nil", err)
		}
		if !slices.Contains(candidates, late) {
			t.Errorf("Candidates() = %+v, want it to contain %+v", candidates, late)
		}
	})

	t.Run("closes twice without reporting a failure", func(t *testing.T) {
		snap := openSnapshot(t, db.URL)

		for range 2 {
			if err := snap.Close(t.Context()); err != nil {
				t.Errorf("Close() error = %v, want nil", err)
			}
		}
	})
}

// TestProjectGraph pins the two loaders attribution walks: the registry whole,
// and the relations whose validity overlaps the period (D4).
func TestProjectGraph(t *testing.T) {
	db := storetest.NewDB(t)
	pool := db.Store.Pool()

	t.Run("lists no projects while the registry is empty", func(t *testing.T) {
		snap := openSnapshot(t, db.URL)

		got, err := snap.Projects(t.Context())
		if err != nil {
			t.Fatalf("Projects() error = %v, want nil", err)
		}
		wantEmpty(t, "Projects()", got)
	})

	// Seeded in an order the query returns them in only if it sorts.
	far := source.Project{ID: uuid.New(), Platform: platform, Cloud: otherCloud, ExternalID: "z-tenant"}
	second := source.Project{ID: uuid.New(), Platform: platform, Cloud: cloud, ExternalID: "b-tenant"}
	first := source.Project{ID: uuid.New(), Platform: platform, Cloud: cloud, ExternalID: "a-tenant"}
	for _, project := range []source.Project{far, second, first} {
		seedProject(t, pool, project)
	}

	t.Run("lists every project ordered by cloud and external id", func(t *testing.T) {
		snap := openSnapshot(t, db.URL)

		got, err := snap.Projects(t.Context())
		if err != nil {
			t.Fatalf("Projects() error = %v, want nil", err)
		}
		want := []source.Project{first, second, far}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Projects() = %+v, want %+v", got, want)
		}
	})

	endsInside := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
	endedBefore := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	open := source.Relation{
		ID: uuid.New(), SourceID: second.ID, TargetID: first.ID,
		RelationType: relationType, ValidFrom: longBefore,
	}
	closedInside := source.Relation{
		ID: uuid.New(), SourceID: first.ID, TargetID: second.ID,
		RelationType: relationType, ValidFrom: beforePeriod, ValidTo: at(endsInside),
	}
	startedInside := source.Relation{
		ID: uuid.New(), SourceID: far.ID, TargetID: second.ID,
		RelationType: relationType, ValidFrom: insidePeriod,
	}
	for _, relation := range []source.Relation{
		open, closedInside, startedInside,
		// Closed at the first instant of the period, which no longer overlaps
		// it: the predicate is valid_to > period_from.
		{
			ID: uuid.New(), SourceID: far.ID, TargetID: first.ID, RelationType: relationType,
			ValidFrom: longBefore, ValidTo: at(periodFrom),
		},
		// Opened at the first instant after the period.
		{
			ID: uuid.New(), SourceID: first.ID, TargetID: far.ID, RelationType: relationType,
			ValidFrom: periodTo,
		},
		{
			ID: uuid.New(), SourceID: second.ID, TargetID: far.ID, RelationType: relationType,
			ValidFrom: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), ValidTo: at(endedBefore),
		},
		// Overlapping, but of a type attribution was not asked to walk.
		{
			ID: uuid.New(), SourceID: second.ID, TargetID: first.ID, RelationType: "managed_by",
			ValidFrom: longBefore,
		},
	} {
		seedRelation(t, pool, relation)
	}

	t.Run("lists the relations overlapping the period", func(t *testing.T) {
		snap := openSnapshot(t, db.URL)

		got, err := snap.Relations(t.Context(), []string{relationType}, periodFrom, periodTo)
		if err != nil {
			t.Fatalf("Relations() error = %v, want nil", err)
		}
		// Ordered by id, which Postgres compares byte-wise.
		want := []source.Relation{open, closedInside, startedInside}
		slices.SortFunc(want, func(a, b source.Relation) int { return bytes.Compare(a.ID[:], b.ID[:]) })
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Relations() = %+v, want %+v", got, want)
		}
	})

	t.Run("runs no query for an empty relation type list", func(t *testing.T) {
		// Attribution turned off, which is what an explicitly empty
		// TALLY_ENGINE_ATTRIBUTING_RELATION_TYPES configures. A closed snapshot
		// is how the test sees that no statement was sent.
		snap := openSnapshot(t, db.URL)
		if err := snap.Close(t.Context()); err != nil {
			t.Fatalf("Close() error = %v, want nil", err)
		}

		for name, types := range map[string][]string{"nil": nil, "empty": {}} {
			t.Run(name, func(t *testing.T) {
				got, err := snap.Relations(t.Context(), types, periodFrom, periodTo)
				if err != nil {
					t.Fatalf("Relations() error = %v, want nil", err)
				}
				wantEmpty(t, "Relations()", got)
			})
		}
	})
}

// TestReaderRole is the deployment's story: the engine connects as a member of
// tally_engine_reader, and that membership carries every read this package
// makes and nothing beyond them.
func TestReaderRole(t *testing.T) {
	const (
		readerRole     = "source_test_reader"
		readerPassword = "source-test-password"
	)

	db := storetest.NewDB(t)
	pool := db.Store.Pool()

	metered := resource("instance", "reader-1")
	seedResource(t, pool, metered, at(beforePeriod), nil)
	seedEvent(t, pool, metered, "reader-ev-1", "instance.create", insidePeriod, insidePeriod, activePayload)
	attributor := source.Project{ID: uuid.New(), Platform: platform, Cloud: cloud, ExternalID: "reader-attributor"}
	tenant := source.Project{ID: uuid.New(), Platform: platform, Cloud: cloud, ExternalID: "reader-tenant"}
	seedProject(t, pool, attributor)
	seedProject(t, pool, tenant)
	relation := source.Relation{
		ID: uuid.New(), SourceID: tenant.ID, TargetID: attributor.ID,
		RelationType: relationType, ValidFrom: beforePeriod,
	}
	seedRelation(t, pool, relation)

	if _, err := pool.Exec(t.Context(),
		"CREATE ROLE "+readerRole+" LOGIN PASSWORD '"+readerPassword+"'"); err != nil {
		t.Fatalf("creating the login role: %v", err)
	}
	if _, err := pool.Exec(t.Context(), "GRANT tally_engine_reader TO "+readerRole); err != nil {
		t.Fatalf("granting the reader role: %v", err)
	}
	parsed, err := url.Parse(db.URL)
	if err != nil {
		t.Fatalf("parsing the database url: %v", err)
	}
	parsed.User = url.UserPassword(readerRole, readerPassword)
	readerURL := parsed.String()

	t.Run("reads everything a run meters", func(t *testing.T) {
		snap := openSnapshot(t, readerURL)

		// The grant on the events hypertable has to reach its chunks, which is
		// where the row actually is.
		history, err := snap.History(t.Context(), metered, periodTo)
		if err != nil {
			t.Fatalf("History() error = %v, want nil", err)
		}
		if ids := eventIDs(history); !slices.Equal(ids, []string{"reader-ev-1"}) {
			t.Errorf("History() event ids = %v, want [reader-ev-1]", ids)
		}

		candidates, err := snap.Candidates(t.Context(), []string{cloud}, periodFrom, periodTo)
		if err != nil {
			t.Fatalf("Candidates() error = %v, want nil", err)
		}
		if !slices.Equal(candidates, []source.Resource{metered}) {
			t.Errorf("Candidates() = %+v, want %+v", candidates, []source.Resource{metered})
		}

		projects, err := snap.Projects(t.Context())
		if err != nil {
			t.Fatalf("Projects() error = %v, want nil", err)
		}
		if want := []source.Project{attributor, tenant}; !reflect.DeepEqual(projects, want) {
			t.Errorf("Projects() = %+v, want %+v", projects, want)
		}

		relations, err := snap.Relations(t.Context(), []string{relationType}, periodFrom, periodTo)
		if err != nil {
			t.Fatalf("Relations() error = %v, want nil", err)
		}
		if want := []source.Relation{relation}; !reflect.DeepEqual(relations, want) {
			t.Errorf("Relations() = %+v, want %+v", relations, want)
		}
	})

	t.Run("writes nothing and reads no credentials", func(t *testing.T) {
		conn, err := pgx.Connect(t.Context(), readerURL)
		if err != nil {
			t.Fatalf("connecting as %s: %v", readerRole, err)
		}
		t.Cleanup(func() {
			if err := conn.Close(context.Background()); err != nil {
				t.Errorf("closing the reader connection: %v", err)
			}
		})

		_, err = conn.Exec(t.Context(),
			`INSERT INTO events (event_id, timestamp, event_type, platform, cloud,
			                     resource_type, resource_id, project_id)
			 VALUES ('reader-write', $1, 'instance.update', $2, $3, 'instance', 'reader-1', $4)`,
			insidePeriod, platform, cloud, projectID)
		if err == nil {
			t.Error("INSERT INTO events succeeded, want the reader role denied every write")
		} else if code := sqlState(err); code != insufficientPrivilege {
			t.Errorf("INSERT INTO events error = %v (SQLSTATE %q), want SQLSTATE %q",
				err, code, insufficientPrivilege)
		}

		var tokens int64
		err = conn.QueryRow(t.Context(), "SELECT count(*) FROM api_tokens").Scan(&tokens)
		if err == nil {
			t.Errorf("SELECT FROM api_tokens returned %d rows, want the reader role denied the table", tokens)
		} else if code := sqlState(err); code != insufficientPrivilege {
			t.Errorf("SELECT FROM api_tokens error = %v (SQLSTATE %q), want SQLSTATE %q",
				err, code, insufficientPrivilege)
		}
	})
}

// TestNew covers what fails before any row is read: the connection string the
// pool cannot be built from, and the database it cannot reach. Neither needs a
// database, because neither gets that far.
// TestReadsReportTheQueryThatFailed pins what an operator reads when a read
// fails: every method names its own query and hands the driver's error on, so
// an incident is diagnosed from the message rather than from the stack. The
// snapshot is closed first, which is the one failure every read shares.
func TestReadsReportTheQueryThatFailed(t *testing.T) {
	db := storetest.NewDB(t)
	snap := openSnapshot(t, db.URL)
	if err := snap.Close(t.Context()); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}

	metered := resource("instance", "i-1")
	for name, tc := range map[string]struct {
		read func() error
		want string
	}{
		"Candidates": {
			read: func() error {
				_, err := snap.Candidates(t.Context(), []string{cloud}, periodFrom, periodTo)
				return err
			},
			want: "listing the candidate resources:",
		},
		"History": {
			read: func() error {
				_, err := snap.History(t.Context(), metered, periodTo)
				return err
			},
			// The one message that names the row it was reading.
			want: "loading the history of " + cloud + "/instance/i-1:",
		},
		"Projects": {
			read: func() error {
				_, err := snap.Projects(t.Context())
				return err
			},
			want: "listing the projects:",
		},
		"Relations": {
			read: func() error {
				_, err := snap.Relations(t.Context(), []string{relationType}, periodFrom, periodTo)
				return err
			},
			want: "listing the project relations:",
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := tc.read()
			if err == nil {
				t.Fatalf("%s() error = nil, want the failed query reported", name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%s() error = %q, want it to contain %q", name, err, tc.want)
			}
			if !errors.Is(err, pgx.ErrTxClosed) {
				t.Errorf("%s() error = %v, want the driver's error wrapped", name, err)
			}
		})
	}
}

func TestNew(t *testing.T) {
	t.Run("reports a url it cannot parse", func(t *testing.T) {
		_, err := source.New(t.Context(), "postgres://[::1:bad")
		if err == nil {
			t.Fatal("New() error = nil, want the unparseable url reported")
		}
		if want := "parsing the reporting database url:"; !strings.Contains(err.Error(), want) {
			t.Errorf("New() error = %q, want it to contain %q", err, want)
		}
	})

	t.Run("reports the database it cannot reach when the snapshot opens", func(t *testing.T) {
		// Port 1 refuses the connection. Building the pool still succeeds: it
		// dials on the first use, which is the snapshot.
		db, err := source.New(t.Context(), "postgres://u:p@127.0.0.1:1/db?sslmode=disable&connect_timeout=2")
		if err != nil {
			t.Fatalf("New() error = %v, want nil", err)
		}
		defer db.Close()

		_, err = db.Snapshot(t.Context())
		if err == nil {
			t.Fatal("Snapshot() error = nil, want the unreachable database reported")
		}
		if want := "opening the reporting snapshot:"; !strings.Contains(err.Error(), want) {
			t.Errorf("Snapshot() error = %q, want it to contain %q", err, want)
		}
	})
}

// openSnapshot opens a pool on dbURL and takes one snapshot through it. Both
// are closed when the test ends, the snapshot first: closing a pool waits for
// the connection the open transaction holds.
func openSnapshot(t *testing.T, dbURL string) *source.Snapshot {
	t.Helper()

	db, err := source.New(t.Context(), dbURL)
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	t.Cleanup(db.Close)

	snap, err := db.Snapshot(t.Context())
	if err != nil {
		t.Fatalf("Snapshot() error = %v, want nil", err)
	}
	t.Cleanup(func() {
		// The test's own context is canceled before this runs.
		if err := snap.Close(context.Background()); err != nil {
			t.Errorf("Close() error = %v, want nil", err)
		}
	})
	return snap
}

// resource names a candidate of the cloud the period is metered for.
func resource(resourceType, resourceID string) source.Resource {
	return source.Resource{
		Cloud: cloud, Platform: platform, ResourceType: resourceType, ResourceID: resourceID,
	}
}

// at is the address of an instant, which is how the nullable timestamp columns
// and an ended relation take a value.
func at(t time.Time) *time.Time {
	return &t
}

// eventIDs is the ids of a loaded history, in the order it came back.
func eventIDs(history []event.Stored) []string {
	ids := make([]string, 0, len(history))
	for _, stored := range history {
		ids = append(ids, stored.EventID)
	}
	return ids
}

// sqlState is the SQLSTATE err carries, or the empty string when it is not a
// Postgres error.
func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// wantEmpty fails unless the slice is empty and not nil. No rows is an empty
// slice here rather than a nil one, so that a caller ranging or measuring it
// meets the same value either way.
func wantEmpty[T any](t *testing.T, call string, got []T) {
	t.Helper()

	switch {
	case got == nil:
		t.Errorf("%s = nil, want an empty slice", call)
	case len(got) != 0:
		t.Errorf("%s returned %d rows, want none", call, len(got))
	}
}

// seedResource writes one projection row. createdAt and deletedAt are optional:
// a nil pointer leaves the column NULL, which is the row of a resource the
// projection never saw created or has not seen deleted. The columns metering
// does not read are filled with whatever the schema requires.
func seedResource(t *testing.T, pool *pgxpool.Pool, r source.Resource, createdAt, deletedAt *time.Time) {
	t.Helper()

	if _, err := pool.Exec(t.Context(),
		`INSERT INTO current_resources (cloud, platform, resource_type, resource_id, project_id,
		                                state, created_at, deleted_at, last_event_type, last_event_at)
		 VALUES ($1, $2, $3, $4, $5, 'active', $6, $7, 'instance.update', now())`,
		r.Cloud, r.Platform, r.ResourceType, r.ResourceID, projectID, createdAt, deletedAt); err != nil {
		t.Fatalf("seeding the projection row of %s/%s: %v", r.ResourceType, r.ResourceID, err)
	}
}

// seedEvent writes one event of r. payload is the JSON text of the payload
// column, or nil for the NULL column an event without an envelope carries.
func seedEvent(t *testing.T, pool *pgxpool.Pool, r source.Resource, eventID, eventType string,
	timestamp, receivedAt time.Time, payload any,
) {
	t.Helper()

	if _, err := pool.Exec(t.Context(),
		`INSERT INTO events (event_id, timestamp, received_at, event_type, platform, cloud,
		                     resource_type, resource_id, project_id, source, payload)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'collector', $10)`,
		eventID, timestamp, receivedAt, eventType, r.Platform, r.Cloud,
		r.ResourceType, r.ResourceID, projectID, payload); err != nil {
		t.Fatalf("seeding the event %s: %v", eventID, err)
	}
}

// seedProject writes one registry entry under the id the test holds it by.
func seedProject(t *testing.T, pool *pgxpool.Pool, p source.Project) {
	t.Helper()

	if _, err := pool.Exec(t.Context(),
		`INSERT INTO projects (id, platform, cloud, external_id) VALUES ($1, $2, $3, $4)`,
		p.ID, p.Platform, p.Cloud, p.ExternalID); err != nil {
		t.Fatalf("seeding the project %s: %v", p.ExternalID, err)
	}
}

// seedRelation writes one edge of the project graph.
func seedRelation(t *testing.T, pool *pgxpool.Pool, r source.Relation) {
	t.Helper()

	if _, err := pool.Exec(t.Context(),
		`INSERT INTO project_relations (id, source_id, target_id, relation_type, valid_from, valid_to)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		r.ID, r.SourceID, r.TargetID, r.RelationType, r.ValidFrom, r.ValidTo); err != nil {
		t.Fatalf("seeding the relation %s: %v", r.ID, err)
	}
}
