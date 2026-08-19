package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/b42labs/tally/internal/core/event"
	"github.com/b42labs/tally/internal/reporting/auth"
	"github.com/b42labs/tally/internal/reporting/httpapi/problem"
	"github.com/b42labs/tally/internal/reporting/ingest"
	"github.com/b42labs/tally/internal/reporting/reconciliation"
	"github.com/b42labs/tally/internal/reporting/registry"
	"github.com/b42labs/tally/internal/reporting/store"
	"github.com/b42labs/tally/internal/reporting/store/sqlcgen"
	"github.com/b42labs/tally/internal/reporting/store/storetest"
)

// The project the summarized fixture belongs to. A summary reads the events of
// one (cloud, external id) pair, so the pair the registry holds has to be the
// pair the events carry.
const (
	summaryCloud     = "os-summary"
	summaryProjectID = "summary-project"
)

// The window the summary is asked for and the instant the server takes as now.
// Both are fixtures: an interval still open is billed up to now, so the minutes
// the answer reports are only hand-computable when the clock is pinned.
var (
	summaryFrom = time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	summaryTo   = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	summaryNow  = time.Date(2026, 2, 15, 12, 0, 0, 0, time.UTC)
)

// The instants the three resources of the fixture live at: one created before
// the window and deleted inside it, one created inside it and still running, and
// one whose history starts mid-life.
var (
	summaryCreatedBefore = time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	summaryDeletedInside = time.Date(2026, 2, 5, 0, 0, 0, 0, time.UTC)
	summaryCreatedInside = time.Date(2026, 2, 10, 0, 0, 0, 0, time.UTC)
	summaryPickedUp      = time.Date(2026, 2, 12, 0, 0, 0, 0, time.UTC)
)

// summaryActivity is what the fixture did inside [summaryFrom, summaryTo), with
// now at 02-15T12:00, hand-computed:
//
//	the deleted instance was created before the window and deleted on 02-05, so
//	it counts as deleted once and bills 02-01 to 02-05, 4 days;
//
//	the open instance was created on 02-10 and still runs, so it counts as
//	created once and bills 02-10 to now, 5 days and 12 hours;
//
//	the volume's history starts without a create, so neither counter moves and
//	the interval its first event opens bills 02-12 to now, 3 days and 12 hours.
//
// That is 5760 + 7920 minutes for the instances and 5040 for the volume.
//
// active_now comes from the projection instead: the deleted instance keeps its
// row under state 'deleted' and is left out, the open one is counted, and the
// volume has no row at all, because its event was stored past the pipeline that
// writes them.
var summaryActivity = []ProjectActivity{
	{ResourceType: "instance", ActiveNow: 1, Created: 1, Deleted: 1, TotalMinutes: 13680},
	{ResourceType: "volume", TotalMinutes: 5040},
}

