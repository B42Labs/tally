package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/b42labs/tally/internal/core/event"
	"github.com/b42labs/tally/internal/reporting/auth"
	"github.com/b42labs/tally/internal/reporting/httpapi/problem"
	"github.com/b42labs/tally/internal/reporting/ingest"
	"github.com/b42labs/tally/internal/reporting/registry"
	"github.com/b42labs/tally/internal/reporting/store"
	"github.com/b42labs/tally/internal/reporting/store/sqlcgen"
	"github.com/b42labs/tally/internal/reporting/store/storetest"
)

// The fixtures every route test builds its events from. The platform is the one
// the migration chain seeds the resource types of, so a reported size meets the
// schema an operator would meet.
const (
	fixturePlatform = "openstack"
	fixtureProject  = "project-a"
)

// internalToken is the shared secret the /internal routes of a test router are
// guarded with.
const internalToken = "internal-token-of-the-test-router"

// The instants the fixtures use. They are whole seconds, so what Postgres
// stores compares equal to what the test passed in.
var (
	createTime = time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	updateTime = createTime.Add(time.Hour)
)

// TestIngestEventsOverHTTP drives POST /api/v1/events through the whole stack:
// the contract validator, the ingest guard, and the pipeline behind them. The
// subtests share one database, so each works on a cloud and a credential of its
// own.
func TestIngestEventsOverHTTP(t *testing.T) {
	db := storetest.NewDB(t)
	a := newAPI(t, db.Store)

	t.Run("takes a single event object and stores the source the server names", func(t *testing.T) {
		const cloud = "os-http-single"
		_, token := seedIngestCredential(t, a.queries, fixturePlatform, cloud)
		res := fixture{cloud: cloud, resourceType: "volume", id: "vol-single"}
		// The body is one object rather than an array, which is what a collector
		// sends when it has one event, and it claims a source the server ignores.
		claimed := res.event("vol-single-create", "volume.create", createTime,
			payloadOf("available", volumeSize(10)))
		claimed.Source = event.SourceReconciliation

		rec := a.call(t, http.MethodPost, "/api/v1/events", token, item(t, claimed))

		got := ingestResultOf(t, rec)
		if want := (IngestResult{Accepted: 1, Rejected: []RejectedEvent{}}); !reflect.DeepEqual(got, want) {
			t.Errorf("result = %+v, want %+v", got, want)
		}
		if stored := eventSource(t, a, claimed.EventID); stored != string(event.SourceCollector) {
			t.Errorf("stored source = %q, want %q", stored, event.SourceCollector)
		}
	})

	t.Run("reports every refused item of a mixed batch by index", func(t *testing.T) {
		const cloud = "os-http-mixed"
		_, token := seedIngestCredential(t, a.queries, fixturePlatform, cloud)
		res := fixture{cloud: cloud, resourceType: "volume", id: "vol-mixed"}
		instance := fixture{cloud: cloud, resourceType: "instance", id: "inst-mixed"}

		// An item without an event id is reported by its index alone, which is why
		// the answer carries both.
		anonymous := res.event("", "volume.create", createTime, payloadOf("available", volumeSize(10)))
		// The seeded (openstack, instance) schema takes vcpus as an integer.
		oversized := instance.event("inst-mixed-create", "instance.create", createTime,
			payloadOf("active", map[string]any{
				"vcpus": "four", "ram_gb": 8, "disk_gb": 40, "flavor": "m1.large",
			}))
		body := batch(t,
			item(t, res.event("vol-mixed-create", "volume.create", createTime,
				payloadOf("available", volumeSize(10)))),
			item(t, anonymous),
			item(t, oversized),
			item(t, res.event("vol-mixed-resize", "volume.resize", updateTime,
				payloadOf("available", volumeSize(20)))),
		)

		got := ingestResultOf(t, a.call(t, http.MethodPost, "/api/v1/events", token, body))

		if got.Accepted != 2 || got.Duplicates != 0 {
			t.Errorf("result accepted %d and %d duplicates, want 2 and 0", got.Accepted, got.Duplicates)
		}
		if len(got.Rejected) != 2 {
			t.Fatalf("rejected = %+v, want the anonymous and the oversized item", got.Rejected)
		}
		assertRejected(t, got.Rejected[0], 1, "", "schema: event_id: ")
		assertRejected(t, got.Rejected[1], 2, "inst-mixed-create", "size_schema: ")

		if rows := deadLetters(t, a, cloud); rows != 2 {
			t.Errorf("dead-letter rows = %d, want one per refused item, 2", rows)
		}
	})

	t.Run("counts a replayed batch as duplicates and leaves the projection alone", func(t *testing.T) {
		const cloud = "os-http-replay"
		_, token := seedIngestCredential(t, a.queries, fixturePlatform, cloud)
		res := fixture{cloud: cloud, resourceType: "volume", id: "vol-replay"}
		other := fixture{cloud: cloud, resourceType: "volume", id: "vol-replay-second"}
		body := batch(t,
			item(t, res.event("vol-replay-create", "volume.create", createTime,
				payloadOf("available", volumeSize(10)))),
			item(t, res.event("vol-replay-resize", "volume.resize", updateTime,
				payloadOf("available", volumeSize(20)))),
			item(t, other.event("vol-replay-second-create", "volume.create", createTime,
				payloadOf("available", volumeSize(5)))),
		)

		first := ingestResultOf(t, a.call(t, http.MethodPost, "/api/v1/events", token, body))
		if want := (IngestResult{Accepted: 3, Rejected: []RejectedEvent{}}); !reflect.DeepEqual(first, want) {
			t.Fatalf("result of the first call = %+v, want %+v", first, want)
		}
		before := resourceRows(t, a, cloud)

		replayed := ingestResultOf(t, a.call(t, http.MethodPost, "/api/v1/events", token, body))

		if want := (IngestResult{Duplicates: 3, Rejected: []RejectedEvent{}}); !reflect.DeepEqual(replayed, want) {
			t.Errorf("result of the replay = %+v, want %+v", replayed, want)
		}
		if after := resourceRows(t, a, cloud); !reflect.DeepEqual(after, before) {
			t.Errorf("projection rows = %v, want them unchanged at %v", after, before)
		}
	})

	t.Run("answers 200 for a batch in which every item is refused", func(t *testing.T) {
		const cloud = "os-http-all-refused"
		_, token := seedIngestCredential(t, a.queries, fixturePlatform, cloud)
		res := fixture{cloud: cloud, resourceType: "volume", id: "vol-refused"}
		body := batch(t,
			// A create has to report the size the resource starts with.
			item(t, res.event("vol-refused-sizeless", "volume.create", createTime,
				payloadOf("available", nil))),
			item(t, res.event("vol-refused-typeless", "volume-create", createTime,
				payloadOf("available", volumeSize(10)))),
		)

		got := ingestResultOf(t, a.call(t, http.MethodPost, "/api/v1/events", token, body))

		if got.Accepted != 0 || got.Duplicates != 0 {
			t.Errorf("result accepted %d and %d duplicates, want none of either", got.Accepted, got.Duplicates)
		}
		if len(got.Rejected) != 2 {
			t.Fatalf("rejected = %+v, want both items", got.Rejected)
		}
		assertRejected(t, got.Rejected[0], 0, "vol-refused-sizeless",
			"schema: payload.size: required on create events")
		assertRejected(t, got.Rejected[1], 1, "vol-refused-typeless", "schema: event_type: ")
	})

	t.Run("audits a scope violation against the credential that reported it", func(t *testing.T) {
		const (
			cloud   = "os-http-scope"
			foreign = "os-http-scope-foreign"
		)
		credentialID, token := seedIngestCredential(t, a.queries, fixturePlatform, cloud)
		outside := fixture{cloud: foreign, resourceType: "volume", id: "vol-scope-foreign"}
		reported := outside.event("vol-scope-foreign-create", "volume.create", createTime,
			payloadOf("available", volumeSize(10)))

		got := ingestResultOf(t, a.call(t, http.MethodPost, "/api/v1/events", token, item(t, reported)))

		if got.Accepted != 0 {
			t.Errorf("result accepted %d, want the event outside the credential's cloud refused", got.Accepted)
		}
		if len(got.Rejected) != 1 {
			t.Fatalf("rejected = %+v, want the event outside the credential's cloud", got.Rejected)
		}
		assertRejected(t, got.Rejected[0], 0, reported.EventID, "scope")

		rows := auditRows(t, a, credentialID.String(), "events.scope_violation")
		if len(rows) != 1 {
			t.Fatalf("scope violation audit rows = %v, want exactly one", rows)
		}
		if rows[0].objectType != "events" || rows[0].objectID != reported.EventID {
			t.Errorf("audit row names %s %q, want events %q",
				rows[0].objectType, rows[0].objectID, reported.EventID)
		}
		want := map[string]any{
			"credential_platform": fixturePlatform,
			"credential_cloud":    cloud,
			"event_platform":      reported.Platform,
			"event_cloud":         foreign,
		}
		if !reflect.DeepEqual(rows[0].details, want) {
			t.Errorf("audit details = %v, want %v", rows[0].details, want)
		}

		// Decision D5: the event body of a scope violation is not kept.
		if dead := deadLetters(t, a, foreign); dead != 0 {
			t.Errorf("dead-letter rows = %d, want none for a scope violation", dead)
		}
	})

	t.Run("records one ingest audit row per batch", func(t *testing.T) {
		const cloud = "os-http-audit"
		credentialID, token := seedIngestCredential(t, a.queries, fixturePlatform, cloud)
		res := fixture{cloud: cloud, resourceType: "volume", id: "vol-audit"}
		create := item(t, res.event("vol-audit-create", "volume.create", createTime,
			payloadOf("available", volumeSize(10))))
		// The same item twice: the second is the duplicate the answer counts.
		body := batch(t, create, create,
			item(t, res.event("vol-audit-broken", "volume-create", updateTime,
				payloadOf("available", volumeSize(20)))))

		got := ingestResultOf(t, a.call(t, http.MethodPost, "/api/v1/events", token, body))

		if got.Accepted != 1 || got.Duplicates != 1 || len(got.Rejected) != 1 {
			t.Fatalf("result = %+v, want one accepted, one duplicate, and one refused item", got)
		}

		rows := auditRows(t, a, credentialID.String(), "events.ingest")
		if len(rows) != 1 {
			t.Fatalf("ingest audit rows = %v, want exactly one per batch", rows)
		}
		if rows[0].objectType != "events" {
			t.Errorf("audit object type = %q, want %q", rows[0].objectType, "events")
		}
		want := map[string]any{"accepted": float64(1), "duplicates": float64(1), "rejected": float64(1)}
		if !reflect.DeepEqual(rows[0].details, want) {
			t.Errorf("audit details = %v, want %v", rows[0].details, want)
		}
	})

	t.Run("refuses a batch of more than 1000 items and stores none of it", func(t *testing.T) {
		const cloud = "os-http-oversized"
		credentialID, token := seedIngestCredential(t, a.queries, fixturePlatform, cloud)
		res := fixture{cloud: cloud, resourceType: "volume", id: "vol-oversized"}
		items := make([]json.RawMessage, maxBatchItems+1)
		for i := range items {
			items[i] = item(t, res.event(fmt.Sprintf("vol-oversized-%d", i), "volume.create",
				createTime.Add(time.Duration(i)*time.Second), payloadOf("available", volumeSize(10))))
		}

		rec := a.call(t, http.MethodPost, "/api/v1/events", token, batch(t, items...))

		assertProblem(t, rec, http.StatusRequestEntityTooLarge, problem.TypePayloadTooLarge)
		if stored := storedEvents(t, a, cloud); stored != 0 {
			t.Errorf("stored events = %d, want a refused batch to store none", stored)
		}
		if rows := auditRows(t, a, credentialID.String(), "events.ingest"); len(rows) != 0 {
			t.Errorf("ingest audit rows = %v, want none for a batch that was never read", rows)
		}
	})

	t.Run("takes a batch of exactly 1000 items", func(t *testing.T) {
		const cloud = "os-http-full-batch"
		_, token := seedIngestCredential(t, a.queries, fixturePlatform, cloud)
		res := fixture{cloud: cloud, resourceType: "volume", id: "vol-full-batch"}
		// The batch the contract promises is accepted, one below the size the
		// subtest above is refused at: the cap is a ceiling, not a limit the
		// collector has to stay under.
		items := make([]json.RawMessage, maxBatchItems)
		for i := range items {
			items[i] = item(t, res.event(fmt.Sprintf("vol-full-batch-%d", i), "volume.create",
				createTime.Add(time.Duration(i)*time.Second), payloadOf("available", volumeSize(10))))
		}

		got := ingestResultOf(t, a.call(t, http.MethodPost, "/api/v1/events", token, batch(t, items...)))

		if got.Accepted != maxBatchItems {
			t.Errorf("result = %+v, want all %d items accepted", got, maxBatchItems)
		}
	})

	t.Run("touches nothing for an empty batch", func(t *testing.T) {
		const cloud = "os-http-empty"
		_, token := seedIngestCredential(t, a.queries, fixturePlatform, cloud)
		before := tableCounts(t, a)

		got := ingestResultOf(t, a.call(t, http.MethodPost, "/api/v1/events", token, []byte(`[]`)))

		if want := (IngestResult{Rejected: []RejectedEvent{}}); !reflect.DeepEqual(got, want) {
			t.Errorf("result = %+v, want %+v", got, want)
		}
		if after := tableCounts(t, a); !reflect.DeepEqual(after, before) {
			t.Errorf("row counts = %v, want them unchanged at %v", after, before)
		}
	})

	t.Run("refuses a credential the ingest route does not accept", func(t *testing.T) {
		const cloud = "os-http-unauthorized"
		revokedID, revoked := seedIngestCredential(t, a.queries, fixturePlatform, cloud)
		if err := a.queries.RevokeIngestCredential(t.Context(), revokedID); err != nil {
			t.Fatalf("RevokeIngestCredential() error = %v, want nil", err)
		}
		unknown, err := auth.GenerateIngestToken()
		if err != nil {
			t.Fatalf("generating the ingest token: %v", err)
		}
		// The two token tables do not cross: an api_tokens row is a query
		// credential and buys nothing on the ingest route.
		_, queryToken := seedAPIToken(t, a.queries, auth.RoleAdmin, nil)

		res := fixture{cloud: cloud, resourceType: "volume", id: "vol-unauthorized"}
		body := item(t, res.event("vol-unauthorized-create", "volume.create", createTime,
			payloadOf("available", volumeSize(10))))

		for name, token := range map[string]string{
			"no token":               "",
			"an unknown token":       unknown,
			"a revoked ingest token": revoked,
			"an api token":           queryToken,
		} {
			t.Run(name, func(t *testing.T) {
				rec := a.call(t, http.MethodPost, "/api/v1/events", token, body)

				assertChallenged(t, rec)
			})
		}

		if stored := storedEvents(t, a, cloud); stored != 0 {
			t.Errorf("stored events = %d, want a refused request to store none", stored)
		}
	})
}

