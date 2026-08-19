package httpapi

import (
	"encoding/json"
	"fmt"
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

// eventStatsRoute is the route the event statistics tests call, spelled once
// because every one of them addresses it.
const eventStatsRoute = "/api/v1/stats/events"

// The clouds the counted events live in. This route narrows on the window and
// on nothing else, so each fixture gets a window of its own and the cloud is
// what names its part of the answer.
const (
	eventStatsCloudA     = "os-evstats-a"
	eventStatsCloudB     = "os-evstats-b"
	eventStatsCloudDays  = "os-evstats-days"
	eventStatsCloudScope = "os-evstats-scope"
	eventStatsCloudBulk  = "os-evstats-bulk"
	eventStatsCloudBulkB = "os-evstats-bulk-b"
)

// eventStatsBulkBuckets is how many buckets the bulk fixture fills. Two clouds
// report in every one of them, so the finest grouping carries two rows per
// bucket and the row bound is met by a window naming half as many buckets as the
// bound counts rows. That is what keeps the two bounds apart: those hours are
// under seven months, well inside the window bound, so what refuses the request
// is the rows it fills rather than how wide it is.
const eventStatsBulkBuckets = maxEventStatsRows / 2

// The projects of the scope fixture. Both store an event in one window and one
// cloud, so what a project token counts is decided by the project alone.
const (
	eventStatsProjectMine  = "evstats-project-mine"
	eventStatsProjectOther = "evstats-project-other"
)

// Where each fixture's window starts. They are whole hours in UTC, which is what
// the buckets are aligned on, so a bucket start is an instant the test names
// rather than one it computes.
var (
	eventStatsHourly = time.Date(2026, 3, 1, 5, 0, 0, 0, time.UTC)
	eventStatsDaily  = time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)
	eventStatsScoped = time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	eventStatsBulk   = time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
)