// TestGetProjectSummaryOverHTTP drives GET /api/v1/projects/{id}/summary through
// the whole stack: the registry lookup, the project scope, the fold of the
// project's history, and the projection behind active_now.
//
// The subtests share one database. Each fixture other than the summarized one
// gets a cloud and a project of its own, so no history reaches another's answer.
func TestGetProjectSummaryOverHTTP(t *testing.T) {
	db := storetest.NewDB(t)
	a := newAPIAtInstant(t, db.Store, summaryNow)

	_, adminToken := seedAPIToken(t, a.queries, auth.RoleAdmin, nil)
	_, readAllToken := seedAPIToken(t, a.queries, auth.RoleReadAll, nil)

	project := seedProject(t, a, summaryCloud, summaryProjectID)
	seedSummaryFixture(t, a)

	t.Run("counts what each resource type did inside the window", func(t *testing.T) {
		got := projectSummaryOf(t, a.call(t, http.MethodGet,
			projectSummaryPath(project, summaryFrom, summaryTo), adminToken, nil))

		if got.Project.Id != project {
			t.Errorf("project id = %s, want %s", got.Project.Id, project)
		}
		if got.Project.ExternalId != summaryProjectID || got.Project.Cloud != summaryCloud {
			t.Errorf("project = (%s, %s), want (%s, %s)", got.Project.Cloud,
				got.Project.ExternalId, summaryCloud, summaryProjectID)
		}

		// The rows are the hand-computed ones, in the order the contract states:
		// what each number stands for is written out at summaryActivity.
		if !reflect.DeepEqual(got.ResourceTypes, summaryActivity) {
			t.Errorf("resource types = %+v, want %+v", got.ResourceTypes, summaryActivity)
		}
	})

	t.Run("bills no minute past now for a window that ends in the future", func(t *testing.T) {
		// The window runs a year past the pinned now, and the answer is the one the
		// window ending at to gives: an interval still open has accrued up to now
		// and no further, whatever a caller asks for.
		got := projectSummaryOf(t, a.call(t, http.MethodGet,
			projectSummaryPath(project, summaryFrom, summaryTo.AddDate(1, 0, 0)), adminToken, nil))

		if !reflect.DeepEqual(got.ResourceTypes, summaryActivity) {
			t.Errorf("resource types = %+v, want the %+v the window ending at to reports",
				got.ResourceTypes, summaryActivity)
		}
	})

	t.Run("zeroes the window a from at the to opens while active_now stays what it is", func(t *testing.T) {
		// The history is read up to the to and folded, so both types still have a
		// row; what the empty window clips away is every interval and every
		// lifecycle instant inside it.
		at := time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC)

		got := projectSummaryOf(t, a.call(t, http.MethodGet,
			projectSummaryPath(project, at, at), adminToken, nil))

		want := []ProjectActivity{
			{ResourceType: "instance", ActiveNow: 1},
			{ResourceType: "volume"},
		}
		if !reflect.DeepEqual(got.ResourceTypes, want) {
			t.Errorf("resource types = %+v, want %+v", got.ResourceTypes, want)
		}
	})

	t.Run("reports the projection alone for a window the history does not reach", func(t *testing.T) {
		// The window ends before the first event of the project, so the fold reads
		// nothing at all and every number it produces is missing rather than zero.
		// What the answer carries is the resource types the project runs today,
		// zero-filled on the side the window would have answered.
		at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

		got := projectSummaryOf(t, a.call(t, http.MethodGet,
			projectSummaryPath(project, at, at), adminToken, nil))

		// The volume has no projection row, so this window says nothing about it.
		want := []ProjectActivity{{ResourceType: "instance", ActiveNow: 1}}
		if !reflect.DeepEqual(got.ResourceTypes, want) {
			t.Errorf("resource types = %+v, want %+v", got.ResourceTypes, want)
		}
	})

	t.Run("answers a project with neither events nor resources with the empty array", func(t *testing.T) {
		const cloud = "os-summary-empty"
		empty := seedProject(t, a, cloud, "summary-empty")

		rec := a.call(t, http.MethodGet, projectSummaryPath(empty, summaryFrom, summaryTo), adminToken, nil)

		if got := projectSummaryOf(t, rec); len(got.ResourceTypes) != 0 {
			t.Errorf("resource types = %+v, want none for a project with neither", got.ResourceTypes)
		}
		// The raw body rather than the decoded one: a client iterates the rows
		// without a nil check, which null would break and [] does not.
		if body := rec.Body.String(); !strings.Contains(body, `"resource_types":[]`) {
			t.Errorf("body = %s, want it to carry \"resource_types\":[]", body)
		}
	})

	t.Run("answers a project outside the scope the way it answers an unknown id", func(t *testing.T) {
		// A token that could tell the two apart would be able to map the projects
		// of other tokens by asking for summaries one id at a time.
		const cloud = "os-summary-foreign"
		foreign := seedProject(t, a, cloud, "summary-foreign")
		_, token := seedAPIToken(t, a.queries, auth.RoleProject, []uuid.UUID{foreign})

		unknown := a.call(t, http.MethodGet,
			projectSummaryPath(uuid.New(), summaryFrom, summaryTo), token, nil)
		outside := a.call(t, http.MethodGet,
			projectSummaryPath(project, summaryFrom, summaryTo), token, nil)

		for name, rec := range map[string]*httptest.ResponseRecorder{
			"an unknown id":                       unknown,
			"a project outside the token's scope": outside,
		} {
			assertProblem(t, rec, http.StatusNotFound, problem.TypeNotFound)
			if got, want := problemDetail(t, rec), "this project is not registered"; got != want {
				t.Errorf("detail of %s = %q, want %q", name, got, want)
			}
		}
		if !bytes.Equal(outside.Body.Bytes(), unknown.Body.Bytes()) {
			t.Errorf("the body of a project outside the scope = %s, want the %s of an unknown id",
				outside.Body, unknown.Body)
		}
	})

	t.Run("refuses a history longer than one summary folds", func(t *testing.T) {
		const (
			cloud     = "os-summary-long"
			projectID = "summary-long"
		)
		long := seedProject(t, a, cloud, projectID)
		// One event past the bound, written through the pool rather than ingested:
		// what is being pinned is the handler's refusal, and folding a hundred
		// thousand events through the write path would say nothing more about it.
		// They sit a second apart below the to, so the read the refusal is decided
		// on covers all of them.
		if _, err := a.store.Pool().Exec(t.Context(),
			`INSERT INTO events (event_id, timestamp, event_type, platform, cloud,
			                     resource_type, resource_id, project_id, source, payload)
			 SELECT 'summary-long-' || n, $1::timestamptz + (n * INTERVAL '1 second'),
			        'volume.update', $2, $3, 'volume', 'vol-summary-long', $4, 'collector',
			        '{"state": "available"}'::jsonb
			 FROM generate_series(1, $5::int) AS n`,
			summaryFrom, fixturePlatform, cloud, projectID, maxSummaryEvents+1); err != nil {
			t.Fatalf("writing a project history past the bound: %v", err)
		}

		rec := a.call(t, http.MethodGet, projectSummaryPath(long, summaryFrom, summaryTo), adminToken, nil)

		assertProblem(t, rec, http.StatusUnprocessableEntity, problem.TypeHistoryTooLong)
	})

	t.Run("folds a row whose payload nothing could decode", func(t *testing.T) {
		// The payload column is typed as an object by the contract, and the write
		// path stores nothing else; a row written past this API could hold any
		// JSON. The summary reads no payload at all, because no number it reports
		// is decided by one, so a row carrying an array folds like every other
		// row. That is what says the column stays out of this read: were it
		// selected and unmarshalled, this row is the one that could not be.
		const (
			cloud     = "os-summary-undecodable"
			projectID = "summary-undecodable"
		)
		bad := seedProject(t, a, cloud, projectID)
		res := fixture{cloud: cloud, resourceType: "volume", id: "vol-summary-undecodable"}
		insertEventRow(t, a, inProject(res.event("summary-undecodable-update", "volume.update",
			summaryCreatedInside, event.PayloadEnvelope{}), projectID), []byte(`[1,2]`))

		got := projectSummaryOf(t, a.call(t, http.MethodGet,
			projectSummaryPath(bad, summaryFrom, summaryTo), adminToken, nil))

		// The one event opens an interval on 02-10 that is still open, so it bills
		// up to the pinned now on 02-15T12:00: 5 days and 12 hours.
		want := []ProjectActivity{{ResourceType: "volume", TotalMinutes: 7920}}
		if !reflect.DeepEqual(got.ResourceTypes, want) {
			t.Errorf("resource types = %+v, want %+v", got.ResourceTypes, want)
		}
	})

	t.Run("bills a transferred resource to one project at a time", func(t *testing.T) {
		// The transfer writes the new project onto the event that moves the
		// resource, so the old owner's own events end there. Reading them alone
		// leaves an interval that never closes and bills up to now, beside the new
		// owner billing the same resource from the transfer on.
		const (
			cloud    = "os-summary-transfer"
			oldOwner = "summary-transfer-old"
			newOwner = "summary-transfer-new"
		)
		before := seedProject(t, a, cloud, oldOwner)
		after := seedProject(t, a, cloud, newOwner)

		res := fixture{cloud: cloud, resourceType: "volume", id: "vol-summary-transfer"}
		ingestEvents(t, a, fixturePlatform, cloud,
			inProject(res.event("summary-transfer-create", "volume.create",
				summaryCreatedInside, payloadOf("available", volumeSize(10))), oldOwner),
			inProject(res.event("summary-transfer-move", "volume.update",
				summaryPickedUp, payloadOf("available", volumeSize(10))), newOwner))

		// Created on 02-10 and handed over on 02-12: two days for the old owner,
		// who created it and no longer runs it. The projection follows the
		// transfer, so active_now is the new owner's.
		gave := projectSummaryOf(t, a.call(t, http.MethodGet,
			projectSummaryPath(before, summaryFrom, summaryTo), adminToken, nil))
		wantGave := []ProjectActivity{{ResourceType: "volume", Created: 1, TotalMinutes: 2880}}
		if !reflect.DeepEqual(gave.ResourceTypes, wantGave) {
			t.Errorf("resource types of the old owner = %+v, want %+v", gave.ResourceTypes, wantGave)
		}

		// From 02-12 up to the pinned now on 02-15T12:00: three days and twelve
		// hours. The create belongs to the old owner, so it is not counted here.
		took := projectSummaryOf(t, a.call(t, http.MethodGet,
			projectSummaryPath(after, summaryFrom, summaryTo), adminToken, nil))
		wantTook := []ProjectActivity{{ResourceType: "volume", ActiveNow: 1, TotalMinutes: 5040}}
		if !reflect.DeepEqual(took.ResourceTypes, wantTook) {
			t.Errorf("resource types of the new owner = %+v, want %+v", took.ResourceTypes, wantTook)
		}

		// What the two of them bill together is the resource's whole life inside
		// the window and not a minute more, which is the property a transfer must
		// not break: one resource, one span, one bill.
		if got, want := gave.ResourceTypes[0].TotalMinutes+took.ResourceTypes[0].TotalMinutes,
			int64(7920); got != want {
			t.Errorf("minutes billed by both owners = %d, want the %d of one life", got, want)
		}
	})

	t.Run("serves a project token the project it holds", func(t *testing.T) {
		// The route is guarded lower than the registry read it hangs off: a summary
		// reports what a project ran, which is what its own token is for.
		_, token := seedAPIToken(t, a.queries, auth.RoleProject, []uuid.UUID{project})

		got := projectSummaryOf(t, a.call(t, http.MethodGet,
			projectSummaryPath(project, summaryFrom, summaryTo), token, nil))

		if !reflect.DeepEqual(got.ResourceTypes, summaryActivity) {
			t.Errorf("resource types = %+v, want %+v", got.ResourceTypes, summaryActivity)
		}
	})

	t.Run("serves a read_all token any project", func(t *testing.T) {
		admin := projectSummaryOf(t, a.call(t, http.MethodGet,
			projectSummaryPath(project, summaryFrom, summaryTo), adminToken, nil))

		got := projectSummaryOf(t, a.call(t, http.MethodGet,
			projectSummaryPath(project, summaryFrom, summaryTo), readAllToken, nil))

		if !reflect.DeepEqual(got.ResourceTypes, admin.ResourceTypes) {
			t.Errorf("resource types = %+v, want the %+v an admin token reads",
				got.ResourceTypes, admin.ResourceTypes)
		}
	})

	t.Run("refuses a request this API cannot authenticate", func(t *testing.T) {
		rec := a.call(t, http.MethodGet, projectSummaryPath(project, summaryFrom, summaryTo), "", nil)

		assertChallenged(t, rec)
	})
}