// TestIngestEventsSurvivesADatabaseOutage drives the failure a collector has to
// retry: the database is gone when a batch arrives, the call fails, and the
// collector sends the same batch again once the database is back. Nothing may
// be lost and nothing may be stored twice, which is what the idempotency key is
// for. Only a database that really disappears proves it.
func TestIngestEventsSurvivesADatabaseOutage(t *testing.T) {
	db := storetest.NewDB(t)
	ctx := t.Context()

	const cloud = "os-http-outage"
	_, token := seedIngestCredential(t, sqlcgen.New(db.Store.Pool()), fixturePlatform, cloud)
	res := fixture{cloud: cloud, resourceType: "volume", id: "vol-outage"}
	body := batch(t,
		item(t, res.event("vol-outage-create", "volume.create", createTime,
			payloadOf("available", volumeSize(10)))),
		item(t, res.event("vol-outage-resize", "volume.resize", updateTime,
			payloadOf("available", volumeSize(20)))),
	)

	// How long the database gets to shut down cleanly.
	stopTimeout := 10 * time.Second
	if err := db.Container.Stop(ctx, &stopTimeout); err != nil {
		t.Fatalf("stopping the database container: %v", err)
	}

	// The credential lookup is the first statement the request runs, so this is
	// where a database that is gone stops it. What the call proves is the answer
	// and the retry, not which of the two transactions failed.
	rec := newAPI(t, db.Store).call(t, http.MethodPost, "/api/v1/events", token, body)

	assertProblem(t, rec, http.StatusInternalServerError, problem.TypeInternal)

	if err := db.Container.Start(ctx); err != nil {
		t.Fatalf("starting the database container again: %v", err)
	}
	// The container comes back on a different host port, so everything bound to
	// the old one is rebuilt from the endpoint it has now.
	a := newAPI(t, reopen(t, db))
	waitForReady(t, a.handler, time.Minute)

	if stored := storedEvents(t, a, cloud); stored != 0 {
		t.Fatalf("stored events after the outage = %d, want the failed batch to have stored none", stored)
	}

	got := ingestResultOf(t, a.call(t, http.MethodPost, "/api/v1/events", token, body))

	if want := (IngestResult{Accepted: 2, Rejected: []RejectedEvent{}}); !reflect.DeepEqual(got, want) {
		t.Errorf("result of the retry = %+v, want %+v", got, want)
	}
	for _, eventID := range []string{"vol-outage-create", "vol-outage-resize"} {
		if copies := eventCopies(t, a, eventID); copies != 1 {
			t.Errorf("stored copies of %s = %d, want exactly 1", eventID, copies)
		}
	}
}

