package httpapi

import (
	"context"
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
	"github.com/b42labs/tally/internal/core/timeline"
	"github.com/b42labs/tally/internal/reporting/auth"
	"github.com/b42labs/tally/internal/reporting/httpapi/problem"
	"github.com/b42labs/tally/internal/reporting/store/storetest"
)

// resourcesRoute is the route the resource list tests call, spelled once because
// every one of them addresses it.
const resourcesRoute = "/api/v1/resources"

// The clouds the base fleet lives in, and the platform of the third. Every
// subtest that seeds resources of its own works on a cloud of its own, so the
// base fleet stays what the subtests describing it expect.
const (
	fleetCloudA    = "os-fleet-a"
	fleetCloudB    = "os-fleet-b"
	fleetCloudC    = "tally-fleet-c"
	fleetPlatformC = "tallytest"
)

// fleetProjectB is the project of the one base-fleet resource that does not
// belong to the fixture's own project, in the cloud the others live in too. It
// is what makes the project filter more than another spelling of the cloud
// filter.
const fleetProjectB = "fleet-project-b"

// The instants the resource fixtures use: a create, a change, and a delete two
// hours apart on whole seconds, so what Postgres stores compares equal to what
// the test passed in.
var (
	fleetCreated = time.Date(2026, 7, 1, 6, 0, 0, 0, time.UTC)
	fleetChanged = fleetCreated.Add(time.Hour)
	fleetRemoved = fleetCreated.Add(2 * time.Hour)
)

