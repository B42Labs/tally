package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/b42labs/tally/internal/reporting/auth"
	"github.com/b42labs/tally/internal/reporting/httpapi/problem"
	"github.com/b42labs/tally/internal/reporting/ingest"
	"github.com/b42labs/tally/internal/reporting/reconciliation"
	"github.com/b42labs/tally/internal/reporting/registry"
	"github.com/b42labs/tally/internal/reporting/store"
	"github.com/b42labs/tally/internal/reporting/store/sqlcgen"
	"github.com/b42labs/tally/internal/reporting/store/storetest"
)

// syncAdapterName is the key the configured cloud of these tests reaches its
// adapter under.
const syncAdapterName = "fake"

// syncResourceType is the type the fake enumerates. It is one of the seeded
// OpenStack types, so a correction is measured against the size schema an
// operator would meet.
const syncResourceType = "volume"

// errPlatformDown stands in for whatever a platform client returns when it
// cannot be reached at all. It is not an EnumerationError, so it ends the run
// rather than costing it one resource type.
var errPlatformDown = errors.New("connection refused")

// TestSyncCloudOverHTTP drives POST /internal/sync/{cloud} through the whole
// stack: the contract validator, the internal-token guard, and the
// reconciliation framework behind them. The subtests share one database, so
// each of them works on a cloud of its own: a sync reads the projection, the
// run history, and the advisory lock all by cloud.
func TestSyncCloudOverHTTP(t *testing.T) {
	db := storetest.NewDB(t)

	t.Run("answers a run that corrected the projection with its stats", func(t *testing.T) {
		const cloud = "os-http-sync-drift"
		// The projection holds nothing for this cloud, so the observed resource
		// is drift the run has to report as a creation.
		fake := &syncFake{observed: []reconciliation.ObservedResource{{
			ResourceType: syncResourceType,
			ResourceID:   "vol-drifted",
			ProjectID:    fixtureProject,
			State:        "available",
			Size:         map[string]any{"size_gb": 10, "type": "ssd"},
		}}}
		a := newSyncAPI(t, db.Store, syncerOver(db.Store, cloud, fake))
		before := auditCount(t, a)

		rec := a.call(t, http.MethodPost, syncRoute(cloud), internalToken, nil)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusOK, rec.Body)
		}
		// A sync leaves no audit row by design: sync_runs is the operational
		// record of a run, and the same run kept in two places would only let the
		// two drift apart. The rebuild next door does audit, so the absence here
		// has to be asserted rather than assumed.
		if after := auditCount(t, a); after != before {
			t.Errorf("audit rows = %d, want them unchanged at %d: sync_runs is the record of a sync",
				after, before)
		}
		var got SyncResult
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decoding the body %q: %v", rec.Body.String(), err)
		}
		if got.SyncRunId == "" {
			t.Error("sync_run_id is empty, want the id of the run's row")
		}
		want := SyncStats{Created: 1}
		if !reflect.DeepEqual(got.Stats, want) {
			t.Errorf("stats = %+v, want %+v", got.Stats, want)
		}

		// The answer and the row are one record: what an operator reads back in
		// sync_runs is what the caller was told.
		row := runRow(t, a, got.SyncRunId)
		if row.Status != "completed" {
			t.Errorf("run status = %q, want %q", row.Status, "completed")
		}
		if !row.CompletedAt.Valid {
			t.Error("completed at = NULL, want the instant the run ended")
		}
		var stored SyncStats
		if err := json.Unmarshal(row.Stats, &stored); err != nil {
			t.Fatalf("decoding the stored stats %q: %v", row.Stats, err)
		}
		if !reflect.DeepEqual(stored, got.Stats) {
			t.Errorf("stored stats = %+v, want the ones the caller got, %+v", stored, got.Stats)
		}
	})

	t.Run("takes a body that names no instant the way it takes no body at all", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			cloud string
			body  []byte
		}{
			{name: "an empty body", cloud: "os-http-sync-empty-body", body: []byte{}},
			{name: "a body with no members", cloud: "os-http-sync-empty-object", body: []byte(`{}`)},
		} {
			t.Run(tc.name, func(t *testing.T) {
				fake := &syncFake{observed: []reconciliation.ObservedResource{{
					ResourceType: syncResourceType,
					ResourceID:   "vol-untold",
					ProjectID:    fixtureProject,
					State:        "available",
					Size:         map[string]any{"size_gb": 10, "type": "ssd"},
				}}}
				a := newSyncAPI(t, db.Store, syncerOver(db.Store, tc.cloud, fake))

				before := time.Now()
				rec := a.call(t, http.MethodPost, syncRoute(tc.cloud), internalToken, tc.body)

				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusOK, rec.Body)
				}
				var got SyncResult
				if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
					t.Fatalf("decoding the body %q: %v", rec.Body.String(), err)
				}
				if want := (SyncStats{Created: 1}); !reflect.DeepEqual(got.Stats, want) {
					t.Errorf("stats = %+v, want %+v", got.Stats, want)
				}
				// The run read the clock, the way one that carries no body does:
				// a body without the member names no instant to run at.
				if fake.at.Before(before) || fake.at.After(time.Now()) {
					t.Errorf("the instant handed to the adapter = %s, want one the request was served in",
						fake.at.UTC())
				}
			})
		}
	})

	t.Run("runs at the instant the request names when the deployment allows one", func(t *testing.T) {
		const cloud = "os-http-sync-told"
		told := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
		fake := &syncFake{observed: []reconciliation.ObservedResource{{
			ResourceType: syncResourceType,
			ResourceID:   "vol-told",
			ProjectID:    fixtureProject,
			State:        "available",
			Size:         map[string]any{"size_gb": 10, "type": "ssd"},
		}}}
		a := newSyncAPIWithAt(t, db.Store, syncerOver(db.Store, cloud, fake), true)

		rec := a.call(t, http.MethodPost, syncRoute(cloud), internalToken,
			[]byte(`{"at":"`+told.Format(time.RFC3339)+`"}`))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusOK, rec.Body)
		}
		var got SyncResult
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decoding the body %q: %v", rec.Body.String(), err)
		}
		if want := (SyncStats{Created: 1}); !reflect.DeepEqual(got.Stats, want) {
			t.Errorf("stats = %+v, want %+v", got.Stats, want)
		}
		// The adapter, the run's row, and the corrections all work from the told
		// instant, so the record of the run says one thing about when it was.
		if !fake.at.Equal(told) {
			t.Errorf("the instant handed to the adapter = %s, want the told %s",
				fake.at.UTC(), told)
		}
		if started := runRow(t, a, got.SyncRunId).StartedAt.Time; !started.Equal(told) {
			t.Errorf("started at = %s, want the told %s", started.UTC(), told)
		}
		instants := correctionInstants(t, a, cloud)
		if len(instants) != 1 {
			t.Fatalf("corrections = %d, want the one the run booked", len(instants))
		}
		if !instants[0].Equal(told) {
			t.Errorf("the correction is dated %s, want the told %s", instants[0].UTC(), told)
		}
	})

	t.Run("refuses an instant the deployment does not take", func(t *testing.T) {
		const cloud = "os-http-sync-told-refused"
		a := newSyncAPI(t, db.Store, syncerOver(db.Store, cloud, &syncFake{}))

		rec := a.call(t, http.MethodPost, syncRoute(cloud), internalToken,
			[]byte(`{"at":"2026-06-01T12:00:00Z"}`))

		assertProblem(t, rec, http.StatusBadRequest, problem.TypeValidation)
		assertDetail(t, rec,
			"this deployment does not take a sync instant; TALLY_REPORTING_SYNC_ALLOW_AT is off")
		// The gate is read before the run takes the lock, so a refused request
		// leaves nothing behind for an operator to read a run out of.
		if got := runsOf(t, a, cloud); got != 0 {
			t.Errorf("sync_runs rows = %d, want none: the refusal comes before the run", got)
		}
	})

	t.Run("refuses a body it cannot read as a request", func(t *testing.T) {
		t.Run("a body that is not JSON", func(t *testing.T) {
			const cloud = "os-http-sync-body-broken"
			a := newSyncAPI(t, db.Store, syncerOver(db.Store, cloud, &syncFake{}))

			rec := a.call(t, http.MethodPost, syncRoute(cloud), internalToken, []byte("not json"))

			// The contract validator reads the body before the handler does, so
			// which of the two refuses this is not pinned here. Both answer the
			// same problem.
			assertProblem(t, rec, http.StatusBadRequest, problem.TypeValidation)
			if got := runsOf(t, a, cloud); got != 0 {
				t.Errorf("sync_runs rows = %d, want none", got)
			}
		})

		// The gate is on in both subtests below, so what refuses them is the
		// instant itself rather than the deployment declining to be told one.
		t.Run("an instant that is not RFC 3339", func(t *testing.T) {
			const cloud = "os-http-sync-at-broken"
			a := newSyncAPIWithAt(t, db.Store, syncerOver(db.Store, cloud, &syncFake{}), true)

			rec := a.call(t, http.MethodPost, syncRoute(cloud), internalToken,
				[]byte(`{"at":"yesterday"}`))

			// The contract's date-time format is what refuses this, so the answer
			// is the validator's rather than the handler's.
			assertProblem(t, rec, http.StatusBadRequest, problem.TypeValidation)
			if got := runsOf(t, a, cloud); got != 0 {
				t.Errorf("sync_runs rows = %d, want none", got)
			}
		})

		// The contract's date-time format is a pattern rather than a calendar: it
		// takes any day from 01 to 31 in any month, so an instant that never
		// happened passes the validator and the handler is what refuses it.
		t.Run("an instant no calendar has", func(t *testing.T) {
			const cloud = "os-http-sync-at-impossible"
			a := newSyncAPIWithAt(t, db.Store, syncerOver(db.Store, cloud, &syncFake{}), true)

			rec := a.call(t, http.MethodPost, syncRoute(cloud), internalToken,
				[]byte(`{"at":"2026-02-31T00:00:00Z"}`))

			assertProblem(t, rec, http.StatusBadRequest, problem.TypeValidation)
			assertDetail(t, rec,
				`the request body must be a JSON object whose optional member "at" is an RFC 3339 instant`)
			if got := runsOf(t, a, cloud); got != 0 {
				t.Errorf("sync_runs rows = %d, want none", got)
			}
		})
	})

	t.Run("refuses a request that does not carry the internal token", func(t *testing.T) {
		a := newAPI(t, db.Store)

		for name, token := range map[string]string{
			"no token":      "",
			"a wrong token": internalToken + "-not",
		} {
			t.Run(name, func(t *testing.T) {
				rec := a.call(t, http.MethodPost, syncRoute("os-http-sync-guarded"), token, nil)

				assertChallenged(t, rec)
			})
		}
	})

	t.Run("answers a cloud the configuration does not name with 404", func(t *testing.T) {
		t.Run("a configuration that names another cloud", func(t *testing.T) {
			a := newSyncAPI(t, db.Store,
				syncerOver(db.Store, "os-http-sync-configured", &syncFake{}))

			rec := a.call(t, http.MethodPost, syncRoute("os-http-sync-misspelled"), internalToken, nil)

			assertProblem(t, rec, http.StatusNotFound, problem.TypeNotFound)
		})

		// A deployment that only receives collector events configures no cloud at
		// all, which is the harness every other route test runs on.
		t.Run("a configuration that names no cloud", func(t *testing.T) {
			a := newAPI(t, db.Store)

			rec := a.call(t, http.MethodPost, syncRoute("os-http-sync-anything"), internalToken, nil)

			assertProblem(t, rec, http.StatusNotFound, problem.TypeNotFound)
		})
	})

	t.Run("refuses a second sync of a cloud a run is holding", func(t *testing.T) {
		const cloud = "os-http-sync-lock"
		fake := blockingSyncFake()
		a := newSyncAPI(t, db.Store, syncerOver(db.Store, cloud, fake))

		held := make(chan *httptest.ResponseRecorder, 1)
		go func() { held <- a.call(t, http.MethodPost, syncRoute(cloud), internalToken, nil) }()
		// The fake signals from inside the listing, which the run reaches only
		// after it has taken the lock and written its row.
		<-fake.entered
		defer func() {
			close(fake.released)
			if rec := <-held; rec.Code != http.StatusOK {
				t.Errorf("the holding request's status = %d, want %d (body %q)",
					rec.Code, http.StatusOK, rec.Body)
			}
		}()

		rec := a.call(t, http.MethodPost, syncRoute(cloud), internalToken, nil)

		assertProblem(t, rec, http.StatusConflict, problem.TypeConflict)
		// The refused request steps aside before anything is recorded, so the run
		// it competed with is the only one in the history of this cloud.
		if got := runsOf(t, a, cloud); got != 1 {
			t.Errorf("sync_runs rows = %d, want only the holding run's, 1", got)
		}
	})

	t.Run("answers a run that failed with 500", func(t *testing.T) {
		const cloud = "os-http-sync-failed"
		fake := &syncFake{err: errPlatformDown}
		a := newSyncAPI(t, db.Store, syncerOver(db.Store, cloud, fake))

		rec := a.call(t, http.MethodPost, syncRoute(cloud), internalToken, nil)

		assertProblem(t, rec, http.StatusInternalServerError, problem.TypeInternal)
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decoding the body %q: %v", rec.Body.String(), err)
		}
		if got, want := body["detail"], "the sync run failed"; got != want {
			t.Errorf("body detail = %v, want %v", got, want)
		}
		// The caller is told nothing about the platform, so the run's row is
		// where the reason for the failure is kept.
		row := runRow(t, a, latestRunID(t, a, cloud))
		if row.Status != "failed" {
			t.Errorf("run status = %q, want %q", row.Status, "failed")
		}
		// The row carries more than the answer's stats do, so it is read back as
		// the record it is rather than as the document a 200 would have carried.
		var stored reconciliation.Stats
		if err := json.Unmarshal(row.Stats, &stored); err != nil {
			t.Fatalf("decoding the stored stats %q: %v", row.Stats, err)
		}
		if !strings.Contains(strings.Join(stored.Errors, "; "), errPlatformDown.Error()) {
			t.Errorf("stored errors = %v, want them to carry %q", stored.Errors, errPlatformDown)
		}
	})
}