// TestGetEventStatsOverHTTP drives GET /api/v1/stats/events through the whole
// stack: the contract validator, the grouping rule the handler enforces, the
// bucketing the database does, and the project scope.
//
// The subtests share one database. Every one of them counts a window of its own,
// so the events another subtest stores are outside what it asks for.
func TestGetEventStatsOverHTTP(t *testing.T) {
	db := storetest.NewDB(t)
	a := newAPI(t, db.Store)

	_, adminToken := seedAPIToken(t, a.queries, auth.RoleAdmin, nil)
	_, readAllToken := seedAPIToken(t, a.queries, auth.RoleReadAll, nil)

	seedEventStatsEvents(t, a)

	// The three hours the hourly fixture was stored in, which most subtests below
	// ask for in full.
	hourlyWindow := eventStatsPath("cloud,event_type", eventStatsHourly,
		eventStatsHourly.Add(3*time.Hour), "1h")

	t.Run("counts the events of each hour of a window", func(t *testing.T) {
		rec := a.call(t, http.MethodGet, hourlyWindow, adminToken, nil)

		list := eventStatsOf(t, rec)
		// The two pipelines of the six o'clock hour are summed into one count:
		// this grouping does not name source, so it does not keep them apart.
		want := []string{
			"2026-03-01T05:00:00Z|" + eventStatsCloudA + "|volume.create|-=1",
			"2026-03-01T05:00:00Z|" + eventStatsCloudA + "|volume.resize|-=1",
			"2026-03-01T06:00:00Z|" + eventStatsCloudA + "|volume.create|-=2",
			"2026-03-01T07:00:00Z|" + eventStatsCloudB + "|volume.create|-=1",
		}
		if got := renderEventItems(list.Items); !slices.Equal(got, want) {
			t.Errorf("counted buckets = %q, want %q", got, want)
		}
		// The rendered instant rather than the decoded one: a bucket stated in a
		// zone of the server's own would decode to the same instant, and the
		// contract states every bucket in UTC.
		if body := rec.Body.String(); !strings.Contains(body, `"bucket":"2026-03-01T05:00:00Z"`) {
			t.Errorf("body = %s, want it to carry \"bucket\":\"2026-03-01T05:00:00Z\"", body)
		}
	})

	t.Run("buckets a window by day across a month boundary", func(t *testing.T) {
		// The two events of the thirty-first are ninety minutes apart across
		// midnight from the third, so the days they fall into are what separates
		// them rather than any distance between them.
		list := eventStatsOf(t, a.call(t, http.MethodGet,
			eventStatsPath("cloud,event_type", eventStatsDaily, eventStatsDaily.Add(48*time.Hour), "1d"),
			adminToken, nil))

		want := []string{
			"2026-01-31T00:00:00Z|" + eventStatsCloudDays + "|volume.update|-=2",
			"2026-02-01T00:00:00Z|" + eventStatsCloudDays + "|volume.update|-=1",
		}
		if got := renderEventItems(list.Items); !slices.Equal(got, want) {
			t.Errorf("counted buckets = %q, want %q", got, want)
		}
	})

	t.Run("keeps the pipelines apart when the grouping names source", func(t *testing.T) {
		list := eventStatsOf(t, a.call(t, http.MethodGet,
			eventStatsPath("cloud,event_type,source", eventStatsHourly,
				eventStatsHourly.Add(3*time.Hour), "1h"), adminToken, nil))

		// The six o'clock hour is two items here and one above, which is the whole
		// difference the source dimension makes.
		want := []string{
			"2026-03-01T05:00:00Z|" + eventStatsCloudA + "|volume.create|collector=1",
			"2026-03-01T05:00:00Z|" + eventStatsCloudA + "|volume.resize|collector=1",
			"2026-03-01T06:00:00Z|" + eventStatsCloudA + "|volume.create|collector=1",
			"2026-03-01T06:00:00Z|" + eventStatsCloudA + "|volume.create|reconciliation=1",
			"2026-03-01T07:00:00Z|" + eventStatsCloudB + "|volume.create|collector=1",
		}
		if got := renderEventItems(list.Items); !slices.Equal(got, want) {
			t.Errorf("counted buckets = %q, want %q", got, want)
		}
	})

	t.Run("answers a window holding no event with the empty array", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			target string
		}{
			{
				name: "a window nothing was stored in",
				target: eventStatsPath("cloud,event_type", time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
					time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC), "1d"),
			},
			{
				// The instant of a stored event, taken as both bounds: the window
				// is half-open, so it holds nothing even there.
				name: "a from at the to",
				target: eventStatsPath("cloud,event_type", eventStatsHourly.Add(10*time.Minute),
					eventStatsHourly.Add(10*time.Minute), "1h"),
			},
			{
				// Each bound is a valid instant on its own, and the window between
				// them is empty, which is an answer rather than a refusal.
				name: "a from past the to",
				target: eventStatsPath("cloud,event_type", eventStatsHourly.Add(3*time.Hour),
					eventStatsHourly, "1h"),
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				rec := a.call(t, http.MethodGet, tc.target, adminToken, nil)

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
		}
	})

	t.Run("refuses a grouping that cannot name what it counted", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			groupBy string
		}{
			{name: "without cloud", groupBy: "event_type"},
			{name: "without event_type", groupBy: "cloud"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				rec := a.call(t, http.MethodGet, eventStatsPath(tc.groupBy, eventStatsHourly,
					eventStatsHourly.Add(3*time.Hour), "1h"), adminToken, nil)

				assertProblem(t, rec, http.StatusBadRequest, problem.TypeValidation)
				if got, want := problemDetail(t, rec),
					"group_by must name cloud and event_type"; got != want {
					t.Errorf("detail = %q, want %q", got, want)
				}
			})
		}
	})

	t.Run("refuses a request the contract does not allow", func(t *testing.T) {
		// The window and the width are what bound this answer, so each of them is
		// required and a request leaving one out is refused rather than answered
		// over whatever the other two imply.
		from, to := instant(eventStatsHourly), instant(eventStatsHourly.Add(3*time.Hour))
		for _, tc := range []struct {
			name  string
			query string
		}{
			{name: "no from", query: "group_by=cloud,event_type&to=" + to + "&interval=1h"},
			{name: "no to", query: "group_by=cloud,event_type&from=" + from + "&interval=1h"},
			{name: "no interval", query: "group_by=cloud,event_type&from=" + from + "&to=" + to},
			{name: "an interval outside the two", query: "group_by=cloud,event_type&from=" + from +
				"&to=" + to + "&interval=1w"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				rec := a.call(t, http.MethodGet, eventStatsRoute+"?"+tc.query, adminToken, nil)

				assertProblem(t, rec, http.StatusBadRequest, problem.TypeValidation)
			})
		}
	})

	t.Run("refuses a grouping above the bound", func(t *testing.T) {
		seedEventStatsBulk(t, a)

		rec := a.call(t, http.MethodGet, eventStatsPath("cloud,event_type", eventStatsBulk,
			eventStatsBulk.Add(time.Duration(eventStatsBulkBuckets+1)*time.Hour), "1h"), adminToken, nil)

		assertProblem(t, rec, http.StatusUnprocessableEntity, problem.TypeResultTooLarge)

		t.Run("serves a grouping exactly as large as the bound", func(t *testing.T) {
			// The row one past the bound is what the refusal is decided on, so the
			// window that fills it exactly is the boundary the answer still holds.
			// The window is half-open, so the last hour it covers is the one before
			// its end, and each of those hours carries one group per cloud.
			list := eventStatsOf(t, a.call(t, http.MethodGet,
				eventStatsPath("cloud,event_type", eventStatsBulk,
					eventStatsBulk.Add(eventStatsBulkBuckets*time.Hour), "1h"), adminToken, nil))

			if got := len(list.Items); got != maxEventStatsRows {
				t.Errorf("counted groups = %d, want the %d the bound allows", got, maxEventStatsRows)
			}
		})
	})

	t.Run("refuses a window wider than the bound", func(t *testing.T) {
		// The count aggregates every event of the window before its own limit can
		// discard a row, so what a window costs is the events it holds and not the
		// buckets it names. Nothing is seeded for these: what is being pinned is
		// that the width of the window alone decides the refusal.
		for _, tc := range []struct {
			name     string
			from, to time.Time
			interval string
		}{
			{
				name:     "one hour past the bound",
				from:     eventStatsBulk,
				to:       eventStatsBulk.Add(maxEventStatsWindow + time.Hour),
				interval: "1h",
			},
			{
				// A window the buckets alone would have let through: at a daily
				// width it names 9862 of them, under the number of rows one answer
				// carries, while the aggregate behind it walks and decompresses
				// every chunk of the archive.
				name:     "twenty-seven years of daily buckets",
				from:     eventStatsBulk,
				to:       eventStatsBulk.AddDate(27, 0, 0),
				interval: "1d",
			},
			{
				// Every instant the contract's date-time can name. The distance
				// between the two overflows a duration, which saturates rather than
				// wrapping into a window that would pass the bound.
				name:     "every instant there is",
				from:     time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC),
				to:       time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC),
				interval: "1d",
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				rec := a.call(t, http.MethodGet,
					eventStatsPath("cloud,event_type", tc.from, tc.to, tc.interval), adminToken, nil)

				assertProblem(t, rec, http.StatusUnprocessableEntity, problem.TypeResultTooLarge)
			})
		}

		t.Run("serves a window exactly as wide as the bound", func(t *testing.T) {
			// The window one hour longer is what the refusal is decided on, so the
			// one that meets the bound exactly is the boundary the answer still
			// holds. It runs out before the earliest fixture of this test starts, so
			// what comes back is the empty array rather than a grouping the other
			// bound would then decide.
			empty := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
			list := eventStatsOf(t, a.call(t, http.MethodGet,
				eventStatsPath("cloud,event_type", empty,
					empty.Add(maxEventStatsWindow), "1h"), adminToken, nil))

			if len(list.Items) != 0 {
				t.Errorf("counted groups = %d, want the empty window answered as none", len(list.Items))
			}
		})
	})

	t.Run("confines a project token to the projects it holds", func(t *testing.T) {
		held := seedProject(t, a, eventStatsCloudScope, eventStatsProjectMine)
		_, token := seedAPIToken(t, a.queries, auth.RoleProject, []uuid.UUID{held})
		window := eventStatsPath("cloud,event_type", eventStatsScoped,
			eventStatsScoped.Add(time.Hour), "1h")

		// The other project stored its event in the same hour and the same cloud,
		// so the count an unfiltered token reads is what the scoped one is told
		// from.
		admin := eventStatsOf(t, a.call(t, http.MethodGet, window, adminToken, nil))
		if got, want := renderEventItems(admin.Items),
			[]string{"2026-04-01T09:00:00Z|" + eventStatsCloudScope + "|volume.create|-=2"}; !slices.Equal(got, want) {
			t.Fatalf("counted buckets = %q, want %q", got, want)
		}

		list := eventStatsOf(t, a.call(t, http.MethodGet, window, token, nil))

		want := []string{"2026-04-01T09:00:00Z|" + eventStatsCloudScope + "|volume.create|-=1"}
		if got := renderEventItems(list.Items); !slices.Equal(got, want) {
			t.Errorf("counted buckets = %q, want %q", got, want)
		}
	})

	t.Run("refuses a request this API cannot authenticate", func(t *testing.T) {
		rec := a.call(t, http.MethodGet, hourlyWindow, "", nil)

		assertChallenged(t, rec)
	})

	t.Run("serves a read_all token the whole window", func(t *testing.T) {
		admin := eventStatsOf(t, a.call(t, http.MethodGet, hourlyWindow, adminToken, nil))

		list := eventStatsOf(t, a.call(t, http.MethodGet, hourlyWindow, readAllToken, nil))

		if got, want := renderEventItems(list.Items), renderEventItems(admin.Items); !slices.Equal(got, want) {
			t.Errorf("counted buckets = %q, want the %q an admin token counts", got, want)
		}
	})
}