// TestListResourcesOverHTTP drives GET /api/v1/resources through the whole
// stack: the contract validator, the query guard, the project scope, and the
// projection rows behind them.
//
// The subtests share one database and run in order: the ones describing the base
// fleet come first, and the ones seeding resources of their own afterwards work
// on clouds those assertions do not name.
func TestListResourcesOverHTTP(t *testing.T) {
	db := storetest.NewDB(t)
	a := newAPI(t, db.Store)

	_, adminToken := seedAPIToken(t, a.queries, auth.RoleAdmin, nil)
	_, readAllToken := seedAPIToken(t, a.queries, auth.RoleReadAll, nil)

	seedFleet(t, a)

	t.Run("serves the rows of a cloud in cloud, type, and id order", func(t *testing.T) {
		// status=all, because the fleet holds a deleted resource and the default
		// leaves it out, which the status subtests below are about.
		list := resourceListOf(t, a.call(t, http.MethodGet,
			resourcesRoute+"?cloud="+fleetCloudA+"&status=all", adminToken, nil))

		if list.NextCursor != nil {
			t.Errorf("next_cursor = %q, want null on a page that holds everything", *list.NextCursor)
		}
		// The resource type sorts before the resource id, so the instance leads
		// the volumes however their ids compare.
		want := []string{"inst-off", "vol-alive", "vol-gone", "vol-orphan", "vol-project-b"}
		if got := resourceIDs(list.Items); !slices.Equal(got, want) {
			t.Errorf("served resources = %v, want %v", got, want)
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
				query: "cloud=" + fleetCloudA + "&status=all",
				want:  []string{"inst-off", "vol-alive", "vol-gone", "vol-orphan", "vol-project-b"},
			},
			{
				name:  "platform",
				query: "platform=" + fleetPlatformC,
				want:  []string{"widget-c"},
			},
			{
				name:  "project_id",
				query: "project_id=" + fleetProjectB,
				want:  []string{"vol-project-b"},
			},
			{
				name:  "resource_type",
				query: "resource_type=instance",
				want:  []string{"inst-off"},
			},
			{
				// The state is matched exactly: the instance was powered off, and
				// nothing else in the fleet reports that state.
				name:  "state",
				query: "state=shutoff",
				want:  []string{"inst-off"},
			},
			{
				name:  "cloud and resource type at once",
				query: "cloud=" + fleetCloudB + "&resource_type=volume",
				want:  []string{"vol-elsewhere"},
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				list := resourceListOf(t, a.call(t, http.MethodGet,
					resourcesRoute+"?"+tc.query, adminToken, nil))

				if got := resourceIDs(list.Items); !slices.Equal(got, tc.want) {
					t.Errorf("served resources = %v, want %v", got, tc.want)
				}
			})
		}
	})

	t.Run("serves the part of the fleet the status names", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			query string
			want  []string
		}{
			{
				// No status at all: the contract's default is active, and the
				// deleted volume is what says it was applied.
				name:  "no status",
				query: "",
				want:  []string{"inst-off", "vol-alive", "vol-orphan", "vol-project-b"},
			},
			{name: "status=active", query: "&status=active", want: []string{"inst-off", "vol-alive", "vol-orphan", "vol-project-b"}},
			{name: "status=deleted", query: "&status=deleted", want: []string{"vol-gone"}},
			{
				name:  "status=all",
				query: "&status=all",
				want:  []string{"inst-off", "vol-alive", "vol-gone", "vol-orphan", "vol-project-b"},
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				list := resourceListOf(t, a.call(t, http.MethodGet,
					resourcesRoute+"?cloud="+fleetCloudA+tc.query, adminToken, nil))

				if got := resourceIDs(list.Items); !slices.Equal(got, tc.want) {
					t.Errorf("served resources = %v, want %v", got, tc.want)
				}
			})
		}
	})

	t.Run("answers an empty page rather than null items", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			query string
		}{
			{name: "a filter nothing matches", query: "cloud=os-fleet-nothing"},
			// The two filters are independent, and no row can be in a live state
			// and deleted at once, so the contradiction is an answer rather than a
			// refusal.
			{name: "a live state together with status=deleted", query: "state=available&status=deleted"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				rec := a.call(t, http.MethodGet, resourcesRoute+"?"+tc.query, adminToken, nil)

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

	t.Run("serves the instants a history implies", func(t *testing.T) {
		list := resourceListOf(t, a.call(t, http.MethodGet,
			resourcesRoute+"?cloud="+fleetCloudA+"&status=all", adminToken, nil))

		t.Run("created_at is set and deleted_at null while the resource lives", func(t *testing.T) {
			alive := byResourceID(t, list.Items, "vol-alive")

			if alive.CreatedAt == nil {
				t.Errorf("created_at = null, want %v", fleetCreated)
			} else if !alive.CreatedAt.Equal(fleetCreated) {
				t.Errorf("created_at = %v, want %v", alive.CreatedAt, fleetCreated)
			}
			if alive.DeletedAt != nil {
				t.Errorf("deleted_at = %v, want null while the resource lives", *alive.DeletedAt)
			}
		})

		t.Run("deleted_at and the deleted state are set once it is gone", func(t *testing.T) {
			gone := byResourceID(t, list.Items, "vol-gone")

			if gone.DeletedAt == nil {
				t.Errorf("deleted_at = null, want %v", fleetRemoved)
			} else if !gone.DeletedAt.Equal(fleetRemoved) {
				t.Errorf("deleted_at = %v, want %v", gone.DeletedAt, fleetRemoved)
			}
			if got, want := gone.State, "deleted"; got != want {
				t.Errorf("state = %q, want %q", got, want)
			}
		})

		t.Run("created_at is null for a history that never showed a create", func(t *testing.T) {
			orphan := byResourceID(t, list.Items, "vol-orphan")

			if orphan.CreatedAt != nil {
				t.Errorf("created_at = %v, want null for a history that starts with an update",
					*orphan.CreatedAt)
			}
		})
	})

	t.Run("serves every member of a projection row", func(t *testing.T) {
		const cloud = "os-fleet-members"
		res := fixture{cloud: cloud, resourceType: "volume", id: "vol-members"}
		created := res.event("vol-members-create", "volume.create", fleetCreated,
			payloadOf("available", volumeSize(10)))
		resized := res.event("vol-members-resize", "volume.resize", fleetChanged,
			payloadOf("in-use", volumeSize(20)))
		ingestEvents(t, a, fixturePlatform, cloud, created, resized)

		list := resourceListOf(t, a.call(t, http.MethodGet, resourcesRoute+"?cloud="+cloud, adminToken, nil))

		if len(list.Items) != 1 {
			t.Fatalf("served resources = %v, want the one of %s", resourceIDs(list.Items), cloud)
		}
		got := list.Items[0]
		if got.Cloud != cloud {
			t.Errorf("cloud = %q, want %q", got.Cloud, cloud)
		}
		if got.Platform != fixturePlatform {
			t.Errorf("platform = %q, want %q", got.Platform, fixturePlatform)
		}
		if got.ResourceType != res.resourceType {
			t.Errorf("resource_type = %q, want %q", got.ResourceType, res.resourceType)
		}
		if got.ResourceId != res.id {
			t.Errorf("resource_id = %q, want %q", got.ResourceId, res.id)
		}
		if got.ProjectId != fixtureProject {
			t.Errorf("project_id = %q, want %q", got.ProjectId, fixtureProject)
		}
		// The state and the size come from the newer of the two events, which is
		// what says the row was folded rather than taken from the create.
		if want := "in-use"; got.State != want {
			t.Errorf("state = %q, want %q", got.State, want)
		}
		if want := asDocument(t, volumeSize(20)); !reflect.DeepEqual(got.Size, want) {
			t.Errorf("size = %v, want %v", got.Size, want)
		}
		if got.CreatedAt == nil || !got.CreatedAt.Equal(fleetCreated) {
			t.Errorf("created_at = %v, want %v", got.CreatedAt, fleetCreated)
		}
		if got.DeletedAt != nil {
			t.Errorf("deleted_at = %v, want null", *got.DeletedAt)
		}
		if got.LastEventType != resized.EventType {
			t.Errorf("last_event_type = %q, want %q", got.LastEventType, resized.EventType)
		}
		if !got.LastEventAt.Equal(fleetChanged) {
			t.Errorf("last_event_at = %v, want %v", got.LastEventAt, fleetChanged)
		}
		if got.LastPayload == nil {
			t.Fatalf("last_payload = null, want the envelope of %s", resized.EventID)
		}
		if want := asDocument(t, resized.Payload); !reflect.DeepEqual(*got.LastPayload, want) {
			t.Errorf("last_payload = %v, want the stored %v", *got.LastPayload, want)
		}
	})

	t.Run("serves a row whose last payload is NULL as a null member", func(t *testing.T) {
		const cloud = "os-fleet-payloadless"
		res := fixture{cloud: cloud, resourceType: "volume", id: "vol-payloadless"}
		ingestEvents(t, a, fixturePlatform, cloud,
			res.event("vol-payloadless-create", "volume.create", fleetCreated,
				payloadOf("available", volumeSize(10))))

		// The column is nullable, and the write path never leaves it NULL: it
		// marshals an envelope for every event it folds. What the mapper does with
		// a NULL is only readable on a row that carries one, so the row is put
		// into that state through the pool.
		if _, err := a.store.Pool().Exec(t.Context(),
			`UPDATE current_resources SET last_payload = NULL WHERE cloud = $1`, cloud); err != nil {
			t.Fatalf("clearing the last payload of %s: %v", cloud, err)
		}

		rec := a.call(t, http.MethodGet, resourcesRoute+"?cloud="+cloud, adminToken, nil)

		list := resourceListOf(t, rec)
		if len(list.Items) != 1 {
			t.Fatalf("served resources = %v, want the one of %s", resourceIDs(list.Items), cloud)
		}
		if list.Items[0].LastPayload != nil {
			t.Errorf("last_payload = %v, want it served as null", *list.Items[0].LastPayload)
		}
		// A pointer that decoded to nil would also come from a member left out, so
		// the raw body is what says the member is there and null.
		if body := rec.Body.String(); !strings.Contains(body, `"last_payload":null`) {
			t.Errorf("body = %s, want it to carry \"last_payload\":null", body)
		}
	})

	t.Run("walks a list in pages and ends the walk with a null cursor", func(t *testing.T) {
		const cloud = "os-fleet-pages"
		events := make([]event.Event, 7)
		for i := range events {
			res := fixture{cloud: cloud, resourceType: "volume", id: fmt.Sprintf("vol-page-%d", i)}
			events[i] = res.event(fmt.Sprintf("vol-page-%d-create", i), "volume.create",
				fleetCreated, payloadOf("available", volumeSize(float64(10+i))))
		}
		ingestEvents(t, a, fixturePlatform, cloud, events...)

		whole := resourceListOf(t, a.call(t, http.MethodGet, resourcesRoute+"?cloud="+cloud, adminToken, nil))
		if len(whole.Items) != len(events) {
			t.Fatalf("the unpaginated answer holds %d resources, want %d", len(whole.Items), len(events))
		}

		for _, tc := range []struct {
			name  string
			limit int
			sizes []int
		}{
			{name: "a last page shorter than the limit", limit: 3, sizes: []int{3, 3, 1}},
			// The whole fleet in one page: the last page is exactly full, which is
			// what tells "the page is full" from "there is more".
			{name: "a last page exactly as long as the limit", limit: len(events), sizes: []int{len(events)}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				page := fmt.Sprintf("%s?cloud=%s&limit=%d", resourcesRoute, cloud, tc.limit)

				var walked []Resource
				var sizes []int
				for count := 1; ; count++ {
					if count > len(events) {
						t.Fatalf("the walk had not ended after %d pages, want it over after %d",
							count, len(tc.sizes))
					}
					list := resourceListOf(t, a.call(t, http.MethodGet, page, adminToken, nil))

					sizes = append(sizes, len(list.Items))
					walked = append(walked, list.Items...)
					if list.NextCursor == nil {
						break
					}
					page = fmt.Sprintf("%s?cloud=%s&limit=%d&cursor=%s",
						resourcesRoute, cloud, tc.limit, url.QueryEscape(*list.NextCursor))
				}

				if !slices.Equal(sizes, tc.sizes) {
					t.Errorf("page sizes = %v, want %v", sizes, tc.sizes)
				}
				if !reflect.DeepEqual(walked, whole.Items) {
					t.Errorf("the walk served %v, want the unpaginated %v",
						resourceIDs(walked), resourceIDs(whole.Items))
				}
			})
		}
	})

	t.Run("reports a row it cannot decode", func(t *testing.T) {
		// The size column is typed as an object by the contract, and the write
		// path stores nothing else. A row written past this API could hold any
		// JSON, so the mapper's refusal is only readable on a row that does.
		const cloud = "os-fleet-undecodable"
		res := fixture{cloud: cloud, resourceType: "volume", id: "vol-undecodable"}
		ingestEvents(t, a, fixturePlatform, cloud,
			res.event("vol-undecodable-create", "volume.create", fleetCreated,
				payloadOf("available", volumeSize(10))))
		if _, err := a.store.Pool().Exec(t.Context(),
			`UPDATE current_resources SET size = '"x"'::jsonb WHERE cloud = $1`, cloud); err != nil {
			t.Fatalf("writing an undecodable size into %s: %v", cloud, err)
		}
		// The row stays out of the way of the subtests after this one, which read
		// the whole projection and would meet the same refusal.
		t.Cleanup(func() {
			if _, err := a.store.Pool().Exec(context.Background(),
				`DELETE FROM current_resources WHERE cloud = $1`, cloud); err != nil {
				t.Fatalf("removing the undecodable row: %v", err)
			}
		})

		rec := a.call(t, http.MethodGet, resourcesRoute+"?cloud="+cloud, adminToken, nil)

		assertProblem(t, rec, http.StatusInternalServerError, problem.TypeInternal)
		if got, want := problemDetail(t, rec), "the resources could not be read"; got != want {
			t.Errorf("detail = %q, want %q", got, want)
		}
	})

	t.Run("refuses a cursor it did not issue", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			cursor string
		}{
			{name: "not base64url", cursor: "not base64!"},
			// The sort key of this list has three parts, so a cursor of the events
			// list resumes nothing here.
			{name: "two keys where the sort key has three", cursor: encodeCursor([]string{fleetCloudA, "volume"})},
		} {
			t.Run(tc.name, func(t *testing.T) {
				rec := a.call(t, http.MethodGet,
					resourcesRoute+"?cursor="+url.QueryEscape(tc.cursor), adminToken, nil)

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
			{name: "a status outside the three", query: "status=banana"},
			{name: "a limit above the maximum", query: "limit=1001"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				rec := a.call(t, http.MethodGet, resourcesRoute+"?"+tc.query, adminToken, nil)

				assertProblem(t, rec, http.StatusBadRequest, problem.TypeValidation)
			})
		}
	})

	t.Run("confines a project token to the projects it holds", func(t *testing.T) {
		// The same external id in two clouds, and two external ids in one cloud:
		// only the pair the token holds may come back.
		const (
			cloudA = "os-fleet-scope-a"
			cloudB = "os-fleet-scope-b"
		)
		held := seedProject(t, a, cloudA, "p-1")
		seedProject(t, a, cloudA, "p-2")
		seedProject(t, a, cloudB, "p-1")
		_, token := seedAPIToken(t, a.queries, auth.RoleProject, []uuid.UUID{held})

		mine := seedScopedResource(t, a, cloudA, "p-1", "vol-scope-mine")
		seedScopedResource(t, a, cloudA, "p-2", "vol-scope-sibling")
		seedScopedResource(t, a, cloudB, "p-1", "vol-scope-other-cloud")

		list := resourceListOf(t, a.call(t, http.MethodGet, resourcesRoute, token, nil))

		if got, want := resourceIDs(list.Items), []string{mine}; !slices.Equal(got, want) {
			t.Errorf("served resources = %v, want %v", got, want)
		}

		t.Run("refuses a project outside its scope", func(t *testing.T) {
			rec := a.call(t, http.MethodGet, resourcesRoute+"?project_id=p-2", token, nil)

			assertProblem(t, rec, http.StatusForbidden, problem.TypeForbidden)
		})

		t.Run("serves the project it holds", func(t *testing.T) {
			// The other cloud spells the id of a project of its own the same way,
			// so this also says the filter runs on the pair.
			list := resourceListOf(t, a.call(t, http.MethodGet, resourcesRoute+"?project_id=p-1", token, nil))

			if got, want := resourceIDs(list.Items), []string{mine}; !slices.Equal(got, want) {
				t.Errorf("served resources = %v, want %v", got, want)
			}
		})
	})

	t.Run("serves a token whose projects are gone an empty page", func(t *testing.T) {
		const cloud = "os-fleet-orphaned-token"
		project := seedProject(t, a, cloud, "p-orphaned")
		_, token := seedAPIToken(t, a.queries, auth.RoleProject, []uuid.UUID{project})
		stored := seedScopedResource(t, a, cloud, "p-orphaned", "vol-orphaned-token")

		before := resourceListOf(t, a.call(t, http.MethodGet, resourcesRoute, token, nil))
		if got, want := resourceIDs(before.Items), []string{stored}; !slices.Equal(got, want) {
			t.Fatalf("served resources = %v, want %v", got, want)
		}

		if _, err := a.store.Pool().Exec(t.Context(), `DELETE FROM projects WHERE id = $1`, project); err != nil {
			t.Fatalf("deleting the project %s: %v", project, err)
		}

		// The scope narrows to nothing rather than widening to everything, so the
		// token reads no resource at all instead of every resource there is.
		list := resourceListOf(t, a.call(t, http.MethodGet, resourcesRoute, token, nil))

		if got := resourceIDs(list.Items); len(got) != 0 {
			t.Errorf("served resources = %v, want none for a token whose projects are gone", got)
		}
	})

	t.Run("refuses a request this API cannot authenticate", func(t *testing.T) {
		rec := a.call(t, http.MethodGet, resourcesRoute, "", nil)

		assertChallenged(t, rec)
	})

	t.Run("serves a read_all token the whole list", func(t *testing.T) {
		admin := resourceListOf(t, a.call(t, http.MethodGet, resourcesRoute+"?limit=1000&status=all", adminToken, nil))

		list := resourceListOf(t, a.call(t, http.MethodGet, resourcesRoute+"?limit=1000&status=all", readAllToken, nil))

		if got, want := resourceIDs(list.Items), resourceIDs(admin.Items); !slices.Equal(got, want) {
			t.Errorf("served resources = %v, want the %v an admin token reads", got, want)
		}
	})

	t.Run("serves the route without a token while authentication is disabled", func(t *testing.T) {
		// A second router over the same database, which is the development setup:
		// the guard injects an admin principal instead of reading a credential.
		open := newAPIInMode(t, db.Store, auth.ModeDisabled)
		admin := resourceListOf(t, a.call(t, http.MethodGet, resourcesRoute+"?limit=1000&status=all", adminToken, nil))

		list := resourceListOf(t, open.call(t, http.MethodGet, resourcesRoute+"?limit=1000&status=all", "", nil))

		if got, want := resourceIDs(list.Items), resourceIDs(admin.Items); !slices.Equal(got, want) {
			t.Errorf("served resources = %v, want the %v an admin token reads", got, want)
		}
	})
}