// TestIngestEventsWithAuthenticationDisabled drives the mode a development
// setup runs in. Nothing checks a credential, so no principal reaches the
// handler, and what the handler does without one is what this pins: the batch
// is recorded as the service's own work and no cloud is out of scope.
func TestIngestEventsWithAuthenticationDisabled(t *testing.T) {
	db := storetest.NewDB(t)
	a := newAPIInMode(t, db.Store, auth.ModeDisabled)

	// One credential exists, for a cloud neither item names. Without a principal
	// there is no scope to check an item against, so both are stored anyway.
	seedIngestCredential(t, a.queries, fixturePlatform, "os-http-disabled-elsewhere")
	first := fixture{cloud: "os-http-disabled-a", resourceType: "volume", id: "vol-disabled-a"}
	second := fixture{cloud: "os-http-disabled-b", resourceType: "volume", id: "vol-disabled-b"}
	body := batch(t,
		item(t, first.event("vol-disabled-a-create", "volume.create", createTime,
			payloadOf("available", volumeSize(10)))),
		item(t, second.event("vol-disabled-b-create", "volume.create", createTime,
			payloadOf("available", volumeSize(20)))),
	)

	// No Authorization header at all, which is what every request looks like
	// while authentication is disabled.
	got := ingestResultOf(t, a.call(t, http.MethodPost, "/api/v1/events", "", body))

	if want := (IngestResult{Accepted: 2, Rejected: []RejectedEvent{}}); !reflect.DeepEqual(got, want) {
		t.Fatalf("result = %+v, want %+v", got, want)
	}
	for _, cloud := range []string{first.cloud, second.cloud} {
		if stored := storedEvents(t, a, cloud); stored != 1 {
			t.Errorf("stored events of %s = %d, want the item to have been stored", cloud, stored)
		}
	}
	if rows := auditRows(t, a, "internal", "events.scope_violation"); len(rows) != 0 {
		t.Errorf("scope violation audit rows = %v, want none for a batch without a scope", rows)
	}

	// The actor is the literal the log carries for what the service does on its
	// own, which is the whole record a request without a credential leaves.
	rows := auditRows(t, a, "internal", "events.ingest")
	if len(rows) != 1 {
		t.Fatalf("ingest audit rows of the service itself = %v, want exactly one", rows)
	}
	want := map[string]any{"accepted": float64(2), "duplicates": float64(0), "rejected": float64(0)}
	if !reflect.DeepEqual(rows[0].details, want) {
		t.Errorf("audit details = %v, want %v", rows[0].details, want)
	}
}

