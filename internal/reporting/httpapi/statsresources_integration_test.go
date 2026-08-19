package httpapi

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/b42labs/tally/internal/core/event"
	"github.com/b42labs/tally/internal/reporting/auth"
	"github.com/b42labs/tally/internal/reporting/httpapi/problem"
	"github.com/b42labs/tally/internal/reporting/store/storetest"
)

// resourceStatsRoute is the route the resource statistics tests call, spelled
// once because every one of them addresses it.
const resourceStatsRoute = "/api/v1/stats/resources"

// The clouds the counted fleet lives in, and the platform of the third. This
// route carries no filter at all, so what it counts is the whole projection:
// the clouds are what the assertions tell the fixture's parts apart by.
const (
	statsCloudA    = "os-stats-a"
	statsCloudB    = "os-stats-b"
	statsCloudC    = "tally-stats-c"
	statsPlatformC = "tallytest"
)

// The projects the fleet names. The second one shares a cloud with the first,
// and the first is also the project of a resource in another cloud, so a scope
// narrowing on the (cloud, project) pair answers something neither half of the
// pair answers alone.
const (
	statsProjectA = "stats-project-a"
	statsProjectB = "stats-project-b"
)

// The instants the fleet is built at: a create, a change, and a delete an hour
// apart. What is counted is the projection rather than the events, so the
// instants matter only in that the delete comes last.
var (
	statsCreated = time.Date(2026, 5, 1, 6, 0, 0, 0, time.UTC)
	statsChanged = statsCreated.Add(time.Hour)
	statsRemoved = statsCreated.Add(2 * time.Hour)
)

// statsFlavor is a size the seeded (openstack, instance) schema accepts.
var statsFlavor = map[string]any{"vcpus": 2, "ram_gb": 4, "disk_gb": 20, "flavor": "m1.small"}