// TestReadResourceHistoryOverHTTP drives
// GET /api/v1/resources/{cloud}/{resource_type}/{resource_id}/events: the whole
// history of one resource, in the order the fold reads it in and never paged.
func TestReadResourceHistoryOverHTTP(t *testing.T) {
	db := storetest.NewDB(t)
	a := newAPI(t, db.Store)

	_, adminToken := seedAPIToken(t, a.queries, auth.RoleAdmin, nil)
	_, readAllToken := seedAPIToken(t, a.queries, auth.RoleReadAll, nil)

	const cloud = "os-history"
	alive := fixture{cloud: cloud, resourceType: "volume", id: "vol-history"}
	gone := fixture{cloud: cloud, resourceType: "volume", id: "vol-history-gone"}
	// A second resource in the same cloud, so the answer being one resource's
	// history rather than the cloud's is readable off it.
	ingestEvents(t, a, fixturePlatform, cloud,
		alive.event("vol-history-create", "volume.create", fleetCreated,
			payloadOf("available", volumeSize(10))),
		alive.event("vol-history-resize", "volume.resize", fleetChanged,
			payloadOf("available", volumeSize(20))),
		alive.event("vol-history-attach", "volume.update", fleetRemoved,
			payloadOf("in-use", nil)),
		gone.event("vol-history-gone-create", "volume.create", fleetCreated,
			payloadOf("available", volumeSize(30))),
		gone.event("vol-history-gone-delete", "volume.delete", fleetRemoved,
			event.PayloadEnvelope{}))

	t.Run("serves the full history in timestamp order with a null cursor", func(t *testing.T) {
		rec := a.call(t, http.MethodGet,
			resourceEventsPath(cloud, alive.resourceType, alive.id), adminToken, nil)

		list := eventListOf(t, rec)
		if list.NextCursor != nil {
			t.Errorf("next_cursor = %q, want null on an answer that is never paged", *list.NextCursor)
		}
		want := []string{"vol-history-create", "vol-history-resize", "vol-history-attach"}
		if got := ids(list.Items); !slices.Equal(got, want) {
			t.Errorf("served events = %v, want %v", got, want)
		}
	})

	t.Run("serves the history of a deleted resource", func(t *testing.T) {
		// The projection keeps the row of a deleted resource forever, so its
		// history stays readable after the delete.
		list := eventListOf(t, a.call(t, http.MethodGet,
			resourceEventsPath(cloud, gone.resourceType, gone.id), adminToken, nil))

		want := []string{"vol-history-gone-create", "vol-history-gone-delete"}
		if got := ids(list.Items); !slices.Equal(got, want) {
			t.Errorf("served events = %v, want %v", got, want)
		}
	})

	t.Run("serves a read_all token the history", func(t *testing.T) {
		list := eventListOf(t, a.call(t, http.MethodGet,
			resourceEventsPath(cloud, alive.resourceType, alive.id), readAllToken, nil))

		if got, want := len(list.Items), 3; got != want {
			t.Errorf("served events = %d, want %d", got, want)
		}
	})

	t.Run("refuses a history longer than the unpaginated reads serve", func(t *testing.T) {
		// One event past the bound, written through the pool rather than ingested:
		// what is being pinned is the handler's refusal, and folding ten thousand
		// events through the write path would say nothing more about it.
		//
		// The bulk carries a project of its own, so that the resource's history and
		// the slice of it a project token reads differ in length. Only the create is
		// the fixture project's, which is therefore the pair the projection row
		// carries and the one a project token is let past the gate on.
		const (
			longCloud = "os-history-long"
			longBulk  = "project-history-long-bulk"
		)
		long := fixture{cloud: longCloud, resourceType: "volume", id: "vol-history-long"}
		ingestEvents(t, a, fixturePlatform, longCloud,
			long.event("vol-history-long-create", "volume.create", fleetCreated,
				payloadOf("available", volumeSize(10))))
		if _, err := a.store.Pool().Exec(t.Context(),
			`INSERT INTO events (event_id, timestamp, event_type, platform, cloud,
			                     resource_type, resource_id, project_id, source, payload)
			 SELECT 'vol-history-long-' || n, $1::timestamptz + (n * INTERVAL '1 second'),
			        'volume.update', $2, $3, $4, $5, $6, 'collector', '{"state": "available"}'::jsonb
			 FROM generate_series(1, $7::int) AS n`,
			fleetCreated, fixturePlatform, longCloud, long.resourceType, long.id, longBulk,
			maxResourceHistory); err != nil {
			t.Fatalf("writing a history past the bound: %v", err)
		}

		for name, path := range map[string]string{
			"the history":   resourceEventsPath(longCloud, long.resourceType, long.id),
			"the lifecycle": resourceLifecyclePath(longCloud, long.resourceType, long.id),
		} {
			t.Run(name, func(t *testing.T) {
				rec := a.call(t, http.MethodGet, path, adminToken, nil)

				assertProblem(t, rec, http.StatusUnprocessableEntity, problem.TypeHistoryTooLong)
			})
		}

		t.Run("measures the bound against the history the token reads", func(t *testing.T) {
			// The refusal is decided before the payloads are fetched, and what it is
			// decided on has to be the scoped history rather than the resource's: a
			// token that reads one event of this resource is served it, though the
			// resource has one event more than the bound.
			project := seedProject(t, a, longCloud, fixtureProject)
			_, token := seedAPIToken(t, a.queries, auth.RoleProject, []uuid.UUID{project})

			list := eventListOf(t, a.call(t, http.MethodGet,
				resourceEventsPath(longCloud, long.resourceType, long.id), token, nil))

			if got, want := ids(list.Items), []string{"vol-history-long-create"}; !slices.Equal(got, want) {
				t.Errorf("served events = %v, want the %v this token's scope reaches", got, want)
			}
		})

		t.Run("serves a history exactly as long as the bound", func(t *testing.T) {
			// The row one past the bound is what the refusal is decided on, so the
			// history that fills it exactly is the boundary the answer still holds.
			if _, err := a.store.Pool().Exec(t.Context(),
				`DELETE FROM events WHERE event_id = $1`,
				fmt.Sprintf("vol-history-long-%d", maxResourceHistory)); err != nil {
				t.Fatalf("cutting the history back to the bound: %v", err)
			}

			list := eventListOf(t, a.call(t, http.MethodGet,
				resourceEventsPath(longCloud, long.resourceType, long.id), adminToken, nil))

			if got := len(list.Items); got != maxResourceHistory {
				t.Errorf("served events = %d, want the %d the bound allows", got, maxResourceHistory)
			}
		})
	})

	t.Run("reports a row it cannot decode", func(t *testing.T) {
		const badCloud = "os-history-undecodable"
		bad := fixture{cloud: badCloud, resourceType: "volume", id: "vol-history-undecodable"}
		ingestEvents(t, a, fixturePlatform, badCloud,
			bad.event("vol-history-undecodable-create", "volume.create", fleetCreated,
				payloadOf("available", volumeSize(10))))
		// The payload column is typed as an object by the contract, and the write
		// path stores nothing else. A row written past this API could hold any
		// JSON, so the mapper's refusal is only readable on a row that does.
		insertEventRow(t, a, bad.event("vol-history-undecodable-update", "volume.update",
			fleetChanged, event.PayloadEnvelope{}), []byte(`[1,2]`))

		for name, tc := range map[string]struct {
			path   string
			detail string
		}{
			"the history": {
				path:   resourceEventsPath(badCloud, bad.resourceType, bad.id),
				detail: "the resource history could not be read",
			},
			"the lifecycle": {
				path:   resourceLifecyclePath(badCloud, bad.resourceType, bad.id),
				detail: "the lifecycle could not be read",
			},
		} {
			t.Run(name, func(t *testing.T) {
				rec := a.call(t, http.MethodGet, tc.path, adminToken, nil)

				assertProblem(t, rec, http.StatusInternalServerError, problem.TypeInternal)
				if got := problemDetail(t, rec); got != tc.detail {
					t.Errorf("detail = %q, want %q", got, tc.detail)
				}
			})
		}
	})

	t.Run("refuses a request this API cannot authenticate", func(t *testing.T) {
		rec := a.call(t, http.MethodGet,
			resourceEventsPath(cloud, alive.resourceType, alive.id), "", nil)

		assertChallenged(t, rec)
	})

	t.Run("serves the route without a token while authentication is disabled", func(t *testing.T) {
		open := newAPIInMode(t, db.Store, auth.ModeDisabled)

		list := eventListOf(t, open.call(t, http.MethodGet,
			resourceEventsPath(cloud, alive.resourceType, alive.id), "", nil))

		if got, want := len(list.Items), 3; got != want {
			t.Errorf("served events = %d, want %d", got, want)
		}
	})
}

