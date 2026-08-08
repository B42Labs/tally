package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
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

// eventsRoute is the route the list tests call, spelled once because every one
// of them addresses it.
const eventsRoute = "/api/v1/events"

// The clouds the base set of the list tests lives in, and the platform of the
// third. Every subtest that stores more events works on a cloud of its own, so
// the base set stays what the unfiltered subtests describe.
const (
	listCloudA    = "os-list-a"
	listCloudB    = "os-list-b"
	listCloudC    = "tally-list-c"
	listPlatformC = "tallytest"
)

// The projects the base set names. The second one shares a cloud with the first,
// which is what makes the project filter more than another spelling of the cloud
// filter.
const (
	listProjectA = "list-project-a"
	listProjectB = "list-project-b"
)

// listBase is the instant the base set starts at. The events sit one minute
// apart on whole seconds, so what Postgres stores compares equal to what the
// test passed in, and the order they are declared in is the order the list
// serves them.
var listBase = time.Date(2026, 6, 1, 8, 0, 0, 0, time.UTC)

// listAt is the instant of the nth base event.
func listAt(n int) time.Time {
	return listBase.Add(time.Duration(n) * time.Minute)
}

// TestListEventsOverHTTP drives GET /api/v1/events through the whole stack: the
// contract validator, the query guard, the project scope, and the rows behind
// them.
//
// The subtests share one database and run in order: the ones describing the
// whole table come first, and the ones storing events of their own afterwards
// work on clouds those assertions do not name.
func TestListEventsOverHTTP(t *testing.T) {
	db := storetest.NewDB(t)
	a := newAPI(t, db.Store)

	_, adminToken := seedAPIToken(t, a.queries, auth.RoleAdmin, nil)
	_, readAllToken := seedAPIToken(t, a.queries, auth.RoleReadAll, nil)
	_, ingestToken := seedIngestCredential(t, a.queries, fixturePlatform, listCloudA)

	base := seedBaseEvents(t, a)

	t.Run("serves every stored event in timestamp and event id order", func(t *testing.T) {
		list := eventListOf(t, a.call(t, http.MethodGet, eventsRoute, adminToken, nil))

		if list.NextCursor != nil {
			t.Errorf("next_cursor = %q, want null on a page that holds everything", *list.NextCursor)
		}
		if got, want := ids(list.Items), base.ids(); !slices.Equal(got, want) {
			t.Fatalf("served events = %v, want %v", got, want)
		}
		// Every member the contract requires, checked against the event that was
		// submitted, so a mapper dropping or transposing a column is caught.
		for i, item := range list.Items {
			assertServedEvent(t, item, base[i], storedPayload(t, a, item.EventId))
		}
	})

	t.Run("serves a payload with every member the submitted one held", func(t *testing.T) {
		// The create event carries a provider block next to the state and the
		// size, so this compares three members rather than one.
		submitted := base.byID(t, "list-vol-create")
		query := "?cloud=" + listCloudA + "&event_type=" + url.QueryEscape(submitted.EventType)

		list := eventListOf(t, a.call(t, http.MethodGet, eventsRoute+query, adminToken, nil))

		if got, want := ids(list.Items), []string{submitted.EventID}; !slices.Equal(got, want) {
			t.Fatalf("served events = %v, want %v", got, want)
		}
		if list.Items[0].Payload == nil {
			t.Fatal("payload = null, want the stored envelope")
		}
		if got, want := *list.Items[0].Payload, asDocument(t, submitted.Payload); !reflect.DeepEqual(got, want) {
			t.Errorf("payload = %v, want the submitted %v", got, want)
		}
	})

	t.Run("serves an event stored without a payload as a null member", func(t *testing.T) {
		rec := a.call(t, http.MethodGet, eventsRoute+"?event_type=volume.delete", adminToken, nil)

		list := eventListOf(t, rec)
		if got, want := ids(list.Items), []string{"list-vol-delete"}; !slices.Equal(got, want) {
			t.Fatalf("served events = %v, want %v", got, want)
		}
		if list.Items[0].Payload != nil {
			t.Errorf("payload = %v, want it served as null", *list.Items[0].Payload)
		}
		// A pointer that decoded to nil would also come from a member left out, so
		// the raw body is what says the member is there and null.
		if body := rec.Body.String(); !strings.Contains(body, `"payload":null`) {
			t.Errorf("body = %s, want it to carry \"payload\":null", body)
		}
	})

	t.Run("narrows the answer to what a filter names", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			query string
			want  []string
		}{
			{
				name:  "cloud",
				query: "cloud=" + listCloudA,
				want: []string{
					"list-vol-create", "list-vol-resize", "list-inst-create",
					"list-vol-recon", "list-vol-delete",
				},
			},
			{
				name:  "platform",
				query: "platform=" + listPlatformC,
				want:  []string{"list-widget-create"},
			},
			{
				name:  "project_id",
				query: "project_id=" + listProjectB,
				want:  []string{"list-inst-create"},
			},
			{
				name:  "resource_type",
				query: "resource_type=instance",
				want:  []string{"list-inst-create"},
			},
			{
				name:  "event_type",
				query: "event_type=volume.resize",
				want:  []string{"list-vol-resize"},
			},
			{
				name:  "source",
				query: "source=reconciliation",
				want:  []string{"list-vol-recon"},
			},
			{
				name: "cloud, event type, and a window at once",
				query: "cloud=" + listCloudA + "&event_type=volume.create" +
					"&from=" + instant(listAt(0)) + "&to=" + instant(listAt(1)),
				want: []string{"list-vol-create"},
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				list := eventListOf(t, a.call(t, http.MethodGet, eventsRoute+"?"+tc.query, adminToken, nil))

				if got := ids(list.Items); !slices.Equal(got, tc.want) {
					t.Errorf("served events = %v, want %v", got, tc.want)
				}
			})
		}
	})

	t.Run("takes from as inclusive and to as exclusive", func(t *testing.T) {
		// The window starts on the second base event and ends on the fourth, so
		// what the two ends say is read off the bounds themselves.
		query := "from=" + instant(listAt(1)) + "&to=" + instant(listAt(3))

		list := eventListOf(t, a.call(t, http.MethodGet, eventsRoute+"?"+query, adminToken, nil))

		want := []string{"list-vol-resize", "list-inst-create"}
		if got := ids(list.Items); !slices.Equal(got, want) {
			t.Errorf("served events = %v, want %v", got, want)
		}
	})

	t.Run("answers an empty page rather than null items", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			query string
		}{
			{name: "a filter nothing matches", query: "cloud=os-list-nothing"},
			// Each bound is a valid instant on its own, and the window between
			// them is empty, which is an answer rather than a refusal.
			{name: "a from later than the to", query: "from=" + instant(listAt(6)) + "&to=" + instant(listAt(0))},
		} {
			t.Run(tc.name, func(t *testing.T) {
				rec := a.call(t, http.MethodGet, eventsRoute+"?"+tc.query, adminToken, nil)

				if got := rec.Code; got != http.StatusOK {
					t.Fatalf("status = %d, want %d (body %q)", got, http.StatusOK, rec.Body)
				}
				// The raw body rather than the decoded one: a client iterates items
				// without a nil check, which null would break and [] does not.
				want := `{"items":[],"next_cursor":null}`
				if got := strings.TrimSpace(rec.Body.String()); got != want {
					t.Errorf("body = %s, want %s", got, want)
				}
			})
		}
	})

	t.Run("walks a list in pages and ends the walk with a null cursor", func(t *testing.T) {
		const cloud = "os-list-pages"
		_, token := seedIngestCredential(t, a.queries, fixturePlatform, cloud)
		res := fixture{cloud: cloud, resourceType: "volume", id: "vol-pages"}

		items := make([]json.RawMessage, 7)
		for i := range items {
			items[i] = item(t, res.event(pageEventID(i), "volume.update",
				pageAt(i), payloadOf("available", volumeSize(float64(10+i)))))
		}
		if got := ingestResultOf(t, a.call(t, http.MethodPost, eventsRoute, token, batch(t, items...))); got.Accepted != len(items) {
			t.Fatalf("result = %+v, want the %d events of the walk stored", got, len(items))
		}

		whole := eventListOf(t, a.call(t, http.MethodGet, eventsRoute+"?cloud="+cloud, adminToken, nil))
		if len(whole.Items) != len(items) {
			t.Fatalf("the unpaginated answer holds %d events, want %d", len(whole.Items), len(items))
		}

		for _, tc := range []struct {
			name  string
			limit int
			sizes []int
		}{
			// The third, fourth, and fifth event share an instant, so the first
			// page of this walk ends inside that group and the event id of the
			// cursor is what says where the second one resumes.
			{name: "a last page shorter than the limit", limit: 3, sizes: []int{3, 3, 1}},
			// The whole list in one page: the last page is exactly full, which is
			// what tells "the page is full" from "there is more".
			{name: "a last page exactly as long as the limit", limit: len(items), sizes: []int{len(items)}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				page := fmt.Sprintf("%s?cloud=%s&limit=%d", eventsRoute, cloud, tc.limit)

				var walked []StoredEvent
				var sizes []int
				for count := 1; ; count++ {
					if count > len(items) {
						t.Fatalf("the walk had not ended after %d pages, want it over after %d",
							count, len(tc.sizes))
					}
					list := eventListOf(t, a.call(t, http.MethodGet, page, adminToken, nil))

					sizes = append(sizes, len(list.Items))
					walked = append(walked, list.Items...)
					if list.NextCursor == nil {
						break
					}
					page = fmt.Sprintf("%s?cloud=%s&limit=%d&cursor=%s",
						eventsRoute, cloud, tc.limit, url.QueryEscape(*list.NextCursor))
				}

				if !slices.Equal(sizes, tc.sizes) {
					t.Errorf("page sizes = %v, want %v", sizes, tc.sizes)
				}
				if !reflect.DeepEqual(walked, whole.Items) {
					t.Errorf("the walk served %v, want the unpaginated %v", ids(walked), ids(whole.Items))
				}
			})
		}
	})

	t.Run("serves the contract's default limit when the request names none", func(t *testing.T) {
		// The number is spelled out rather than read off defaultPageSize: what is
		// asserted is the `default: 100` of api/reporting/openapi.yaml, which the
		// generated binding does not apply, so a handler drifting from the
		// contract is what this catches.
		const contractDefault = 100
		// One event more than that, in a cloud of its own: what the page holds is
		// the default rather than the whole cloud, and the cursor says the rest is
		// behind it.
		const cloud = "os-list-default-limit"
		_, token := seedIngestCredential(t, a.queries, fixturePlatform, cloud)
		res := fixture{cloud: cloud, resourceType: "volume", id: "vol-default-limit"}

		items := make([]json.RawMessage, contractDefault+1)
		for i := range items {
			items[i] = item(t, res.event(fmt.Sprintf("list-default-%03d", i), "volume.update",
				listAt(30+i), payloadOf("available", volumeSize(float64(10+i)))))
		}
		if got := ingestResultOf(t, a.call(t, http.MethodPost, eventsRoute, token, batch(t, items...))); got.Accepted != len(items) {
			t.Fatalf("result = %+v, want the %d events stored", got, len(items))
		}

		list := eventListOf(t, a.call(t, http.MethodGet, eventsRoute+"?cloud="+cloud, adminToken, nil))

		if got := len(list.Items); got != contractDefault {
			t.Errorf("served events = %d, want the contract's default of %d", got, contractDefault)
		}
		if list.NextCursor == nil {
			t.Error("next_cursor = null, want a cursor on a page the cloud has more events than")
		}
	})

	t.Run("reports a row it cannot decode", func(t *testing.T) {
		// The payload column is typed as an object by the contract, and the write
		// path stores nothing else. A row written past this API could hold any
		// JSON, so the mapper's refusal is only readable on a row that does.
		const cloud = "os-list-undecodable"
		res := fixture{cloud: cloud, resourceType: "volume", id: "vol-undecodable"}
		e := res.event("list-undecodable", "volume.create", listAt(40), event.PayloadEnvelope{})
		insertEventRow(t, a, e, []byte(`[1,2]`))
		// The row stays out of the way of the subtests after this one, which read
		// the whole table and would meet the same refusal.
		t.Cleanup(func() {
			if _, err := a.store.Pool().Exec(context.Background(),
				`DELETE FROM events WHERE event_id = $1`, e.EventID); err != nil {
				t.Fatalf("removing the undecodable row: %v", err)
			}
		})

		rec := a.call(t, http.MethodGet, eventsRoute+"?cloud="+cloud, adminToken, nil)

		assertProblem(t, rec, http.StatusInternalServerError, problem.TypeInternal)
		if got, want := problemDetail(t, rec), "the events could not be read"; got != want {
			t.Errorf("detail = %q, want %q", got, want)
		}
	})

	t.Run("refuses a cursor it did not issue", func(t *testing.T) {
		const at = "2026-06-01T08:00:00Z"

		for _, tc := range []struct {
			name   string
			cursor string
		}{
			{name: "not base64url", cursor: "not base64!"},
			{name: "base64url that is not an array of strings", cursor: rawCursor(`{"at":"now"}`)},
			{name: "one key where the sort key has two", cursor: encodeCursor([]string{at})},
			{name: "three keys where the sort key has two", cursor: encodeCursor([]string{at, "a", "b"})},
			{name: "an empty key", cursor: encodeCursor([]string{at, ""})},
			{name: "a timestamp that is not an instant", cursor: encodeCursor([]string{"yesterday", "list-vol-create"})},
		} {
			t.Run(tc.name, func(t *testing.T) {
				rec := a.call(t, http.MethodGet,
					eventsRoute+"?cursor="+url.QueryEscape(tc.cursor), adminToken, nil)

				assertProblem(t, rec, http.StatusBadRequest, problem.TypeValidation)
				if got, want := problemDetail(t, rec), "the cursor is not one this API issued"; got != want {
					t.Errorf("detail = %q, want %q", got, want)
				}
			})
		}
	})

	t.Run("refuses a parameter the contract does not allow", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			query string
		}{
			{name: "a limit below the minimum", query: "limit=0"},
			{name: "a limit above the maximum", query: "limit=1001"},
			{name: "a from that is not an instant", query: "from=not-a-date"},
			{name: "a source outside the two pipelines", query: "source=banana"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				rec := a.call(t, http.MethodGet, eventsRoute+"?"+tc.query, adminToken, nil)

				assertProblem(t, rec, http.StatusBadRequest, problem.TypeValidation)
			})
		}
	})

	t.Run("confines a project token to the projects it holds", func(t *testing.T) {
		// The same external id in two clouds, and two external ids in one cloud:
		// only the pair the token holds may come back.
		const (
			cloudA = "os-list-scope-a"
			cloudB = "os-list-scope-b"
		)
		held := seedProject(t, a, cloudA, "p-1")
		seedProject(t, a, cloudA, "p-2")
		seedProject(t, a, cloudB, "p-1")
		_, token := seedAPIToken(t, a.queries, auth.RoleProject, []uuid.UUID{held})

		mine := seedScopedEvent(t, a, cloudA, "p-1", "list-scope-mine")
		seedScopedEvent(t, a, cloudA, "p-2", "list-scope-sibling")
		seedScopedEvent(t, a, cloudB, "p-1", "list-scope-other-cloud")

		list := eventListOf(t, a.call(t, http.MethodGet, eventsRoute, token, nil))

		if got, want := ids(list.Items), []string{mine}; !slices.Equal(got, want) {
			t.Errorf("served events = %v, want %v", got, want)
		}

		t.Run("refuses a project outside its scope", func(t *testing.T) {
			rec := a.call(t, http.MethodGet, eventsRoute+"?project_id=p-2", token, nil)

			assertProblem(t, rec, http.StatusForbidden, problem.TypeForbidden)
		})

		t.Run("serves the project it holds", func(t *testing.T) {
			// The other cloud spells the id of a project of its own the same way,
			// so this also says the filter runs on the pair.
			list := eventListOf(t, a.call(t, http.MethodGet, eventsRoute+"?project_id=p-1", token, nil))

			if got, want := ids(list.Items), []string{mine}; !slices.Equal(got, want) {
				t.Errorf("served events = %v, want %v", got, want)
			}
		})
	})

	t.Run("serves a token whose projects are gone an empty page", func(t *testing.T) {
		const cloud = "os-list-gone"
		project := seedProject(t, a, cloud, "p-gone")
		_, token := seedAPIToken(t, a.queries, auth.RoleProject, []uuid.UUID{project})
		stored := seedScopedEvent(t, a, cloud, "p-gone", "list-gone")

		before := eventListOf(t, a.call(t, http.MethodGet, eventsRoute, token, nil))
		if got, want := ids(before.Items), []string{stored}; !slices.Equal(got, want) {
			t.Fatalf("served events = %v, want %v", got, want)
		}

		if _, err := a.store.Pool().Exec(t.Context(), `DELETE FROM projects WHERE id = $1`, project); err != nil {
			t.Fatalf("deleting the project %s: %v", project, err)
		}

		// The scope narrows to nothing rather than widening to everything, so the
		// token reads no event at all instead of every event there is.
		list := eventListOf(t, a.call(t, http.MethodGet, eventsRoute, token, nil))

		if got := ids(list.Items); len(got) != 0 {
			t.Errorf("served events = %v, want none for a token whose projects are gone", got)
		}
	})

	t.Run("refuses a request this API cannot authenticate", func(t *testing.T) {
		unknown, err := auth.GenerateAPIToken()
		if err != nil {
			t.Fatalf("generating the api token: %v", err)
		}
		revokedID, revoked := seedAPIToken(t, a.queries, auth.RoleReadAll, nil)
		if err := a.queries.RevokeAPIToken(t.Context(), revokedID); err != nil {
			t.Fatalf("RevokeAPIToken() error = %v, want nil", err)
		}

		for _, tc := range []struct {
			name  string
			token string
		}{
			{name: "no token", token: ""},
			{name: "an unknown token", token: unknown},
			{name: "a revoked token", token: revoked},
			// The ingest credentials are a store of their own, so a token issued
			// for reporting events reads none of them back.
			{name: "an ingest credential", token: ingestToken},
		} {
			t.Run(tc.name, func(t *testing.T) {
				rec := a.call(t, http.MethodGet, eventsRoute, tc.token, nil)

				assertChallenged(t, rec)
			})
		}
	})

	t.Run("serves a read_all token the whole list", func(t *testing.T) {
		admin := eventListOf(t, a.call(t, http.MethodGet, eventsRoute+"?limit=1000", adminToken, nil))

		list := eventListOf(t, a.call(t, http.MethodGet, eventsRoute+"?limit=1000", readAllToken, nil))

		if got, want := ids(list.Items), ids(admin.Items); !slices.Equal(got, want) {
			t.Errorf("served events = %v, want the %v an admin token reads", got, want)
		}
	})

	t.Run("serves the route without a token while authentication is disabled", func(t *testing.T) {
		// A second router over the same database, which is the development setup:
		// the guard injects an admin principal instead of reading a credential.
		open := newAPIInMode(t, db.Store, auth.ModeDisabled)
		admin := eventListOf(t, a.call(t, http.MethodGet, eventsRoute+"?limit=1000", adminToken, nil))

		list := eventListOf(t, open.call(t, http.MethodGet, eventsRoute+"?limit=1000", "", nil))

		if got, want := ids(list.Items), ids(admin.Items); !slices.Equal(got, want) {
			t.Errorf("served events = %v, want the %v an admin token reads", got, want)
		}
	})
}