// TestGetResourceStatsOverHTTP drives GET /api/v1/stats/resources through the
// whole stack: the contract validator, the grouping rule the handler enforces,
// the project scope, and the projection rows the counts come from.
//
// The subtests share one database and run in order. The route counts the whole
// projection, so the fleet is seeded between the one subtest that describes an
// empty projection and the ones that describe the fleet; the subtests after
// those register projects, which counts no resource of their own.
func TestGetResourceStatsOverHTTP(t *testing.T) {
	db := storetest.NewDB(t)
	a := newAPI(t, db.Store)

	_, adminToken := seedAPIToken(t, a.queries, auth.RoleAdmin, nil)
	_, readAllToken := seedAPIToken(t, a.queries, auth.RoleReadAll, nil)

	t.Run("answers a projection holding nothing with the empty array", func(t *testing.T) {
		rec := a.call(t, http.MethodGet,
			resourceStatsRoute+"?group_by=cloud,resource_type", adminToken, nil)

		if got := rec.Code; got != http.StatusOK {
			t.Fatalf("status = %d, want %d (body %q)", got, http.StatusOK, rec.Body)
		}
		// The raw body rather than the decoded one: a client iterates items
		// without a nil check, which null would break and [] does not.
		want := `{"items":[]}`
		if got := strings.TrimSpace(rec.Body.String()); got != want {
			t.Errorf("body = %s, want %s", got, want)
		}
	})

	seedStatsFleet(t, a)

	t.Run("counts the live fleet along the two dimensions an item is read by", func(t *testing.T) {
		rec := a.call(t, http.MethodGet,
			resourceStatsRoute+"?group_by=cloud,resource_type", adminToken, nil)

		list := resourceStatsOf(t, rec)
		// The deleted volume is not in these counts: the contract's default is
		// active, and the status subtests below are what say so in full.
		want := []string{
			statsCloudA + "/instance/-/-/-=2",
			statsCloudA + "/volume/-/-/-=2",
			statsCloudB + "/instance/-/-/-=1",
			statsCloudC + "/widget/-/-/-=1",
		}
		if got := renderResourceItems(list.Items); !slices.Equal(got, want) {
			t.Errorf("counted groups = %q, want %q", got, want)
		}
		// A dash stands for a member that decoded to nil, which a member holding
		// the empty string would not produce; the raw body is what says the three
		// dimensions this grouping did not name are off the wire entirely.
		body := rec.Body.String()
		for _, member := range []string{`"state"`, `"platform"`, `"project_id"`} {
			if strings.Contains(body, member) {
				t.Errorf("body = %s, want it to carry no %s", body, member)
			}
		}
	})

	t.Run("carries every optional member a grouping names", func(t *testing.T) {
		list := resourceStatsOf(t, a.call(t, http.MethodGet,
			resourceStatsRoute+"?group_by=cloud,resource_type,state,platform,project_id", adminToken, nil))

		// The finest grouping there is: one item per resource, since no two live
		// resources of the fleet agree on all five dimensions.
		want := []string{
			statsCloudA + "/instance/active/openstack/" + statsProjectA + "=1",
			statsCloudA + "/instance/shutoff/openstack/" + statsProjectA + "=1",
			statsCloudA + "/volume/available/openstack/" + statsProjectA + "=1",
			statsCloudA + "/volume/available/openstack/" + statsProjectB + "=1",
			statsCloudB + "/instance/active/openstack/" + statsProjectA + "=1",
			statsCloudC + "/widget/ready/" + statsPlatformC + "/" + statsProjectA + "=1",
		}
		if got := renderResourceItems(list.Items); !slices.Equal(got, want) {
			t.Errorf("counted groups = %q, want %q", got, want)
		}
	})

	t.Run("counts the fleet the status names", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			query string
			want  []string
		}{
			{
				// No status at all: the contract's default is active, and the
				// deleted volume missing from the counts is what says it was
				// applied.
				name:  "no status",
				query: "",
				want: []string{
					statsCloudA + "/instance/active/-/-=1",
					statsCloudA + "/instance/shutoff/-/-=1",
					statsCloudA + "/volume/available/-/-=2",
					statsCloudB + "/instance/active/-/-=1",
					statsCloudC + "/widget/ready/-/-=1",
				},
			},
			{
				name:  "status=all",
				query: "&status=all",
				want: []string{
					statsCloudA + "/instance/active/-/-=1",
					statsCloudA + "/instance/shutoff/-/-=1",
					statsCloudA + "/volume/available/-/-=2",
					statsCloudA + "/volume/deleted/-/-=1",
					statsCloudB + "/instance/active/-/-=1",
					statsCloudC + "/widget/ready/-/-=1",
				},
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				list := resourceStatsOf(t, a.call(t, http.MethodGet,
					resourceStatsRoute+"?group_by=cloud,resource_type,state"+tc.query, adminToken, nil))

				if got := renderResourceItems(list.Items); !slices.Equal(got, tc.want) {
					t.Errorf("counted groups = %q, want %q", got, tc.want)
				}
			})
		}

		t.Run("counts every row the metrics gauge counts", func(t *testing.T) {
			// The gauge and this route read the same projection through two
			// queries, and the fixture is the whole of it, so the counts of one
			// have to be the counts of the other. The gauge groups by platform as
			// well, which is a dimension this grouping drops, so its rows are
			// summed over it the way a coarser grouping sums them.
			list := resourceStatsOf(t, a.call(t, http.MethodGet,
				resourceStatsRoute+"?group_by=cloud,resource_type,state&status=all", adminToken, nil))

			counted := make(map[string]int64, len(list.Items))
			for _, item := range list.Items {
				if item.State == nil {
					t.Fatalf("item %+v carries no state, want the dimension it was grouped by", item)
				}
				counted[item.Cloud+"/"+item.ResourceType+"/"+*item.State] += item.Count
			}

			if want := gaugeGroups(t, a); !maps.Equal(counted, want) {
				t.Errorf("counted groups = %v, want the %v the gauge reports", counted, want)
			}
		})
	})

	t.Run("refuses a grouping that cannot name what it counted", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			query string
		}{
			{name: "without cloud", query: "group_by=resource_type"},
			{name: "without resource_type", query: "group_by=cloud"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				rec := a.call(t, http.MethodGet, resourceStatsRoute+"?"+tc.query, adminToken, nil)

				assertProblem(t, rec, http.StatusBadRequest, problem.TypeValidation)
				if got, want := problemDetail(t, rec),
					"group_by must name cloud and resource_type"; got != want {
					t.Errorf("detail = %q, want %q", got, want)
				}
			})
		}
	})

	t.Run("refuses a grouping the contract does not allow", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			query string
		}{
			{name: "a dimension outside the enum", query: "group_by=cloud,resource_type,bogus"},
			{name: "a dimension named twice", query: "group_by=cloud,cloud,resource_type"},
			{name: "no grouping at all", query: ""},
		} {
			t.Run(tc.name, func(t *testing.T) {
				rec := a.call(t, http.MethodGet, resourceStatsRoute+"?"+tc.query, adminToken, nil)

				assertProblem(t, rec, http.StatusBadRequest, problem.TypeValidation)
			})
		}
	})

	t.Run("refuses every at, whatever instant it names", func(t *testing.T) {
		// The projection holds the present alone, and an instant meaning "now"
		// cannot be told from a historic one, so both are refused rather than one
		// of them answered from the rows that happen to be there.
		for _, tc := range []struct {
			name string
			at   time.Time
		}{
			{name: "an instant in the past", at: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
			{name: "an instant meaning now", at: time.Now().UTC()},
		} {
			t.Run(tc.name, func(t *testing.T) {
				rec := a.call(t, http.MethodGet,
					resourceStatsRoute+"?group_by=cloud,resource_type&at="+instant(tc.at), adminToken, nil)

				assertProblem(t, rec, http.StatusNotImplemented, problem.TypeNotImplemented)
			})
		}
	})

	t.Run("confines a project token to the projects it holds", func(t *testing.T) {
		// The fleet holds the other project of this cloud and the same project id
		// under another cloud, so what the token counts says the scope narrows on
		// the pair rather than on either half of it.
		held := seedProject(t, a, statsCloudA, statsProjectA)
		_, token := seedAPIToken(t, a.queries, auth.RoleProject, []uuid.UUID{held})

		list := resourceStatsOf(t, a.call(t, http.MethodGet,
			resourceStatsRoute+"?group_by=cloud,resource_type", token, nil))

		want := []string{
			statsCloudA + "/instance/-/-/-=2",
			statsCloudA + "/volume/-/-/-=1",
		}
		if got := renderResourceItems(list.Items); !slices.Equal(got, want) {
			t.Errorf("counted groups = %q, want %q", got, want)
		}
	})

	t.Run("answers a token whose scope holds no project with the empty array", func(t *testing.T) {
		// A project token always carries at least one project id, which the table
		// constrains; what it names may be gone, and the scope then narrows to
		// nothing rather than widening to everything.
		_, token := seedAPIToken(t, a.queries, auth.RoleProject, []uuid.UUID{uuid.New()})

		rec := a.call(t, http.MethodGet,
			resourceStatsRoute+"?group_by=cloud,resource_type", token, nil)

		if got := rec.Code; got != http.StatusOK {
			t.Fatalf("status = %d, want %d (body %q)", got, http.StatusOK, rec.Body)
		}
		want := `{"items":[]}`
		if got := strings.TrimSpace(rec.Body.String()); got != want {
			t.Errorf("body = %s, want %s", got, want)
		}
	})

	t.Run("refuses a request this API cannot authenticate", func(t *testing.T) {
		rec := a.call(t, http.MethodGet,
			resourceStatsRoute+"?group_by=cloud,resource_type", "", nil)

		assertChallenged(t, rec)
	})

	t.Run("serves a read_all token the whole fleet", func(t *testing.T) {
		query := resourceStatsRoute + "?group_by=cloud,resource_type,state&status=all"
		admin := resourceStatsOf(t, a.call(t, http.MethodGet, query, adminToken, nil))

		list := resourceStatsOf(t, a.call(t, http.MethodGet, query, readAllToken, nil))

		if got, want := renderResourceItems(list.Items), renderResourceItems(admin.Items); !slices.Equal(got, want) {
			t.Errorf("counted groups = %q, want the %q an admin token counts", got, want)
		}
	})
}