// TestResourceLifecycleOverHTTP drives
// GET /api/v1/resources/{cloud}/{resource_type}/{resource_id}/lifecycle: the
// fold of one resource's history into the intervals it implies.
func TestResourceLifecycleOverHTTP(t *testing.T) {
	db := storetest.NewDB(t)
	a := newAPI(t, db.Store)

	_, adminToken := seedAPIToken(t, a.queries, auth.RoleAdmin, nil)

	t.Run("folds the resize example into two intervals", func(t *testing.T) {
		// The concept's Example 1, as roadmap/01-phase-1-core-platform-openstack.md
		// states it: an instance created on the first of March and resized halfway
		// through the month.
		const cloud = "os-lifecycle-resize"
		res := fixture{cloud: cloud, resourceType: "instance", id: "inst-resize"}
		created := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
		resized := time.Date(2026, 3, 16, 0, 0, 0, 0, time.UTC)
		small := map[string]any{"vcpus": 2, "ram_gb": 4, "disk_gb": 40, "flavor": "m1.small"}
		large := map[string]any{"vcpus": 4, "ram_gb": 8, "disk_gb": 80, "flavor": "m1.large"}
		ingestEvents(t, a, fixturePlatform, cloud,
			res.event("inst-resize-create", "compute.instance.create.end", created,
				payloadOf("active", small)),
			res.event("inst-resize-resize", "compute.instance.resize.end", resized,
				payloadOf("active", large)))

		got := lifecycleOf(t, a.call(t, http.MethodGet,
			resourceLifecyclePath(cloud, res.resourceType, res.id), adminToken, nil))

		if len(got.Intervals) != 2 {
			t.Fatalf("intervals = %+v, want the one before and the one after the resize", got.Intervals)
		}
		assertInterval(t, got.Intervals[0], created, &resized, "active", asDocument(t, small))
		assertInterval(t, got.Intervals[1], resized, nil, "active", asDocument(t, large))
		if len(got.Warnings) != 0 {
			t.Errorf("warnings = %v, want none for a history that starts with a create", got.Warnings)
		}

		// The resource is the projection row, which carries the size the resize
		// left behind rather than the one the instance started on.
		if want := asDocument(t, large); !reflect.DeepEqual(got.Resource.Size, want) {
			t.Errorf("the resource's size = %v, want %v", got.Resource.Size, want)
		}
		if got.Resource.CreatedAt == nil || !got.Resource.CreatedAt.Equal(created) {
			t.Errorf("the resource's created_at = %v, want %v", got.Resource.CreatedAt, created)
		}
		want := []string{"inst-resize-create", "inst-resize-resize"}
		if got := ids(got.Events); !slices.Equal(got, want) {
			t.Errorf("served events = %v, want %v", got, want)
		}
	})

	t.Run("closes every interval of a history that ends in a delete", func(t *testing.T) {
		const cloud = "os-lifecycle-deleted"
		res := fixture{cloud: cloud, resourceType: "volume", id: "vol-deleted"}
		ingestEvents(t, a, fixturePlatform, cloud,
			res.event("vol-deleted-create", "volume.create", fleetCreated,
				payloadOf("available", volumeSize(10))),
			res.event("vol-deleted-resize", "volume.resize", fleetChanged,
				payloadOf("available", volumeSize(20))),
			res.event("vol-deleted-delete", "volume.delete", fleetRemoved,
				event.PayloadEnvelope{}))

		got := lifecycleOf(t, a.call(t, http.MethodGet,
			resourceLifecyclePath(cloud, res.resourceType, res.id), adminToken, nil))

		if len(got.Intervals) != 2 {
			t.Fatalf("intervals = %+v, want the one before and the one after the resize", got.Intervals)
		}
		for i, interval := range got.Intervals {
			if interval.To == nil {
				t.Errorf("interval %d is open, want every interval of a deleted resource closed", i)
			}
		}
		// A delete opens no interval of its own: a deleted resource accrues no
		// usage. The row is what reports the end of its life.
		if got, want := got.Resource.State, "deleted"; got != want {
			t.Errorf("the resource's state = %q, want %q", got, want)
		}
		if got.Resource.DeletedAt == nil || !got.Resource.DeletedAt.Equal(fleetRemoved) {
			t.Errorf("the resource's deleted_at = %v, want %v", got.Resource.DeletedAt, fleetRemoved)
		}
	})

	t.Run("serves an interval no event reported a size for as an empty object", func(t *testing.T) {
		// A history whose only event reports a state and no size. Ingestion holds
		// every create to a size, so the case arises on a history that starts
		// mid-life: the fold carries no size forward and leaves the interval's
		// nil. The contract types size as an object rather than as a nullable
		// one, so the empty object stands for it the way the projection's size
		// column does.
		const cloud = "os-lifecycle-sizeless"
		res := fixture{cloud: cloud, resourceType: "volume", id: "vol-sizeless"}
		ingestEvents(t, a, fixturePlatform, cloud,
			res.event("vol-sizeless-update", "volume.update", fleetCreated,
				payloadOf("available", nil)))

		rec := a.call(t, http.MethodGet, resourceLifecyclePath(cloud, res.resourceType, res.id), adminToken, nil)

		got := lifecycleOf(t, rec)
		if len(got.Intervals) != 1 {
			t.Fatalf("intervals = %+v, want the one the create opened", got.Intervals)
		}
		if want := map[string]any{}; !reflect.DeepEqual(got.Intervals[0].Size, want) {
			t.Errorf("size = %v, want %v", got.Intervals[0].Size, want)
		}
		// The raw body rather than the decoded one: a decoded empty map and a
		// null both read as len 0 in Go, and only one of them is the contract.
		if body := strings.TrimSpace(rec.Body.String()); !strings.Contains(body, `"size":{}`) {
			t.Errorf("body = %s, want it to carry \"size\":{}", body)
		}
	})

	t.Run("warns about a history that starts without a create", func(t *testing.T) {
		const cloud = "os-lifecycle-orphan"
		res := fixture{cloud: cloud, resourceType: "volume", id: "vol-lifecycle-orphan"}
		ingestEvents(t, a, fixturePlatform, cloud,
			res.event("vol-lifecycle-orphan-update", "volume.update", fleetChanged,
				payloadOf("available", volumeSize(10))))

		got := lifecycleOf(t, a.call(t, http.MethodGet,
			resourceLifecyclePath(cloud, res.resourceType, res.id), adminToken, nil))

		want := []string{timeline.WarningHistoryStartsWithoutCreate}
		if !slices.Equal(got.Warnings, want) {
			t.Errorf("warnings = %v, want %v", got.Warnings, want)
		}
		if got.Resource.CreatedAt != nil {
			t.Errorf("the resource's created_at = %v, want null when the create was missed",
				*got.Resource.CreatedAt)
		}
	})

	t.Run("drops the interval of a resource created and deleted at one instant", func(t *testing.T) {
		const cloud = "os-lifecycle-instant"
		res := fixture{cloud: cloud, resourceType: "volume", id: "vol-instant"}
		// Both events share the instant and the transaction, so what orders them
		// is the event id, and the create sorts first.
		ingestEvents(t, a, fixturePlatform, cloud,
			res.event("vol-instant-a-create", "volume.create", fleetCreated,
				payloadOf("available", volumeSize(10))),
			res.event("vol-instant-b-delete", "volume.delete", fleetCreated,
				event.PayloadEnvelope{}))

		rec := a.call(t, http.MethodGet, resourceLifecyclePath(cloud, res.resourceType, res.id), adminToken, nil)

		got := lifecycleOf(t, rec)
		// The interval carries no duration, so it bills nothing and is dropped.
		// The events it was folded from are answered all the same.
		if len(got.Intervals) != 0 {
			t.Errorf("intervals = %+v, want none for two changes at one instant", got.Intervals)
		}
		want := []string{"vol-instant-a-create", "vol-instant-b-delete"}
		if got := ids(got.Events); !slices.Equal(got, want) {
			t.Errorf("served events = %v, want %v", got, want)
		}
		// The raw body rather than the decoded one: a client iterates the three
		// arrays without a nil check, which null would break and [] does not.
		body := strings.TrimSpace(rec.Body.String())
		if !strings.Contains(body, `"intervals":[]`) || !strings.Contains(body, `"warnings":[]`) {
			t.Errorf("body = %s, want it to carry \"intervals\":[] and \"warnings\":[]", body)
		}
		if strings.Contains(body, `"events":null`) {
			t.Errorf("body = %s, want the events as an array", body)
		}
	})

	t.Run("refuses a request this API cannot authenticate", func(t *testing.T) {
		rec := a.call(t, http.MethodGet, resourceLifecyclePath("os-lifecycle-resize", "instance", "inst-resize"), "", nil)

		assertChallenged(t, rec)
	})

	t.Run("serves the route without a token while authentication is disabled", func(t *testing.T) {
		open := newAPIInMode(t, db.Store, auth.ModeDisabled)

		got := lifecycleOf(t, open.call(t, http.MethodGet,
			resourceLifecyclePath("os-lifecycle-resize", "instance", "inst-resize"), "", nil))

		if len(got.Intervals) != 2 {
			t.Errorf("intervals = %+v, want the two of the resize example", got.Intervals)
		}
	})
}