// TestIngestEventsReportsAFailedBatch drives the handler's own failure path.
// With authentication disabled nothing reads the database before the batch
// does, so a database that is gone stops the request inside the transaction
// carrying the batch and the audit row, rather than at the credential lookup in
// front of it.
func TestIngestEventsReportsAFailedBatch(t *testing.T) {
	db := storetest.NewDB(t)
	a := newAPIInMode(t, db.Store, auth.ModeDisabled)

	res := fixture{cloud: "os-http-disabled-outage", resourceType: "volume", id: "vol-disabled-outage"}
	body := batch(t,
		item(t, res.event("vol-disabled-outage-create", "volume.create", createTime,
			payloadOf("available", volumeSize(10)))),
		item(t, res.event("vol-disabled-outage-resize", "volume.resize", updateTime,
			payloadOf("available", volumeSize(20)))),
	)

	// How long the database gets to shut down cleanly.
	stopTimeout := 10 * time.Second
	if err := db.Container.Stop(t.Context(), &stopTimeout); err != nil {
		t.Fatalf("stopping the database container: %v", err)
	}

	rec := a.call(t, http.MethodPost, "/api/v1/events", "", body)

	assertProblem(t, rec, http.StatusInternalServerError, problem.TypeInternal)
	// The detail is what tells the two internal problems of this route apart:
	// the batch failed, rather than a credential that could not be checked.
	if got, want := problemDetail(t, rec), "the batch could not be stored"; got != want {
		t.Errorf("detail = %q, want %q", got, want)
	}

	if err := db.Container.Start(t.Context()); err != nil {
		t.Fatalf("starting the database container again: %v", err)
	}
	// The container comes back on a different host port, so everything bound to
	// the old one is rebuilt from the endpoint it has now.
	back := newAPIInMode(t, reopen(t, db), auth.ModeDisabled)
	waitForReady(t, back.handler, time.Minute)

	// The failed call is everything this database ever saw, so every table the
	// batch would have written is still empty.
	for table, rows := range tableCounts(t, back) {
		if rows != 0 {
			t.Errorf("%s holds %d rows, want a failed batch to have written none", table, rows)
		}
	}
}