// TestGetEventStatsReportsAFailedQuery drives the handler's own failure path.
// With authentication disabled nothing reads the database before the count does,
// so a database that is gone stops the request inside the query rather than at
// the credential lookup in front of it.
func TestGetEventStatsReportsAFailedQuery(t *testing.T) {
	db := storetest.NewDB(t)
	a := newAPIInMode(t, db.Store, auth.ModeDisabled)

	// How long the database gets to shut down cleanly.
	stopTimeout := 10 * time.Second
	if err := db.Container.Stop(t.Context(), &stopTimeout); err != nil {
		t.Fatalf("stopping the database container: %v", err)
	}

	rec := a.call(t, http.MethodGet, eventStatsPath("cloud,event_type", eventStatsHourly,
		eventStatsHourly.Add(3*time.Hour), "1h"), "", nil)

	assertProblem(t, rec, http.StatusInternalServerError, problem.TypeInternal)
	// The detail is what tells the internal problems of this route apart: the
	// count failed, rather than a scope that could not be resolved.
	if got, want := problemDetail(t, rec), "the event counts could not be read"; got != want {
		t.Errorf("detail = %q, want %q", got, want)
	}

	// The same request over a window past the bound, against the same database
	// that is gone. A 422 rather than the 500 above is what says the window was
	// refused before anything was read, which is the whole point of bounding it:
	// the refusal of a window too wide to aggregate must not cost the aggregation.
	rec = a.call(t, http.MethodGet, eventStatsPath("cloud,event_type", eventStatsHourly,
		eventStatsHourly.Add(maxEventStatsWindow+time.Hour), "1h"), "", nil)

	assertProblem(t, rec, http.StatusUnprocessableEntity, problem.TypeResultTooLarge)
}