// TestResourceReadsHideWhatIsNotVisible pins the answer both per-resource reads
// give a resource the caller may not see: the 404 of a resource that does not
// exist, byte for byte. A caller that could tell the two apart would be able to
// map another project's fleet by asking for its resources one at a time.
func TestResourceReadsHideWhatIsNotVisible(t *testing.T) {
	db := storetest.NewDB(t)
	a := newAPI(t, db.Store)

	const cloud = "os-hidden"
	held := seedProject(t, a, cloud, "p-held")
	seedProject(t, a, cloud, "p-foreign")
	_, token := seedAPIToken(t, a.queries, auth.RoleProject, []uuid.UUID{held})

	seedScopedResource(t, a, cloud, "p-held", "vol-held")
	foreign := seedScopedResource(t, a, cloud, "p-foreign", "vol-foreign")

	// The token reads its own resource, so what the subtests below assert is a
	// refusal rather than a route that answers nothing at all.
	if list := eventListOf(t, a.call(t, http.MethodGet,
		resourceEventsPath(cloud, "volume", "vol-held"), token, nil)); len(list.Items) == 0 {
		t.Fatalf("the history of the token's own resource is empty, want the seeded event")
	}

	for _, tc := range []struct {
		name string
		path func(cloud, resourceType, resourceID string) string
	}{
		{name: "events", path: resourceEventsPath},
		{name: "lifecycle", path: resourceLifecyclePath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unknown := a.call(t, http.MethodGet, tc.path(cloud, "volume", "vol-nothing"), token, nil)
			outside := a.call(t, http.MethodGet, tc.path(cloud, "volume", foreign), token, nil)

			for name, rec := range map[string]*httptest.ResponseRecorder{
				"an unknown resource":                  unknown,
				"a resource outside the token's scope": outside,
			} {
				assertProblem(t, rec, http.StatusNotFound, problem.TypeNotFound)
				if got, want := problemDetail(t, rec), "this resource is not known"; got != want {
					t.Errorf("detail of %s = %q, want %q", name, got, want)
				}
			}
			if got, want := outside.Body.String(), unknown.Body.String(); got != want {
				t.Errorf("the body of a resource outside the scope = %s, want the %s of an unknown one",
					got, want)
			}
		})
	}
}