// TestGetResourceStatsRefusesAFleetAboveTheBound pins the row bound of the
// route, on a database of its own: the counts cover the whole projection, so a
// fleet this size seeded beside the fixture above would move every count the
// subtests there assert on.
//
// The grouping names project_id, which is the one dimension that carries a value
// per tenant and so the one the row set grows along.
func TestGetResourceStatsRefusesAFleetAboveTheBound(t *testing.T) {
	db := storetest.NewDB(t)
	a := newAPI(t, db.Store)

	_, adminToken := seedAPIToken(t, a.queries, auth.RoleAdmin, nil)
	query := resourceStatsRoute + "?group_by=cloud,resource_type,project_id"

	seedStatsBulkFleet(t, a, 1, maxResourceStatsRows)

	// The row one past the bound is what the refusal is decided on, so a fleet
	// filling it exactly is the boundary the answer still holds.
	list := resourceStatsOf(t, a.call(t, http.MethodGet, query, adminToken, nil))
	if got := len(list.Items); got != maxResourceStatsRows {
		t.Fatalf("counted groups = %d, want the %d the bound allows", got, maxResourceStatsRows)
	}

	seedStatsBulkFleet(t, a, maxResourceStatsRows+1, maxResourceStatsRows+1)

	rec := a.call(t, http.MethodGet, query, adminToken, nil)

	assertProblem(t, rec, http.StatusUnprocessableEntity, problem.TypeResultTooLarge)
}