// seedEventStatsEvents stores the events the counting subtests describe, each
// fixture in the window its subtest asks for.
//
// They go in through POST /api/v1/events, so the counts are taken off the rows
// the write path really stored. One of them cannot come from that path: the
// pipeline records the pipeline it ran under whatever a collector claims, so the
// reconciliation row is inserted through the pool.
func seedEventStatsEvents(t *testing.T, a api) {
	t.Helper()

	first := fixture{cloud: eventStatsCloudA, resourceType: "volume", id: "vol-evstats-first"}
	second := fixture{cloud: eventStatsCloudA, resourceType: "volume", id: "vol-evstats-second"}
	third := fixture{cloud: eventStatsCloudA, resourceType: "volume", id: "vol-evstats-third"}
	sibling := fixture{cloud: eventStatsCloudB, resourceType: "volume", id: "vol-evstats-sibling"}

	ingestEvents(t, a, fixturePlatform, eventStatsCloudA,
		// Two events of one hour that differ in their type, so the first bucket
		// carries two items rather than one count of two.
		first.event("evstats-first-create", "volume.create",
			eventStatsHourly.Add(10*time.Minute), payloadOf("available", volumeSize(10))),
		first.event("evstats-first-resize", "volume.resize",
			eventStatsHourly.Add(40*time.Minute), payloadOf("available", volumeSize(20))),
		// On the hour exactly, which is the bucket that instant opens: a bucket
		// covers its width from its start on, the start included.
		second.event("evstats-second-create", "volume.create",
			eventStatsHourly.Add(time.Hour), payloadOf("available", volumeSize(30))))

	// A synthetic event of the reconciliation pipeline, which no request can
	// store: the ingest path records the pipeline it ran under itself. It carries
	// the hour, the cloud, and the type of the collector's create above, so the
	// two are one group until a request groups by source.
	reconciled := third.event("evstats-third-create", "volume.create",
		eventStatsHourly.Add(90*time.Minute), payloadOf("available", volumeSize(40)))
	reconciled.Source = event.SourceReconciliation
	payload, err := json.Marshal(reconciled.Payload)
	if err != nil {
		t.Fatalf("marshaling the payload of %s: %v", reconciled.EventID, err)
	}
	insertEventRow(t, a, reconciled, payload)

	ingestEvents(t, a, fixturePlatform, eventStatsCloudB,
		sibling.event("evstats-sibling-create", "volume.create",
			eventStatsHourly.Add(2*time.Hour+15*time.Minute), payloadOf("available", volumeSize(50))))

	// The daily fixture: two events on the last day of January and one on the
	// first of February, the last two ninety minutes apart across midnight.
	days := fixture{cloud: eventStatsCloudDays, resourceType: "volume", id: "vol-evstats-days"}
	ingestEvents(t, a, fixturePlatform, eventStatsCloudDays,
		days.event("evstats-days-first", "volume.update",
			eventStatsDaily.Add(22*time.Hour), payloadOf("available", volumeSize(10))),
		days.event("evstats-days-second", "volume.update",
			eventStatsDaily.Add(23*time.Hour), payloadOf("in-use", volumeSize(10))),
		days.event("evstats-days-third", "volume.update",
			eventStatsDaily.Add(24*time.Hour+30*time.Minute), payloadOf("available", volumeSize(10))))

	// The scope fixture: one event per project in one hour of one cloud.
	mine := fixture{cloud: eventStatsCloudScope, resourceType: "volume", id: "vol-evstats-mine"}
	other := fixture{cloud: eventStatsCloudScope, resourceType: "volume", id: "vol-evstats-other"}
	ingestEvents(t, a, fixturePlatform, eventStatsCloudScope,
		inProject(mine.event("evstats-scope-mine", "volume.create",
			eventStatsScoped.Add(10*time.Minute), payloadOf("available", volumeSize(10))),
			eventStatsProjectMine),
		inProject(other.event("evstats-scope-other", "volume.create",
			eventStatsScoped.Add(40*time.Minute), payloadOf("available", volumeSize(20))),
			eventStatsProjectOther))
}