// TestResourceReadsFollowAnOwnershipTransfer pins where the two scoping rules of
// this API meet: a transfer moves a resource from one project to another, and
// what each project reads afterwards is what keeps the transfer from handing
// either of them the other's billing history.
//
// The event list is event-scoped, so the project that gave the resource away
// keeps reading the events it carried. The per-resource reads are gated on the
// projection row, so the same project stops reading the resource there — and the
// project that accepted it reads the events stored since the transfer rather
// than the whole history.
func TestResourceReadsFollowAnOwnershipTransfer(t *testing.T) {
	db := storetest.NewDB(t)
	a := newAPI(t, db.Store)

	const (
		cloud    = "os-transfer"
		fromID   = "p-transfer-from"
		toID     = "p-transfer-to"
		acquired = "vol-transferred-accept"
		created  = "vol-transferred-create"
	)
	from := seedProject(t, a, cloud, fromID)
	to := seedProject(t, a, cloud, toID)
	_, fromToken := seedAPIToken(t, a.queries, auth.RoleProject, []uuid.UUID{from})
	_, toToken := seedAPIToken(t, a.queries, auth.RoleProject, []uuid.UUID{to})
	_, adminToken := seedAPIToken(t, a.queries, auth.RoleAdmin, nil)

	// The volume lives in one project and is accepted by the other, which is the
	// event roadmap/01-phase-1-core-platform-openstack.md maps
	// volume.transfer.accept.end onto: the same resource under a new project id.
	res := fixture{cloud: cloud, resourceType: "volume", id: "vol-transferred"}
	birth := res.event(created, "volume.create", fleetCreated, payloadOf("available", volumeSize(10)))
	birth.ProjectID = fromID
	accept := res.event(acquired, "volume.transfer.accept.end", fleetChanged,
		payloadOf("available", volumeSize(10)))
	accept.ProjectID = toID
	ingestEvents(t, a, fixturePlatform, cloud, birth, accept)

	path := resourceEventsPath(cloud, res.resourceType, res.id)

	t.Run("keeps the whole history readable for an unfiltered token", func(t *testing.T) {
		list := eventListOf(t, a.call(t, http.MethodGet, path, adminToken, nil))

		if got, want := ids(list.Items), []string{created, acquired}; !slices.Equal(got, want) {
			t.Errorf("served events = %v, want %v", got, want)
		}
	})

	t.Run("the project that gave the resource away", func(t *testing.T) {
		t.Run("keeps reading its events through the event list", func(t *testing.T) {
			list := eventListOf(t, a.call(t, http.MethodGet, eventsRoute, fromToken, nil))

			if got, want := ids(list.Items), []string{created}; !slices.Equal(got, want) {
				t.Errorf("served events = %v, want the event it carried, %v", got, want)
			}
		})

		t.Run("stops reading the resource itself", func(t *testing.T) {
			for name, target := range map[string]string{
				"events":    path,
				"lifecycle": resourceLifecyclePath(cloud, res.resourceType, res.id),
			} {
				t.Run(name, func(t *testing.T) {
					rec := a.call(t, http.MethodGet, target, fromToken, nil)

					assertProblem(t, rec, http.StatusNotFound, problem.TypeNotFound)
				})
			}
		})
	})

	t.Run("the project that accepted the resource", func(t *testing.T) {
		t.Run("reads no event of the project it came from", func(t *testing.T) {
			list := eventListOf(t, a.call(t, http.MethodGet, eventsRoute, toToken, nil))

			if got, want := ids(list.Items), []string{acquired}; !slices.Equal(got, want) {
				t.Errorf("served events = %v, want %v", got, want)
			}
		})

		t.Run("reads the resource history from the transfer onwards", func(t *testing.T) {
			list := eventListOf(t, a.call(t, http.MethodGet, path, toToken, nil))

			if got, want := ids(list.Items), []string{acquired}; !slices.Equal(got, want) {
				t.Errorf("served events = %v, want the history since the transfer, %v", got, want)
			}
		})

		t.Run("folds no interval the other project was billed for", func(t *testing.T) {
			got := lifecycleOf(t, a.call(t, http.MethodGet,
				resourceLifecyclePath(cloud, res.resourceType, res.id), toToken, nil))

			if want := []string{acquired}; !slices.Equal(ids(got.Events), want) {
				t.Errorf("served events = %v, want %v", ids(got.Events), want)
			}
			if len(got.Intervals) != 1 {
				t.Fatalf("intervals = %+v, want the one open since the transfer", got.Intervals)
			}
			if !got.Intervals[0].From.Equal(fleetChanged) {
				t.Errorf("the interval starts at %v, want the transfer at %v",
					got.Intervals[0].From, fleetChanged)
			}
			if got, want := got.Intervals[0].ProjectId, toID; got != want {
				t.Errorf("project_id = %q, want %q", got, want)
			}
			// The create belongs to the project the resource left, so the history
			// this token folds legitimately starts mid-life and says so.
			if want := []string{timeline.WarningHistoryStartsWithoutCreate}; !slices.Equal(got.Warnings, want) {
				t.Errorf("warnings = %v, want %v", got.Warnings, want)
			}
			// The embedded resource is the projection row, which is folded from
			// every event under no scope, so its created_at is the create of the
			// project the resource came from. The contract says so, and pins it
			// here: a consumer that bills a span from created_at rather than from
			// the intervals bills this project for the other one's ownership.
			if got.Resource.CreatedAt == nil || !got.Resource.CreatedAt.Equal(fleetCreated) {
				t.Errorf("the resource's created_at = %v, want the unscoped create at %v",
					got.Resource.CreatedAt, fleetCreated)
			}
		})
	})
}