// api is the router one test drives, together with the database behind it.
type api struct {
	store   *store.Store
	queries *sqlcgen.Queries
	handler http.Handler
}

// newAPI builds the full router over s with authentication enforced, which is
// what most route tests need: every request has to carry the credential its
// route demands.
func newAPI(t *testing.T, s *store.Store) api {
	t.Helper()

	return newAPIInMode(t, s, auth.ModeEnforced)
}

// newAPIInMode builds the full router over s in the given authentication mode.
// The credential seams are wired either way, so the mode is the only thing that
// decides whether a request needs one. The pipeline is the lax one, so an
// unregistered resource type is accepted rather than refused.
func newAPIInMode(t *testing.T, s *store.Store, mode auth.Mode) api {
	t.Helper()

	q := sqlcgen.New(s.Pool())
	handler, err := NewRouter(Options{
		Logger:             slog.New(slog.NewJSONHandler(io.Discard, nil)),
		DB:                 s,
		UnhealthyThreshold: time.Minute,
		Queries:            q,
		Store:              s,
		AuthMode:           mode,
		InternalToken:      internalToken,
		Authenticator:      auth.NewStaticTokenAuthenticator(q),
		Pipeline:           ingest.New(registry.New(), false, nil),
	})
	if err != nil {
		t.Fatalf("NewRouter() error = %v, want nil", err)
	}
	return api{store: s, queries: q, handler: handler}
}