// seedSummaryFixture stores the three resources the summary subtests describe.
//
// The two instances go in through POST /api/v1/events, so both halves of the
// answer are real: the events the window folds and the projection rows
// active_now counts. The volume is written through the pool instead, which
// stores an event and no projection row, so what the fold reports of it and what
// the projection reports of it can be told apart.
func seedSummaryFixture(t *testing.T, a api) {
	t.Helper()

	gone := fixture{cloud: summaryCloud, resourceType: "instance", id: "inst-summary-gone"}
	alive := fixture{cloud: summaryCloud, resourceType: "instance", id: "inst-summary-alive"}
	// A size the seeded (openstack, instance) schema accepts.
	flavor := map[string]any{"vcpus": 2, "ram_gb": 4, "disk_gb": 20, "flavor": "m1.small"}

	ingestEvents(t, a, fixturePlatform, summaryCloud,
		inProject(gone.event("summary-gone-create", "instance.create",
			summaryCreatedBefore, payloadOf("active", flavor)), summaryProjectID),
		inProject(gone.event("summary-gone-delete", "instance.delete",
			summaryDeletedInside, event.PayloadEnvelope{}), summaryProjectID),
		inProject(alive.event("summary-alive-create", "instance.create",
			summaryCreatedInside, payloadOf("active", flavor)), summaryProjectID))

	// A volume the project was handed mid-life: its history starts with an update,
	// which is what a missed create leaves behind. The fold opens an interval at
	// that first event and warns about it, so the volume bills from there on
	// without ever having been created inside the window.
	orphan := fixture{cloud: summaryCloud, resourceType: "volume", id: "vol-summary-orphan"}
	update := inProject(orphan.event("summary-orphan-update", "volume.update",
		summaryPickedUp, payloadOf("available", volumeSize(10))), summaryProjectID)
	payload, err := json.Marshal(update.Payload)
	if err != nil {
		t.Fatalf("marshaling the payload of %s: %v", update.EventID, err)
	}
	insertEventRow(t, a, update, payload)
}