// TestResourceReadsReportAFailedQuery drives the failure paths of the three
// reads. With authentication disabled nothing reads the database before the
// handler does, so a database that is gone stops the request inside the query
// rather than at the credential lookup in front of it.
func TestResourceReadsReportAFailedQuery(t *testing.T) {
	db := storetest.NewDB(t)
	a := newAPIInMode(t, db.Store, auth.ModeDisabled)

	// How long the database gets to shut down cleanly.
	stopTimeout := 10 * time.Second
	if err := db.Container.Stop(t.Context(), &stopTimeout); err != nil {
		t.Fatalf("stopping the database container: %v", err)
	}

	for _, tc := range []struct {
		name   string
		target string
		detail string
	}{
		{
			name:   "the list",
			target: resourcesRoute,
			detail: "the resources could not be read",
		},
		{
			// The gate in front of the two per-resource reads: a row that cannot
			// be loaded is not a row that is not there, so the answer is a 500
			// rather than the 404 of an unknown resource.
			name:   "the gate of a per-resource read",
			target: resourceLifecyclePath("os-outage", "volume", "vol-outage"),
			detail: "the resource could not be read",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := a.call(t, http.MethodGet, tc.target, "", nil)

			assertProblem(t, rec, http.StatusInternalServerError, problem.TypeInternal)
			if got := problemDetail(t, rec); got != tc.detail {
				t.Errorf("detail = %q, want %q", got, tc.detail)
			}
		})
	}
}