// syncRoute is the route of one cloud, which the other Tally components call
// with the shared internal token.
func syncRoute(cloud string) string {
	return "/internal/sync/" + cloud
}

// syncFake is the platform half of these tests: what one listing observes,
// whether it fails outright, and whether it stops halfway so that a second
// request can compete with a run that is in flight.
type syncFake struct {
	observed []reconciliation.ObservedResource
	err      error

	// at is the instant the last listing was told the run is at.
	at time.Time

	// entered is closed when a blocking listing starts and released is what
	// holds it there. Both are nil for a fake that does not block.
	entered  chan struct{}
	released chan struct{}
}

// blockingSyncFake is a fake whose listing stops before it observes anything,
// until the test releases it or the request's budget runs out.
func blockingSyncFake() *syncFake {
	return &syncFake{entered: make(chan struct{}), released: make(chan struct{})}
}

func (f *syncFake) Platform() string { return fixturePlatform }

func (f *syncFake) ResourceTypes(map[string]any) ([]string, error) {
	return []string{syncResourceType}, nil
}

func (f *syncFake) ListResources(ctx context.Context, _ map[string]any, _ *time.Time,
	at time.Time,
) iter.Seq2[reconciliation.ObservedResource, error] {
	f.at = at
	return func(yield func(reconciliation.ObservedResource, error) bool) {
		if f.released != nil {
			close(f.entered)
			select {
			case <-f.released:
			case <-ctx.Done():
				yield(reconciliation.ObservedResource{}, ctx.Err())
				return
			}
		}
		if f.err != nil {
			yield(reconciliation.ObservedResource{}, f.err)
			return
		}
		for _, obs := range f.observed {
			if !yield(obs, nil) {
				return
			}
		}
	}
}