// seedStatsBulkFleet writes one live projection row per project in the numbered
// range, all of them in one cloud and of one type, so the number of groups the
// finest counting sees is the number of rows written.
//
// They go in through the pool rather than through ingestion: what is being
// pinned is the handler's refusal, and folding ten thousand events into the
// projection would say nothing more about it.
func seedStatsBulkFleet(t *testing.T, a api, first, last int) {
	t.Helper()

	if _, err := a.store.Pool().Exec(t.Context(),
		`INSERT INTO current_resources (cloud, platform, resource_type, resource_id,
		                                project_id, state, last_event_type, last_event_at)
		 SELECT $1, $2, 'volume', 'vol-stats-bulk-' || n, 'stats-bulk-project-' || n,
		        'available', 'volume.create', $3::timestamptz
		 FROM generate_series($4::int, $5::int) AS n`,
		statsCloudA, fixturePlatform, statsCreated, first, last); err != nil {
		t.Fatalf("writing a fleet past the bound: %v", err)
	}
}

// TestGetResourceStatsReportsAFailedQuery drives the handler's own failure path.
// With authentication disabled nothing reads the database before the count does,
// so a database that is gone stops the request inside the query rather than at
// the credential lookup in front of it.
func TestGetResourceStatsReportsAFailedQuery(t *testing.T) {
	db := storetest.NewDB(t)
	a := newAPIInMode(t, db.Store, auth.ModeDisabled)

	// How long the database gets to shut down cleanly.
	stopTimeout := 10 * time.Second
	if err := db.Container.Stop(t.Context(), &stopTimeout); err != nil {
		t.Fatalf("stopping the database container: %v", err)
	}

	rec := a.call(t, http.MethodGet, resourceStatsRoute+"?group_by=cloud,resource_type", "", nil)

	assertProblem(t, rec, http.StatusInternalServerError, problem.TypeInternal)
	// The detail is what tells the internal problems of this route apart: the
	// count failed, rather than a scope that could not be resolved.
	if got, want := problemDetail(t, rec), "the resource counts could not be read"; got != want {
		t.Errorf("detail = %q, want %q", got, want)
	}
}

