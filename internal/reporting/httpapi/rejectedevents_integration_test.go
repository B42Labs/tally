package httpapi

import (
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

	"github.com/b42labs/tally/internal/reporting/auth"
	"github.com/b42labs/tally/internal/reporting/httpapi/problem"
	"github.com/b42labs/tally/internal/reporting/store/storetest"
)

// rejectedEventsRoute is the route the dead-letter tests call, spelled once
// because every one of them addresses it.
const rejectedEventsRoute = "/api/v1/rejected-events"

// refusedEventType is what makes the pipeline refuse an item these tests submit:
// a hyphen where the pattern (*event.Event).Validate() holds every event to
// demands a dot.
const refusedEventType = "volume-create"

// beforeAnyRefusal is a bound earlier than anything this test dead-letters, so a
// window ending there matches no row at all.
var beforeAnyRefusal = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// TestListRejectedEventsOverHTTP drives GET /api/v1/rejected-events through the
// whole stack: the contract validator, the admin guard, and the rows ingestion
// dead-lettered behind them.
//
// The subtests share one database and run in order. The first describes the
// whole table while nothing has been refused yet, and the ones after it refuse
// more items, so the counts they assert include what the earlier ones left.
func TestListRejectedEventsOverHTTP(t *testing.T) {
	db := storetest.NewDB(t)
	a := newAPI(t, db.Store)

	_, adminToken := seedAPIToken(t, a.queries, auth.RoleAdmin, nil)

	t.Run("keeps a refused item reviewable with the reason ingest reported", func(t *testing.T) {
		const cloud = "os-dead-review"
		_, ingestToken := seedIngestCredential(t, a.queries, fixturePlatform, cloud)
		res := fixture{cloud: cloud, resourceType: "volume", id: "vol-review"}
		// One item the pipeline refuses between two it stores, which is the batch a
		// collector drops from its buffer once the call returns: what the list
		// answers afterwards is the only copy of the refused item left.
		refused := res.event("dead-review-refused", refusedEventType, createTime,
			payloadOf("available", volumeSize(10)))
		body := batch(t,
			item(t, res.event("dead-review-first", "volume.create", createTime,
				payloadOf("available", volumeSize(10)))),
			item(t, refused),
			item(t, res.event("dead-review-last", "volume.resize", updateTime,
				payloadOf("available", volumeSize(20)))),
		)

		ingested := ingestResultOf(t, a.call(t, http.MethodPost, eventsRoute, ingestToken, body))
		if ingested.Accepted != 2 || len(ingested.Rejected) != 1 {
			t.Fatalf("result = %+v, want the two valid items stored and the third refused", ingested)
		}
		assertRejected(t, ingested.Rejected[0], 1, refused.EventID, "schema: ")

		list := deadLetterListOf(t, a.call(t, http.MethodGet, rejectedEventsRoute, adminToken, nil))

		if list.NextCursor != nil {
			t.Errorf("next_cursor = %q, want null on a page that holds everything", *list.NextCursor)
		}
		if len(list.Items) != 1 {
			t.Fatalf("served items = %v, want the one refused item", deadLetterEventIDs(t, list.Items))
		}
		kept := list.Items[0]
		if kept.Id == uuid.Nil {
			t.Error("id = the nil UUID, want the id of the dead-letter row")
		}
		if kept.ReceivedAt.IsZero() {
			t.Error("received_at is unset, want the instant the item was refused")
		}
		// The two reasons are one string: the answer of the call that refused the
		// item and the row it left behind name the same failure.
		if got, want := kept.Reason, ingested.Rejected[0].Reason; got != want {
			t.Errorf("reason = %q, want the %q ingest reported", got, want)
		}
		if want := asDocument(t, refused); !reflect.DeepEqual(kept.Raw, want) {
			t.Errorf("raw = %v, want the submitted %v", kept.Raw, want)
		}
	})

	t.Run("walks the list in pages and ends the walk with a null cursor", func(t *testing.T) {
		// Two batches of three, which brings the table to the seven rows this walk
		// needs: the six here and the one the subtest above refused. Each batch is
		// one transaction, so its rows share the received_at now() assigned it and
		// the id breaks the tie, which puts a page boundary inside a group of
		// equal instants.
		seedRefusedItems(t, a, "os-dead-walk-a", "dead-walk-a1", "dead-walk-a2", "dead-walk-a3")
		seedRefusedItems(t, a, "os-dead-walk-b", "dead-walk-b1", "dead-walk-b2", "dead-walk-b3")

		whole := deadLetterListOf(t, a.call(t, http.MethodGet,
			rejectedEventsRoute+"?limit=1000", adminToken, nil))
		if len(whole.Items) != 7 {
			t.Fatalf("the unpaginated answer holds %d items, want the 7 refused so far", len(whole.Items))
		}

		for _, tc := range []struct {
			name  string
			limit int
			sizes []int
		}{
			{name: "a last page shorter than the limit", limit: 3, sizes: []int{3, 3, 1}},
			// The whole table in one page: the last page is exactly full, which is
			// what tells "the page is full" from "there is more".
			{name: "a last page exactly as long as the limit", limit: 7, sizes: []int{7}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				page := fmt.Sprintf("%s?limit=%d", rejectedEventsRoute, tc.limit)

				var walked []DeadLetteredEvent
				var sizes []int
				for count := 1; ; count++ {
					if count > len(whole.Items) {
						t.Fatalf("the walk had not ended after %d pages, want it over after %d",
							count, len(tc.sizes))
					}
					list := deadLetterListOf(t, a.call(t, http.MethodGet, page, adminToken, nil))

					sizes = append(sizes, len(list.Items))
					walked = append(walked, list.Items...)
					if list.NextCursor == nil {
						break
					}
					page = fmt.Sprintf("%s?limit=%d&cursor=%s",
						rejectedEventsRoute, tc.limit, url.QueryEscape(*list.NextCursor))
				}

				if !slices.Equal(sizes, tc.sizes) {
					t.Errorf("page sizes = %v, want %v", sizes, tc.sizes)
				}
				if !reflect.DeepEqual(walked, whole.Items) {
					t.Errorf("the walk served %v, want the unpaginated %v",
						deadLetterEventIDs(t, walked), deadLetterEventIDs(t, whole.Items))
				}
			})
		}
	})

	t.Run("takes from as inclusive and to as exclusive on received_at", func(t *testing.T) {
		whole := deadLetterListOf(t, a.call(t, http.MethodGet,
			rejectedEventsRoute+"?limit=1000", adminToken, nil))
		if len(whole.Items) < 3 {
			t.Fatalf("the table holds %d items, want the rows the batches above refused", len(whole.Items))
		}
		// received_at is the database's own, so the bound is read off an answer
		// rather than chosen by the test. The rows of one batch share the instant,
		// which is why the expected halves are cut on the bound instead of on the
		// position the bound was taken from.
		middle := whole.Items[len(whole.Items)/2]
		var wantFrom, wantTo []DeadLetteredEvent
		for _, kept := range whole.Items {
			if kept.ReceivedAt.Before(middle.ReceivedAt) {
				wantTo = append(wantTo, kept)
				continue
			}
			wantFrom = append(wantFrom, kept)
		}

		from := deadLetterListOf(t, a.call(t, http.MethodGet,
			rejectedEventsRoute+"?limit=1000&from="+deadLetterBound(middle.ReceivedAt), adminToken, nil))
		to := deadLetterListOf(t, a.call(t, http.MethodGet,
			rejectedEventsRoute+"?limit=1000&to="+deadLetterBound(middle.ReceivedAt), adminToken, nil))

		served, want := deadLetterEventIDs(t, from.Items), deadLetterEventIDs(t, wantFrom)
		if !slices.Equal(served, want) {
			t.Errorf("from served %v, want the items refused at or after the bound, %v", served, want)
		}
		// The row the bound came from is the one the two ends of the window differ
		// in, which is what inclusive and exclusive mean here.
		if refused := deadLetterEventID(t, middle); !slices.Contains(served, refused) {
			t.Errorf("from served %v, want it to hold %s, whose received_at the bound is", served, refused)
		}

		served, want = deadLetterEventIDs(t, to.Items), deadLetterEventIDs(t, wantTo)
		if !slices.Equal(served, want) {
			t.Errorf("to served %v, want the items refused before the bound, %v", served, want)
		}
		if refused := deadLetterEventID(t, middle); slices.Contains(served, refused) {
			t.Errorf("to served %v, want %s left out, whose received_at the bound is", served, refused)
		}
	})

	t.Run("answers an empty page rather than null items", func(t *testing.T) {
		rec := a.call(t, http.MethodGet,
			rejectedEventsRoute+"?to="+deadLetterBound(beforeAnyRefusal), adminToken, nil)

		if got := rec.Code; got != http.StatusOK {
			t.Fatalf("status = %d, want %d (body %q)", got, http.StatusOK, rec.Body)
		}
		// The raw body rather than the decoded one: a client iterates items without
		// a nil check, which null would break and [] does not.
		want := `{"items":[],"next_cursor":null}`
		if got := strings.TrimSpace(rec.Body.String()); got != want {
			t.Errorf("body = %s, want %s", got, want)
		}
	})

	t.Run("refuses a cursor it did not issue", func(t *testing.T) {
		const (
			at = "2026-06-01T08:00:00Z"
			id = "9b1d2b7e-4c31-4d0a-8f6b-0f2a1c3e5d70"
		)

		for _, tc := range []struct {
			name   string
			cursor string
		}{
			{name: "not base64url", cursor: "not base64!"},
			{name: "base64url that is not an array of strings", cursor: rawCursor(`{"at":"now"}`)},
			{name: "one key where the sort key has two", cursor: encodeCursor([]string{at})},
			{name: "three keys where the sort key has two", cursor: encodeCursor([]string{at, id, "b"})},
			{name: "an empty key", cursor: encodeCursor([]string{at, ""})},
			{name: "a timestamp that is not an instant", cursor: encodeCursor([]string{"yesterday", id})},
			// The id of this list is a UUID rather than a column a client could have
			// read off an item, so a cursor spelling it as anything else is refused
			// here instead of reaching the database as a cast that fails there.
			{name: "an id that is not a UUID", cursor: encodeCursor([]string{at, "dead-review-refused"})},
		} {
			t.Run(tc.name, func(t *testing.T) {
				rec := a.call(t, http.MethodGet,
					rejectedEventsRoute+"?cursor="+url.QueryEscape(tc.cursor), adminToken, nil)

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
		} {
			t.Run(tc.name, func(t *testing.T) {
				rec := a.call(t, http.MethodGet, rejectedEventsRoute+"?"+tc.query, adminToken, nil)

				assertProblem(t, rec, http.StatusBadRequest, problem.TypeValidation)
			})
		}
	})

	t.Run("serves the admin role alone", func(t *testing.T) {
		_, readAllToken := seedAPIToken(t, a.queries, auth.RoleReadAll, nil)
		project := seedProject(t, a, "os-dead-scope", "p-dead")
		_, projectToken := seedAPIToken(t, a.queries, auth.RoleProject, []uuid.UUID{project})

		// A refused item has no project to be scoped by, so a token below the admin
		// role is refused rather than served a narrowed page.
		for name, token := range map[string]string{
			"a read_all token": readAllToken,
			"a project token":  projectToken,
		} {
			t.Run(name, func(t *testing.T) {
				rec := a.call(t, http.MethodGet, rejectedEventsRoute, token, nil)

				assertProblem(t, rec, http.StatusForbidden, problem.TypeForbidden)
			})
		}

		t.Run("no token at all", func(t *testing.T) {
			rec := a.call(t, http.MethodGet, rejectedEventsRoute, "", nil)

			assertChallenged(t, rec)
		})
	})

	t.Run("serves the route without a token while authentication is disabled", func(t *testing.T) {
		// A second router over the same database, which is the development setup:
		// the guard injects an admin principal instead of reading a credential.
		open := newAPIInMode(t, db.Store, auth.ModeDisabled)
		admin := deadLetterListOf(t, a.call(t, http.MethodGet,
			rejectedEventsRoute+"?limit=1000", adminToken, nil))

		list := deadLetterListOf(t, open.call(t, http.MethodGet,
			rejectedEventsRoute+"?limit=1000", "", nil))

		if got, want := deadLetterEventIDs(t, list.Items), deadLetterEventIDs(t, admin.Items); !slices.Equal(got, want) {
			t.Errorf("served items = %v, want the %v an admin token reads", got, want)
		}
	})
}

// TestListRejectedEventsReportsAFailedQuery drives the handler's own failure
// path. With authentication disabled nothing reads the database before the list
// does, so a database that is gone stops the request inside the query rather
// than at the credential lookup in front of it.
func TestListRejectedEventsReportsAFailedQuery(t *testing.T) {
	db := storetest.NewDB(t)
	a := newAPIInMode(t, db.Store, auth.ModeDisabled)

	// How long the database gets to shut down cleanly.
	stopTimeout := 10 * time.Second
	if err := db.Container.Stop(t.Context(), &stopTimeout); err != nil {
		t.Fatalf("stopping the database container: %v", err)
	}

	rec := a.call(t, http.MethodGet, rejectedEventsRoute, "", nil)

	assertProblem(t, rec, http.StatusInternalServerError, problem.TypeInternal)
	// The detail is what tells the internal problems of this route apart: the
	// query failed, rather than anything in front of it.
	if got, want := problemDetail(t, rec), "the dead-lettered events could not be read"; got != want {
		t.Errorf("detail = %q, want %q", got, want)
	}
}

// seedRefusedItems submits one batch under a credential for cloud in which every
// item is refused, and fails the test unless each of them was. The items go in
// through POST /api/v1/events, so the rows the list reads back are the ones the
// pipeline really wrote.
func seedRefusedItems(t *testing.T, a api, cloud string, eventIDs ...string) {
	t.Helper()

	_, token := seedIngestCredential(t, a.queries, fixturePlatform, cloud)
	res := fixture{cloud: cloud, resourceType: "volume", id: "vol-" + cloud}
	items := make([]json.RawMessage, len(eventIDs))
	for i, eventID := range eventIDs {
		items[i] = item(t, res.event(eventID, refusedEventType,
			createTime.Add(time.Duration(i)*time.Minute), payloadOf("available", volumeSize(10))))
	}

	got := ingestResultOf(t, a.call(t, http.MethodPost, eventsRoute, token, batch(t, items...)))
	if got.Accepted != 0 || len(got.Rejected) != len(eventIDs) {
		t.Fatalf("result = %+v, want all %d items of %s refused", got, len(eventIDs), cloud)
	}
}

// deadLetterListOf decodes the answer of a dead-letter list call, which the
// contract promises is a 200 carrying one page.
func deadLetterListOf(t *testing.T, rec *httptest.ResponseRecorder) DeadLetterList {
	t.Helper()

	if got := rec.Code; got != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %q)", got, http.StatusOK, rec.Body)
	}
	if got, want := rec.Header().Get("Content-Type"), "application/json"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}

	var list DeadLetterList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decoding the body %q: %v", rec.Body.String(), err)
	}
	if list.Items == nil {
		t.Errorf("body %q carries items as null, want an array", rec.Body)
	}
	return list
}