// syncerOver builds a Syncer serving one cloud through fake. The pipeline is
// the lax one the rest of the harness uses, so a correction is stored the way a
// collector's event of the same resource would be.
func syncerOver(s *store.Store, cloud string, fake *syncFake) *reconciliation.Syncer {
	cfg := reconciliation.Config{Clouds: []reconciliation.CloudConfig{{
		Cloud: cloud, Platform: fixturePlatform, Adapter: syncAdapterName,
	}}}
	return reconciliation.New(s, ingest.New(registry.New(), false, nil, nil), cfg,
		map[string]reconciliation.Adapter{syncAdapterName: fake}, time.Now, nil)
}

// newSyncAPI builds the full router over s with authentication enforced and
// syncer behind the sync route, taking no instant from a request body, which is
// how every deployment but a development one is configured. The shared harness
// wires a Syncer over the empty configuration, which answers every cloud 404,
// so a test that needs a configured cloud builds its router here.
func newSyncAPI(t *testing.T, s *store.Store, syncer *reconciliation.Syncer) api {
	t.Helper()

	return newSyncAPIWithAt(t, s, syncer, false)
}

// newSyncAPIWithAt is newSyncAPI with the TALLY_REPORTING_SYNC_ALLOW_AT gate
// set to allowAt, which is what decides whether the sync route lets a request
// name the instant the run is at.
func newSyncAPIWithAt(t *testing.T, s *store.Store, syncer *reconciliation.Syncer, allowAt bool) api {
	t.Helper()

	q := sqlcgen.New(s.Pool())
	handler, err := NewRouter(Options{
		Logger:             slog.New(slog.NewJSONHandler(io.Discard, nil)),
		DB:                 s,
		UnhealthyThreshold: time.Minute,
		Queries:            q,
		Store:              s,
		AuthMode:           auth.ModeEnforced,
		InternalToken:      internalToken,
		Authenticator:      auth.NewStaticTokenAuthenticator(q),
		Pipeline:           ingest.New(registry.New(), false, nil, nil),
		Syncer:             syncer,
		SyncAllowAt:        allowAt,
	})
	if err != nil {
		t.Fatalf("NewRouter() error = %v, want nil", err)
	}
	return api{store: s, queries: q, handler: handler}
}