// seedStatsFleet stores the resources the counting subtests describe. They go in
// through POST /api/v1/events, so the counts are taken off the rows the write
// path really folded.
//
// The fleet is shaped so that every dimension of the grouping separates
// something: two instances of one project in one state each, two volumes of two
// projects in one cloud, a third volume that was deleted, an instance of the
// first project in another cloud, and a resource of a platform of its own.
func seedStatsFleet(t *testing.T, a api) {
	t.Helper()

	active := fixture{cloud: statsCloudA, resourceType: "instance", id: "inst-stats-active"}
	off := fixture{cloud: statsCloudA, resourceType: "instance", id: "inst-stats-off"}
	alive := fixture{cloud: statsCloudA, resourceType: "volume", id: "vol-stats-alive"}
	gone := fixture{cloud: statsCloudA, resourceType: "volume", id: "vol-stats-gone"}
	sibling := fixture{cloud: statsCloudA, resourceType: "volume", id: "vol-stats-sibling"}
	elsewhere := fixture{cloud: statsCloudB, resourceType: "instance", id: "inst-stats-elsewhere"}
	widget := fixture{cloud: statsCloudC, resourceType: "widget", id: "widget-stats"}

	ingestEvents(t, a, fixturePlatform, statsCloudA,
		inProject(active.event("inst-stats-active-create", "instance.create",
			statsCreated, payloadOf("active", statsFlavor)), statsProjectA),
		// The one instance that was powered off, which is the state that keeps
		// this cloud's two instances apart under a grouping by state.
		inProject(off.event("inst-stats-off-create", "instance.create",
			statsCreated, payloadOf("active", statsFlavor)), statsProjectA),
		inProject(off.event("inst-stats-off-power-off", "instance.power_off",
			statsChanged, payloadOf("shutoff", nil)), statsProjectA),
		inProject(alive.event("vol-stats-alive-create", "volume.create",
			statsCreated, payloadOf("available", volumeSize(10))), statsProjectA),
		// A deleted resource keeps its projection row under state 'deleted', so it
		// is what the status filter is readable on.
		inProject(gone.event("vol-stats-gone-create", "volume.create",
			statsCreated, payloadOf("available", volumeSize(20))), statsProjectA),
		inProject(gone.event("vol-stats-gone-delete", "volume.delete",
			statsRemoved, event.PayloadEnvelope{}), statsProjectA),
		inProject(sibling.event("vol-stats-sibling-create", "volume.create",
			statsCreated, payloadOf("available", volumeSize(30))), statsProjectB))

	ingestEvents(t, a, fixturePlatform, statsCloudB,
		inProject(elsewhere.event("inst-stats-elsewhere-create", "instance.create",
			statsCreated, payloadOf("active", statsFlavor)), statsProjectA))

	built := inProject(widget.event("widget-stats-create", "widget.create",
		statsCreated, payloadOf("ready", map[string]any{"units": 1})), statsProjectA)
	built.Platform = statsPlatformC
	ingestEvents(t, a, statsPlatformC, statsCloudC, built)
}

// inProject stamps which project an event's resource belongs to. The shared
// fixture builds every event under one project, and which project a resource
// belongs to is a dimension these counts are grouped and scoped by.
func inProject(e event.Event, projectID string) event.Event {
	e.ProjectID = projectID
	return e
}

// gaugeGroups is the projection as the metrics gauge counts it, keyed the way a
// grouping by cloud, resource type, and state keys its items. The gauge counts
// per platform too, so its rows are summed over that dimension here.
func gaugeGroups(t *testing.T, a api) map[string]int64 {
	t.Helper()

	rows, err := a.queries.CountCurrentResources(t.Context())
	if err != nil {
		t.Fatalf("CountCurrentResources() error = %v, want nil", err)
	}

	counts := make(map[string]int64, len(rows))
	for _, row := range rows {
		counts[row.Cloud+"/"+row.ResourceType+"/"+row.State] += row.Resources
	}
	return counts
}

// resourceStatsOf decodes the answer of a resource statistics call, which the
// contract promises is a 200 carrying the counted groups.
func resourceStatsOf(t *testing.T, rec *httptest.ResponseRecorder) ResourceStatsList {
	t.Helper()

	if got := rec.Code; got != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %q)", got, http.StatusOK, rec.Body)
	}
	if got, want := rec.Header().Get("Content-Type"), "application/json"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}

	var list ResourceStatsList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decoding the body %q: %v", rec.Body.String(), err)
	}
	if list.Items == nil {
		t.Errorf("body %q carries items as null, want an array", rec.Body)
	}
	return list
}