// call runs one request through the router: body as the JSON payload when there
// is one, and token as the bearer credential when it is not empty.
func (a api) call(t *testing.T, method, target, token string, body []byte) *httptest.ResponseRecorder {
	t.Helper()

	var payload io.Reader
	if body != nil {
		payload = bytes.NewReader(body)
	}
	r := httptest.NewRequest(method, target, payload)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return serve(a.handler, r)
}

// reopen opens a pool on the address the container is published at right now. A
// restarted container comes back on a different host port, so a pool built from
// the original URL would never reconnect.
func reopen(t *testing.T, db storetest.DB) *store.Store {
	t.Helper()

	// The credentials stay the same across a restart; only the host port moves.
	base, err := url.Parse(db.URL)
	if err != nil {
		t.Fatalf("parsing the database URL %q: %v", db.URL, err)
	}
	host, err := db.Container.Host(t.Context())
	if err != nil {
		t.Fatalf("reading the container host: %v", err)
	}
	port, err := db.Container.MappedPort(t.Context(), storetest.ContainerPort)
	if err != nil {
		t.Fatalf("reading the mapped database port: %v", err)
	}
	current := *base
	current.Host = net.JoinHostPort(host, port.Port())

	s, err := store.New(t.Context(), current.String(), 4)
	if err != nil {
		t.Fatalf("store.New() error = %v, want nil", err)
	}
	t.Cleanup(s.Close)
	return s
}