// deadLetterEventIDs is the event ids of a served page, in order. The order
// assertions compare these rather than whole items, so a failure names what came
// back.
func deadLetterEventIDs(t *testing.T, items []DeadLetteredEvent) []string {
	t.Helper()

	found := make([]string, len(items))
	for i, kept := range items {
		found[i] = deadLetterEventID(t, kept)
	}
	return found
}

// deadLetterEventID is the event id the raw document of one refused item
// carries. The row's own id is a UUID the database chose, so the id the test
// submitted is what a failure message is readable by.
func deadLetterEventID(t *testing.T, kept DeadLetteredEvent) string {
	t.Helper()

	document, ok := kept.Raw.(map[string]any)
	if !ok {
		t.Fatalf("the raw item of %s decoded to %T, want the submitted JSON object", kept.Id, kept.Raw)
	}
	eventID, ok := document["event_id"].(string)
	if !ok {
		t.Fatalf("the raw item of %s carries no event_id, want the one it was submitted with", kept.Id)
	}
	return eventID
}

// deadLetterBound renders one bound of the window the way the contract asks for
// it, escaped for the query string it travels in. It keeps the fractional
// seconds instant() drops, because received_at is what the database assigned and
// does not sit on a whole second.
func deadLetterBound(at time.Time) string {
	return url.QueryEscape(at.Format(time.RFC3339Nano))
}