// TestListEventsReportsAFailedQuery drives the handler's own failure path. With
// authentication disabled nothing reads the database before the list does, so a
// database that is gone stops the request inside the query rather than at the
// credential lookup in front of it.
func TestListEventsReportsAFailedQuery(t *testing.T) {
	db := storetest.NewDB(t)
	a := newAPIInMode(t, db.Store, auth.ModeDisabled)

	// How long the database gets to shut down cleanly.
	stopTimeout := 10 * time.Second
	if err := db.Container.Stop(t.Context(), &stopTimeout); err != nil {
		t.Fatalf("stopping the database container: %v", err)
	}

	rec := a.call(t, http.MethodGet, eventsRoute, "", nil)

	assertProblem(t, rec, http.StatusInternalServerError, problem.TypeInternal)
	// The detail is what tells the internal problems of this route apart: the
	// query failed, rather than a scope that could not be resolved.
	if got, want := problemDetail(t, rec), "the events could not be read"; got != want {
		t.Errorf("detail = %q, want %q", got, want)
	}

	// The outage was that request's alone: the database comes back and the route
	// serves again. The container returns on a different host port, so everything
	// bound to the old one is rebuilt from the endpoint it has now.
	if err := db.Container.Start(t.Context()); err != nil {
		t.Fatalf("starting the database container again: %v", err)
	}
	back := newAPIInMode(t, reopen(t, db), auth.ModeDisabled)
	waitForReady(t, back.handler, time.Minute)

	list := eventListOf(t, back.call(t, http.MethodGet, eventsRoute, "", nil))
	if got := ids(list.Items); len(got) != 0 {
		t.Errorf("served events = %v, want none in a database nothing was stored in", got)
	}
}