// seedFleet stores the resources every subtest describing the base fleet works
// on. They go in through POST /api/v1/events, so what the reads answer is
// checked against the rows the write path really folded.
func seedFleet(t *testing.T, a api) {
	t.Helper()

	off := fixture{cloud: fleetCloudA, resourceType: "instance", id: "inst-off"}
	alive := fixture{cloud: fleetCloudA, resourceType: "volume", id: "vol-alive"}
	gone := fixture{cloud: fleetCloudA, resourceType: "volume", id: "vol-gone"}
	orphan := fixture{cloud: fleetCloudA, resourceType: "volume", id: "vol-orphan"}
	sibling := fixture{cloud: fleetCloudA, resourceType: "volume", id: "vol-project-b"}
	elsewhere := fixture{cloud: fleetCloudB, resourceType: "volume", id: "vol-elsewhere"}
	widget := fixture{cloud: fleetCloudC, resourceType: "widget", id: "widget-c"}

	// The one resource of a project of its own, in the cloud the others live in
	// too.
	inSibling := sibling.event("vol-project-b-create", "volume.create", fleetCreated,
		payloadOf("available", volumeSize(50)))
	inSibling.ProjectID = fleetProjectB

	ingestEvents(t, a, fixturePlatform, fleetCloudA,
		// An instance that was powered off, which is the one resource of the fleet
		// whose state is neither a volume state nor deleted.
		off.event("inst-off-create", "instance.create", fleetCreated,
			payloadOf("active", map[string]any{
				"vcpus": 2, "ram_gb": 4, "disk_gb": 20, "flavor": "m1.small",
			})),
		off.event("inst-off-power-off", "instance.power_off", fleetChanged,
			payloadOf("shutoff", nil)),
		alive.event("vol-alive-create", "volume.create", fleetCreated,
			payloadOf("available", volumeSize(10))),
		alive.event("vol-alive-resize", "volume.resize", fleetChanged,
			payloadOf("available", volumeSize(20))),
		gone.event("vol-gone-create", "volume.create", fleetCreated,
			payloadOf("available", volumeSize(30))),
		gone.event("vol-gone-delete", "volume.delete", fleetRemoved, event.PayloadEnvelope{}),
		// A history that starts with an update, which is what a missed create
		// leaves behind.
		orphan.event("vol-orphan-update", "volume.update", fleetChanged,
			payloadOf("available", volumeSize(40))),
		inSibling)

	ingestEvents(t, a, fixturePlatform, fleetCloudB,
		elsewhere.event("vol-elsewhere-create", "volume.create", fleetCreated,
			payloadOf("available", volumeSize(60))))

	// The one resource of a platform of its own.
	built := widget.event("widget-c-create", "widget.create", fleetCreated,
		payloadOf("ready", map[string]any{"units": 1}))
	built.Platform = fleetPlatformC
	ingestEvents(t, a, fleetPlatformC, fleetCloudC, built)
}

// seedScopedResource stores one resource of a (cloud, project) pair through the
// ingest path and returns its resource id. The scope tests care about which pair
// a resource belongs to and about nothing else in it.
func seedScopedResource(t *testing.T, a api, cloud, projectID, resourceID string) string {
	t.Helper()

	res := fixture{cloud: cloud, resourceType: "volume", id: resourceID}
	e := res.event(resourceID+"-create", "volume.create", fleetCreated,
		payloadOf("available", volumeSize(10)))
	e.ProjectID = projectID
	ingestEvents(t, a, fixturePlatform, cloud, e)

	return resourceID
}

// ingestEvents submits one batch through POST /api/v1/events under a credential
// for the (platform, cloud) pair it names, and fails the test unless every item
// was stored.
func ingestEvents(t *testing.T, a api, platform, cloud string, events ...event.Event) {
	t.Helper()

	_, token := seedIngestCredential(t, a.queries, platform, cloud)
	items := make([]json.RawMessage, len(events))
	for i, e := range events {
		items[i] = item(t, e)
	}

	got := ingestResultOf(t, a.call(t, http.MethodPost, eventsRoute, token, batch(t, items...)))
	if got.Accepted != len(events) {
		t.Fatalf("result = %+v, want the %d events of %s stored", got, len(events), cloud)
	}
}

// resourceEventsPath is the per-resource history route of one resource.
func resourceEventsPath(cloud, resourceType, resourceID string) string {
	return resourcePath(cloud, resourceType, resourceID) + "/events"
}

// resourceLifecyclePath is the lifecycle route of one resource.
func resourceLifecyclePath(cloud, resourceType, resourceID string) string {
	return resourcePath(cloud, resourceType, resourceID) + "/lifecycle"
}

// resourcePath is the prefix the two per-resource routes share, with every
// segment escaped the way a client sends it.
func resourcePath(cloud, resourceType, resourceID string) string {
	return resourcesRoute + "/" + url.PathEscape(cloud) +
		"/" + url.PathEscape(resourceType) + "/" + url.PathEscape(resourceID)
}

// resourceListOf decodes the answer of a resource list call, which the contract
// promises is a 200 carrying one page.
func resourceListOf(t *testing.T, rec *httptest.ResponseRecorder) ResourceList {
	t.Helper()

	if got := rec.Code; got != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %q)", got, http.StatusOK, rec.Body)
	}
	if got, want := rec.Header().Get("Content-Type"), "application/json"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}

	var list ResourceList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decoding the body %q: %v", rec.Body.String(), err)
	}
	if list.Items == nil {
		t.Errorf("body %q carries items as null, want an array", rec.Body)
	}
	return list
}

// lifecycleOf decodes the answer of a lifecycle call, which the contract
// promises is a 200 carrying the resource, its history, and its intervals. None
// of the three arrays may be null, so a client iterates them without a nil
// check.
func lifecycleOf(t *testing.T, rec *httptest.ResponseRecorder) Lifecycle {
	t.Helper()

	if got := rec.Code; got != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %q)", got, http.StatusOK, rec.Body)
	}

	var got Lifecycle
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding the body %q: %v", rec.Body.String(), err)
	}
	if got.Events == nil || got.Intervals == nil || got.Warnings == nil {
		t.Errorf("body %q carries one of events, intervals, and warnings as null, want arrays", rec.Body)
	}
	return got
}

// assertInterval checks one served interval against the span the history
// implies. A nil to is the open end of a resource that still lives, and the
// project is the fixture's own: no history here transfers a resource.
func assertInterval(t *testing.T, got LifecycleInterval, from time.Time, to *time.Time,
	state string, size map[string]any,
) {
	t.Helper()

	if !got.From.Equal(from) {
		t.Errorf("from = %v, want %v", got.From, from)
	}
	switch {
	case to == nil && got.To != nil:
		t.Errorf("to = %v, want null on the open interval", *got.To)
	case to != nil && got.To == nil:
		t.Errorf("to = null, want %v", *to)
	case to != nil && !got.To.Equal(*to):
		t.Errorf("to = %v, want %v", *got.To, *to)
	}
	if got.State != state {
		t.Errorf("state = %q, want %q", got.State, state)
	}
	if !reflect.DeepEqual(got.Size, size) {
		t.Errorf("size = %v, want %v", got.Size, size)
	}
	if got.ProjectId != fixtureProject {
		t.Errorf("project_id = %q, want %q", got.ProjectId, fixtureProject)
	}
}

// resourceIDs is the resource ids of a served page, in order. The order
// assertions compare these rather than whole rows, so a failure names what came
// back.
func resourceIDs(items []Resource) []string {
	found := make([]string, len(items))
	for i, item := range items {
		found[i] = item.ResourceId
	}
	return found
}

// byResourceID is the served row of one resource, which is what a member
// assertion works on.
func byResourceID(t *testing.T, items []Resource, resourceID string) Resource {
	t.Helper()

	for _, item := range items {
		if item.ResourceId == resourceID {
			return item
		}
	}
	t.Fatalf("the page holds no resource %q, only %v", resourceID, resourceIDs(items))
	return Resource{}
}