// newAPIAtInstant builds the full router over s with authentication enforced and
// the server's clock pinned to now. The shared harness leaves the clock at
// time.Now, and the summary bills an interval still open up to whatever it
// reads, so a test that hand-computes those minutes builds its router here.
func newAPIAtInstant(t *testing.T, s *store.Store, now time.Time) api {
	t.Helper()

	q := sqlcgen.New(s.Pool())
	handler, err := NewRouter(Options{
		Logger:             slog.New(slog.NewJSONHandler(io.Discard, nil)),
		DB:                 s,
		UnhealthyThreshold: time.Minute,
		Now:                func() time.Time { return now },
		Queries:            q,
		Store:              s,
		AuthMode:           auth.ModeEnforced,
		InternalToken:      internalToken,
		Authenticator:      auth.NewStaticTokenAuthenticator(q),
		Pipeline:           ingest.New(registry.New(), false, nil, nil),
		Syncer: reconciliation.New(s, ingest.New(registry.New(), false, nil, nil),
			reconciliation.Config{}, map[string]reconciliation.Adapter{}, time.Now, nil),
	})
	if err != nil {
		t.Fatalf("NewRouter() error = %v, want nil", err)
	}
	return api{store: s, queries: q, handler: handler}
}

// projectSummaryPath is the summary route of one project over one window, as the
// query string carries it.
func projectSummaryPath(id uuid.UUID, from, to time.Time) string {
	return projectPath(id) + "/summary?from=" + instant(from) + "&to=" + instant(to)
}

// projectSummaryOf decodes the answer of a summary call, which the contract
// promises is a 200 carrying the project and its resource types.
func projectSummaryOf(t *testing.T, rec *httptest.ResponseRecorder) ProjectSummary {
	t.Helper()

	if got := rec.Code; got != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %q)", got, http.StatusOK, rec.Body)
	}
	if got, want := rec.Header().Get("Content-Type"), "application/json"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}

	var got ProjectSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding the body %q: %v", rec.Body.String(), err)
	}
	if got.ResourceTypes == nil {
		t.Errorf("body %q carries resource_types as null, want an array", rec.Body)
	}
	return got
}