// baseSet is the events the list tests describe: the whole table as the
// unfiltered subtests expect it, in the order the list orders by.
type baseSet []event.Event

// ids is the event ids of the set, in order.
func (b baseSet) ids() []string {
	found := make([]string, len(b))
	for i, e := range b {
		found[i] = e.EventID
	}
	return found
}

// byID is the submitted event of one id, which is what a served event is
// compared against member by member.
func (b baseSet) byID(t *testing.T, eventID string) event.Event {
	t.Helper()

	for _, e := range b {
		if e.EventID == eventID {
			return e
		}
	}
	t.Fatalf("the base set holds no event %q", eventID)
	return event.Event{}
}

// seedBaseEvents stores the events every unfiltered subtest describes and
// returns them in the order the list serves them.
//
// They go in through POST /api/v1/events, so the reads are checked against what
// the write path stores rather than against rows a test wrote itself. Two of them
// cannot come from that path: the pipeline records the source it ran under
// whatever a collector claims, and it marshals an envelope for every item, so
// the reconciliation row and the row without a payload are inserted through the
// pool.
func seedBaseEvents(t *testing.T, a api) baseSet {
	t.Helper()

	volume := fixture{cloud: listCloudA, resourceType: "volume", id: "vol-list"}
	instance := fixture{cloud: listCloudA, resourceType: "instance", id: "inst-list"}
	sibling := fixture{cloud: listCloudB, resourceType: "volume", id: "vol-list-b"}
	widget := fixture{cloud: listCloudC, resourceType: "widget", id: "widget-list"}

	// A payload with three members, so the fidelity check compares more than a
	// state every mapper would keep.
	state := "available"
	created := volume.event("list-vol-create", "volume.create", listAt(0), event.PayloadEnvelope{
		State:    &state,
		Size:     volumeSize(10),
		Provider: map[string]any{"az": "nova-1", "volume_type_id": "8f0e"},
	})
	created.ProjectID = listProjectA

	resized := volume.event("list-vol-resize", "volume.resize", listAt(1),
		payloadOf("available", volumeSize(20)))
	resized.ProjectID = listProjectA

	// The one event of a project of its own, in the cloud the others live in too.
	launched := instance.event("list-inst-create", "instance.create", listAt(2),
		payloadOf("active", map[string]any{"vcpus": 2, "ram_gb": 4, "disk_gb": 20, "flavor": "m1.small"}))
	launched.ProjectID = listProjectB

	elsewhere := sibling.event("list-vol-b-create", "volume.create", listAt(3),
		payloadOf("available", volumeSize(30)))
	elsewhere.ProjectID = listProjectA

	// The one event of a platform of its own.
	built := widget.event("list-widget-create", "widget.create", listAt(4),
		payloadOf("ready", map[string]any{"units": 1}))
	built.Platform = listPlatformC
	built.ProjectID = listProjectA

	for _, submitted := range []struct {
		platform string
		cloud    string
		events   []event.Event
	}{
		{platform: fixturePlatform, cloud: listCloudA, events: []event.Event{created, resized, launched}},
		{platform: fixturePlatform, cloud: listCloudB, events: []event.Event{elsewhere}},
		{platform: listPlatformC, cloud: listCloudC, events: []event.Event{built}},
	} {
		_, token := seedIngestCredential(t, a.queries, submitted.platform, submitted.cloud)
		items := make([]json.RawMessage, len(submitted.events))
		for i, e := range submitted.events {
			items[i] = item(t, e)
		}
		got := ingestResultOf(t, a.call(t, http.MethodPost, eventsRoute, token, batch(t, items...)))
		if got.Accepted != len(submitted.events) {
			t.Fatalf("result = %+v, want the %d events of %s stored",
				got, len(submitted.events), submitted.cloud)
		}
	}

	// A synthetic event of the reconciliation pipeline, which no request can
	// store: the ingest path records the pipeline it ran under itself.
	reconciled := volume.event("list-vol-recon", "volume.update", listAt(5),
		payloadOf("available", nil))
	reconciled.ProjectID = listProjectA
	reconciled.Source = event.SourceReconciliation
	payload, err := json.Marshal(reconciled.Payload)
	if err != nil {
		t.Fatalf("marshaling the payload of %s: %v", reconciled.EventID, err)
	}
	insertEventRow(t, a, reconciled, payload)

	// A delete event whose payload column is NULL, which the ingest path stores
	// for no item either: it marshals at least the empty envelope.
	deleted := volume.event("list-vol-delete", "volume.delete", listAt(6), event.PayloadEnvelope{})
	deleted.ProjectID = listProjectA
	insertEventRow(t, a, deleted, nil)

	return baseSet{created, resized, launched, elsewhere, built, reconciled, deleted}
}