// seedIngestCredential issues an ingest token, stores its hash, and returns the
// credential's id together with the token itself.
func seedIngestCredential(t *testing.T, q *sqlcgen.Queries, platform, cloud string) (uuid.UUID, string) {
	t.Helper()

	token, err := auth.GenerateIngestToken()
	if err != nil {
		t.Fatalf("generating the ingest token: %v", err)
	}
	id, err := q.CreateIngestCredential(t.Context(), sqlcgen.CreateIngestCredentialParams{
		Platform:    platform,
		Cloud:       cloud,
		TokenHash:   auth.HashToken(token),
		Description: pgtype.Text{String: "httpapi integration test", Valid: true},
	})
	if err != nil {
		t.Fatalf("CreateIngestCredential() error = %v, want nil", err)
	}
	return id, token
}

// fixture is the resource a subtest works on: one resource of one cloud, which
// every event it builds names.
type fixture struct {
	cloud        string
	resourceType string
	id           string
}

// event builds one event of the resource, as a collector would send it.
func (f fixture) event(eventID, eventType string, ts time.Time, payload event.PayloadEnvelope) event.Event {
	return event.Event{
		EventID:      eventID,
		Timestamp:    ts,
		EventType:    eventType,
		Platform:     fixturePlatform,
		Cloud:        f.cloud,
		ResourceType: f.resourceType,
		ResourceID:   f.id,
		ProjectID:    fixtureProject,
		Source:       event.SourceCollector,
		Payload:      payload,
	}
}

// item renders an event as the batch item a collector submits.
func item(t *testing.T, e event.Event) []byte {
	t.Helper()

	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshaling event %s: %v", e.EventID, err)
	}
	return raw
}

// batch wraps rendered items into the array form of the request body.
func batch(t *testing.T, items ...json.RawMessage) []byte {
	t.Helper()

	raw, err := json.Marshal(items)
	if err != nil {
		t.Fatalf("marshaling the batch: %v", err)
	}
	return raw
}

// payloadOf is a payload reporting a state and, when size is not nil, a new
// size.
func payloadOf(state string, size map[string]any) event.PayloadEnvelope {
	return event.PayloadEnvelope{State: &state, Size: size}
}

// volumeSize is a size the seeded (openstack, volume) schema accepts.
func volumeSize(gb float64) map[string]any {
	return map[string]any{"size_gb": gb, "type": "ssd"}
}

// ingestResultOf decodes the answer of an ingest call, which the contract
// promises is a 200 carrying JSON. The rejected member has to come back as an
// array even when nothing was rejected, so that a client iterates it without
// checking for null first.
func ingestResultOf(t *testing.T, rec *httptest.ResponseRecorder) IngestResult {
	t.Helper()

	if got := rec.Code; got != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %q)", got, http.StatusOK, rec.Body)
	}
	if got, want := rec.Header().Get("Content-Type"), "application/json"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}

	var result IngestResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decoding the body %q: %v", rec.Body.String(), err)
	}
	if result.Rejected == nil {
		t.Errorf("body %q carries rejected as null, want an array", rec.Body)
	}
	return result
}

// assertRejected fails the test unless got names the item at index and refuses
// it for a reason starting with reasonPrefix. The reasons the server writes
// itself are given in full; the ones carrying a validator's own message are not
// this test's to spell out.
func assertRejected(t *testing.T, got RejectedEvent, index int, eventID, reasonPrefix string) {
	t.Helper()

	if got.Index != index || got.EventId != eventID {
		t.Errorf("rejected item = %+v, want index %d and event id %q", got, index, eventID)
	}
	if !strings.HasPrefix(got.Reason, reasonPrefix) {
		t.Errorf("rejected reason = %q, want it prefixed %q", got.Reason, reasonPrefix)
	}
}