// runRow reads one sync_runs row back, which is the record an operator works
// from once the request that started the run is over.
func runRow(t *testing.T, a api, id string) sqlcgen.SyncRun {
	t.Helper()

	parsed, err := uuid.Parse(id)
	if err != nil {
		t.Fatalf("parsing the run id %q: %v", id, err)
	}
	row, err := a.queries.GetSyncRun(t.Context(), parsed)
	if err != nil {
		t.Fatalf("reading sync run %s: %v", id, err)
	}
	return row
}

// latestRunID is the id of the last run of the cloud. A failed run is answered
// with a problem rather than with its id, so this is how the row it left behind
// is found.
func latestRunID(t *testing.T, a api, cloud string) string {
	t.Helper()

	var id uuid.UUID
	if err := a.store.Pool().QueryRow(t.Context(),
		`SELECT id FROM sync_runs WHERE cloud = $1 ORDER BY started_at DESC LIMIT 1`,
		cloud).Scan(&id); err != nil {
		t.Fatalf("reading the last sync run of %s: %v", cloud, err)
	}
	return id.String()
}

// runsOf is how many runs the cloud has a row for.
func runsOf(t *testing.T, a api, cloud string) int {
	t.Helper()

	var found int
	if err := a.store.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM sync_runs WHERE cloud = $1`, cloud).Scan(&found); err != nil {
		t.Fatalf("counting the sync runs of %s: %v", cloud, err)
	}
	return found
}

// correctionInstants is when the synthetic events of cloud are dated, ordered
// by that instant. It is what says which clock the run that booked them worked
// from.
func correctionInstants(t *testing.T, a api, cloud string) []time.Time {
	t.Helper()

	rows, err := a.store.Pool().Query(t.Context(),
		`SELECT timestamp FROM events WHERE cloud = $1 AND source = 'reconciliation'
		 ORDER BY timestamp`, cloud)
	if err != nil {
		t.Fatalf("reading the corrections of %s: %v", cloud, err)
	}
	defer rows.Close()

	var found []time.Time
	for rows.Next() {
		var at time.Time
		if err := rows.Scan(&at); err != nil {
			t.Fatalf("scanning a correction: %v", err)
		}
		found = append(found, at)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the corrections of %s: %v", cloud, err)
	}
	return found
}

// assertDetail checks the sentence a problem response explains itself with,
// which is what tells two refusals of the same type apart.
func assertDetail(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the body %q: %v", rec.Body.String(), err)
	}
	if got := body["detail"]; got != want {
		t.Errorf("body detail = %v, want %q", got, want)
	}
}

// auditCount is how many audit rows exist at all, which is what a handler that
// writes none is measured against.
func auditCount(t *testing.T, a api) int {
	t.Helper()

	var found int
	if err := a.store.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM audit_log`).Scan(&found); err != nil {
		t.Fatalf("counting the audit rows: %v", err)
	}
	return found
}