// seedScopedEvent stores one event of a (cloud, project) pair through the ingest
// path and returns its event id. The scope tests care about which pair an event
// belongs to and about nothing else in it.
func seedScopedEvent(t *testing.T, a api, cloud, projectID, eventID string) string {
	t.Helper()

	_, token := seedIngestCredential(t, a.queries, fixturePlatform, cloud)
	res := fixture{cloud: cloud, resourceType: "volume", id: "vol-" + eventID}
	e := res.event(eventID, "volume.create", listAt(20), payloadOf("available", volumeSize(10)))
	e.ProjectID = projectID

	if got := ingestResultOf(t, a.call(t, http.MethodPost, eventsRoute, token, item(t, e))); got.Accepted != 1 {
		t.Fatalf("result = %+v, want the event of (%s, %s) stored", got, cloud, projectID)
	}
	return eventID
}

// insertEventRow writes one events row through the pool, with the source the
// event names and the payload as it is given, NULL included. It exists for the
// two states the ingest path cannot produce and is used for nothing else.
func insertEventRow(t *testing.T, a api, e event.Event, payload []byte) {
	t.Helper()

	if _, err := a.store.Pool().Exec(t.Context(),
		`INSERT INTO events (event_id, timestamp, event_type, platform, cloud,
		                     resource_type, resource_id, project_id, source, payload)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		e.EventID, e.Timestamp, e.EventType, e.Platform, e.Cloud,
		e.ResourceType, e.ResourceID, e.ProjectID, string(e.Source), payload); err != nil {
		t.Fatalf("inserting the event %s: %v", e.EventID, err)
	}
}

// seedProject registers one project and returns its registry id, which is what a
// project token is scoped by.
func seedProject(t *testing.T, a api, cloud, externalID string) uuid.UUID {
	t.Helper()

	var id uuid.UUID
	if err := a.store.Pool().QueryRow(t.Context(),
		`INSERT INTO projects (platform, cloud, external_id) VALUES ('openstack', $1, $2) RETURNING id`,
		cloud, externalID).Scan(&id); err != nil {
		t.Fatalf("inserting the project (%s, %s): %v", cloud, externalID, err)
	}
	return id
}

// eventListOf decodes the answer of a list call, which the contract promises is
// a 200 carrying one page.
func eventListOf(t *testing.T, rec *httptest.ResponseRecorder) EventList {
	t.Helper()

	if got := rec.Code; got != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %q)", got, http.StatusOK, rec.Body)
	}
	if got, want := rec.Header().Get("Content-Type"), "application/json"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}

	var list EventList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decoding the body %q: %v", rec.Body.String(), err)
	}
	if list.Items == nil {
		t.Errorf("body %q carries items as null, want an array", rec.Body)
	}
	return list
}

// assertServedEvent checks one served event against the event that was
// submitted, member by member, plus the payload document its row holds.
func assertServedEvent(t *testing.T, got StoredEvent, want event.Event, wantPayload map[string]any) {
	t.Helper()

	if got.EventId != want.EventID {
		t.Errorf("event_id = %q, want %q", got.EventId, want.EventID)
	}
	if !got.Timestamp.Equal(want.Timestamp) {
		t.Errorf("timestamp of %s = %v, want %v", want.EventID, got.Timestamp, want.Timestamp)
	}
	if got.EventType != want.EventType {
		t.Errorf("event_type of %s = %q, want %q", want.EventID, got.EventType, want.EventType)
	}
	if got.Platform != want.Platform {
		t.Errorf("platform of %s = %q, want %q", want.EventID, got.Platform, want.Platform)
	}
	if got.Cloud != want.Cloud {
		t.Errorf("cloud of %s = %q, want %q", want.EventID, got.Cloud, want.Cloud)
	}
	if got.ResourceType != want.ResourceType {
		t.Errorf("resource_type of %s = %q, want %q", want.EventID, got.ResourceType, want.ResourceType)
	}
	if got.ResourceId != want.ResourceID {
		t.Errorf("resource_id of %s = %q, want %q", want.EventID, got.ResourceId, want.ResourceID)
	}
	if got.ProjectId != want.ProjectID {
		t.Errorf("project_id of %s = %q, want %q", want.EventID, got.ProjectId, want.ProjectID)
	}
	if got.Source != string(want.Source) {
		t.Errorf("source of %s = %q, want %q", want.EventID, got.Source, want.Source)
	}
	// received_at is the server's own, so what is asserted is that it is there.
	if got.ReceivedAt.IsZero() {
		t.Errorf("received_at of %s is unset, want the instant the row was stored", want.EventID)
	}

	switch {
	case wantPayload == nil:
		if got.Payload != nil {
			t.Errorf("payload of %s = %v, want null", want.EventID, *got.Payload)
		}
	case got.Payload == nil:
		t.Errorf("payload of %s = null, want %v", want.EventID, wantPayload)
	case !reflect.DeepEqual(*got.Payload, wantPayload):
		t.Errorf("payload of %s = %v, want the stored %v", want.EventID, *got.Payload, wantPayload)
	}
}

// storedPayload is the payload column of one event as the database holds it, and
// nil for a row holding none.
func storedPayload(t *testing.T, a api, eventID string) map[string]any {
	t.Helper()

	var payload map[string]any
	if err := a.store.Pool().QueryRow(t.Context(),
		`SELECT payload FROM events WHERE event_id = $1`, eventID).Scan(&payload); err != nil {
		t.Fatalf("reading the payload of event %s: %v", eventID, err)
	}
	return payload
}

// ids is the event ids of a served page, in order. The order assertions compare
// these rather than whole events, so a failure names what came back.
func ids(items []StoredEvent) []string {
	found := make([]string, len(items))
	for i, item := range items {
		found[i] = item.EventId
	}
	return found
}

// pageEventID is the id of the nth event of the paginated walk. The ids sort the
// way the timestamps do, so a page boundary is readable in a failure message.
func pageEventID(n int) string {
	return "list-page-" + string(rune('a'+n))
}

// pageAt is the instant of the nth event of the paginated walk.
//
// The third, fourth, and fifth share one instant, which is the shape a collector
// batch has: every item of one resource change carries the same timestamp. With
// a limit of three that group straddles a page boundary, so the walk only stays
// whole if the event id decides where the next page resumes.
//
// That instant also carries a fraction of a second, which received_at and a
// reconciliation run both produce. It is what the cursor's RFC3339Nano round
// trip is readable on: a cursor cut to whole seconds would resume before the
// event it was issued for and serve it twice.
func pageAt(n int) time.Time {
	if n >= 2 && n <= 4 {
		return listAt(12).Add(500 * time.Microsecond)
	}
	return listAt(10 + n)
}

// instant renders one bound of the time window the way the contract asks for it,
// escaped for the query string it travels in.
func instant(at time.Time) string {
	return url.QueryEscape(at.Format(time.RFC3339))
}

// rawCursor is a cursor carrying bytes of the test's own choosing: base64url, so
// that it gets past the decoder, and whatever the test wants inside.
func rawCursor(payload string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(payload))
}