// assertChallenged checks the answer to a request whose credential this API
// does not accept: the 401 problem and the challenge naming the scheme the
// caller should have used.
func assertChallenged(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()

	assertProblem(t, rec, http.StatusUnauthorized, problem.TypeUnauthorized)
	if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
		t.Errorf("WWW-Authenticate = %q, want %q", got, "Bearer")
	}
}

// auditRow is an audit_log row as the assertions read it back.
type auditRow struct {
	objectType string
	objectID   string
	details    map[string]any
}

// auditRows returns every audit_log row actor left behind under action, oldest
// first.
func auditRows(t *testing.T, a api, actor, action string) []auditRow {
	t.Helper()

	rows, err := a.store.Pool().Query(t.Context(),
		`SELECT coalesce(object_type, ''), coalesce(object_id, ''), details
		 FROM audit_log WHERE actor = $1 AND action = $2 ORDER BY id`,
		actor, action)
	if err != nil {
		t.Fatalf("reading the %s audit rows of %s: %v", action, actor, err)
	}
	defer rows.Close()

	var found []auditRow
	for rows.Next() {
		var row auditRow
		if err := rows.Scan(&row.objectType, &row.objectID, &row.details); err != nil {
			t.Fatalf("scanning an audit row: %v", err)
		}
		found = append(found, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the %s audit rows of %s: %v", action, actor, err)
	}
	return found
}

// eventSource is the source column of the stored event.
func eventSource(t *testing.T, a api, eventID string) string {
	t.Helper()

	var source string
	if err := a.store.Pool().QueryRow(t.Context(),
		`SELECT source FROM events WHERE event_id = $1`, eventID).Scan(&source); err != nil {
		t.Fatalf("reading the stored event %s: %v", eventID, err)
	}
	return source
}

// eventCopies is how many rows the events table holds for one event id.
func eventCopies(t *testing.T, a api, eventID string) int {
	t.Helper()

	var found int
	if err := a.store.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM events WHERE event_id = $1`, eventID).Scan(&found); err != nil {
		t.Fatalf("counting the copies of event %s: %v", eventID, err)
	}
	return found
}

// storedEvents is how many events the cloud holds.
func storedEvents(t *testing.T, a api, cloud string) int {
	t.Helper()

	var found int
	if err := a.store.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM events WHERE cloud = $1`, cloud).Scan(&found); err != nil {
		t.Fatalf("counting the events of %s: %v", cloud, err)
	}
	return found
}

// deadLetters is how many items of cloud were dead-lettered. A refused item
// leaves no events row, so the raw document is the only place its cloud
// survives.
func deadLetters(t *testing.T, a api, cloud string) int {
	t.Helper()

	var found int
	if err := a.store.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM rejected_events WHERE raw->>'cloud' = $1`, cloud).Scan(&found); err != nil {
		t.Fatalf("counting the dead-letter rows of %s: %v", cloud, err)
	}
	return found
}

// resourceRows reads every projection row of cloud as the JSON document
// Postgres renders it as, which compares two rows over all their columns at
// once.
func resourceRows(t *testing.T, a api, cloud string) map[string]string {
	t.Helper()

	rows, err := a.store.Pool().Query(t.Context(),
		`SELECT resource_id, to_jsonb(c)::text FROM current_resources c WHERE cloud = $1`, cloud)
	if err != nil {
		t.Fatalf("reading the projection rows of %s: %v", cloud, err)
	}
	defer rows.Close()

	found := map[string]string{}
	for rows.Next() {
		var id, row string
		if err := rows.Scan(&id, &row); err != nil {
			t.Fatalf("scanning a projection row: %v", err)
		}
		found[id] = row
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the projection rows of %s: %v", cloud, err)
	}
	return found
}

// tableCounts is how many rows each table an ingest call can write holds right
// now, which is what a call that must touch nothing is measured against.
func tableCounts(t *testing.T, a api) map[string]int {
	t.Helper()

	counts := map[string]int{}
	for _, table := range []string{"events", "rejected_events", "current_resources", "audit_log"} {
		var found int
		if err := a.store.Pool().QueryRow(t.Context(), `SELECT count(*) FROM `+table).Scan(&found); err != nil {
			t.Fatalf("counting the rows of %s: %v", table, err)
		}
		counts[table] = found
	}
	return counts
}