// seedEventStatsBulk stores one event per hour and cloud, one hour past what the
// bound serves, in a window of its own.
//
// They are written through the pool rather than ingested: what is being pinned
// is the handler's refusal, and pushing ten thousand events through the write
// path would say nothing more about it. The two clouds share a type and a
// pipeline, so the finest grouping there is holds exactly two rows per bucket
// and the number of rows is what the bound is met with, over a window far
// narrower than the one the window bound refuses.
func seedEventStatsBulk(t *testing.T, a api) {
	t.Helper()

	for _, cloud := range []string{eventStatsCloudBulk, eventStatsCloudBulkB} {
		if _, err := a.store.Pool().Exec(t.Context(),
			`INSERT INTO events (event_id, timestamp, event_type, platform, cloud,
			                     resource_type, resource_id, project_id, source, payload)
			 SELECT 'evstats-bulk-' || $3 || '-' || n, $1::timestamptz + (n * INTERVAL '1 hour'),
			        'volume.update', $2, $3, 'volume', 'vol-evstats-bulk', $4, 'collector',
			        '{"state": "available"}'::jsonb
			 FROM generate_series(0, $5::int) AS n`,
			eventStatsBulk, fixturePlatform, cloud, fixtureProject,
			eventStatsBulkBuckets); err != nil {
			t.Fatalf("writing an event grouping past the bound: %v", err)
		}
	}
}

// eventStatsPath is one statistics request: the grouping, the half-open window,
// and the width of a bucket, as the query string carries them.
func eventStatsPath(groupBy string, from, to time.Time, interval string) string {
	return fmt.Sprintf("%s?group_by=%s&from=%s&to=%s&interval=%s",
		eventStatsRoute, groupBy, instant(from), instant(to), interval)
}

// eventStatsOf decodes the answer of an event statistics call, which the
// contract promises is a 200 carrying the counted buckets.
func eventStatsOf(t *testing.T, rec *httptest.ResponseRecorder) EventStatsList {
	t.Helper()

	if got := rec.Code; got != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %q)", got, http.StatusOK, rec.Body)
	}
	if got, want := rec.Header().Get("Content-Type"), "application/json"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}

	var list EventStatsList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decoding the body %q: %v", rec.Body.String(), err)
	}
	if list.Items == nil {
		t.Errorf("body %q carries items as null, want an array", rec.Body)
	}
	return list
}
