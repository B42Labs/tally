package metrics_test

import (
	"context"
	"io"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/b42labs/tally/internal/reporting/metrics"
	"github.com/b42labs/tally/internal/reporting/store"
	"github.com/b42labs/tally/internal/reporting/store/sqlcgen"
	"github.com/b42labs/tally/internal/reporting/store/storetest"
)

// gaugeName is the series the refresher writes.
const gaugeName = "tally_current_resources"

// The fixtures report one platform and two clouds, which is what lets two
// groups that differ in the cloud alone be told apart.
const (
	platform   = "openstack"
	cloud      = "os-metrics"
	otherCloud = "os-metrics-second"
	projectID  = "project-a"
)

// lastEventAt dates every seeded row. The refresher never reads the column, so
// one instant serves all of them.
var lastEventAt = time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

// group is one label combination of tally_current_resources, and also what a
// fixture seeds a row into.
type group struct {
	platform     string
	cloud        string
	resourceType string
	state        string
}

func TestRefresh(t *testing.T) {
	db := storetest.NewDB(t)

	t.Run("reports one sample per label combination", func(t *testing.T) {
		truncate(t, db)
		volumes := group{platform: platform, cloud: cloud, resourceType: "volume", state: "available"}
		elsewhere := group{platform: platform, cloud: otherCloud, resourceType: "volume", state: "available"}
		instances := group{platform: platform, cloud: cloud, resourceType: "instance", state: "active"}
		// A deleted resource keeps its projection row, so its group is part of
		// what the gauge reports rather than something the count skips.
		gone := group{platform: platform, cloud: cloud, resourceType: "volume", state: "deleted"}

		seed(t, db, volumes, "vol-1")
		seed(t, db, volumes, "vol-2")
		seed(t, db, elsewhere, "vol-3")
		seed(t, db, instances, "srv-1")
		seed(t, db, gone, "vol-4")

		r, reg := newRefresher(t, db.Store)
		refresh(t, r)

		assertSamples(t, reg, map[group]float64{
			volumes:   2,
			elsewhere: 1,
			instances: 1,
			gone:      1,
		})
	})

	t.Run("reports no sample while the projection is empty", func(t *testing.T) {
		truncate(t, db)

		r, reg := newRefresher(t, db.Store)
		refresh(t, r)

		assertSamples(t, reg, map[group]float64{})
	})

	t.Run("drops the sample of a group that lost its last row", func(t *testing.T) {
		truncate(t, db)
		available := group{platform: platform, cloud: cloud, resourceType: "volume", state: "available"}
		deleted := group{platform: platform, cloud: cloud, resourceType: "volume", state: "deleted"}
		seed(t, db, available, "vol-moves")

		r, reg := newRefresher(t, db.Store)
		refresh(t, r)
		assertSamples(t, reg, map[group]float64{available: 1})

		// The delete keeps the row and moves it to another state, which empties
		// the group the first refresh reported.
		seed(t, db, deleted, "vol-moves")
		refresh(t, r)

		assertSamples(t, reg, map[group]float64{deleted: 1})
	})

	t.Run("never reports an incomplete fleet while it refreshes", func(t *testing.T) {
		// A refresh that cleared the gauge before writing it again would leave a
		// window in which a scrape sees no series, or only the ones written so
		// far, so an alert on the fleet being gone fires once an interval and the
		// dashboards saw-tooth. The seeded fleet does not change between the
		// refreshes below, so every gather has to see all of it.
		truncate(t, db)
		const groups = 100
		for i := range groups {
			seed(t, db, group{
				platform:     platform,
				cloud:        cloud,
				resourceType: "volume",
				state:        "state-" + strconv.Itoa(i),
			}, "vol-window-"+strconv.Itoa(i))
		}

		r, reg := newRefresher(t, db.Store)
		refresh(t, r)

		stop := make(chan struct{})
		fewest := make(chan int, 1)
		go func() {
			seen := math.MaxInt
			for {
				select {
				case <-stop:
					fewest <- seen
					return
				default:
					seen = min(seen, gaugeSeries(reg))
				}
			}
		}()

		for range 50 {
			refresh(t, r)
		}
		close(stop)

		if got := <-fewest; got != groups {
			t.Errorf("a scrape landing inside a refresh saw %d %s series, want all %d throughout",
				got, gaugeName, groups)
		}
	})

	t.Run("keeps the previous samples when the count fails", func(t *testing.T) {
		truncate(t, db)
		volumes := group{platform: platform, cloud: cloud, resourceType: "volume", state: "available"}
		seed(t, db, volumes, "vol-broken")

		// A pool of its own on the same database: closing it is what makes the
		// count fail, and the other subtests keep theirs.
		broken, err := store.New(t.Context(), db.URL, 1)
		if err != nil {
			t.Fatalf("opening a second pool: %v", err)
		}
		r, reg := newRefresher(t, broken)
		refresh(t, r)
		broken.Close()

		err = r.Refresh(t.Context())
		if err == nil {
			t.Fatal("Refresh() on a closed pool error = nil, want the failed count")
		}
		if !strings.HasPrefix(err.Error(), "counting the current resources: ") {
			t.Errorf("Refresh() error = %q, want it to name the count it wraps", err)
		}
		// The failed count reset nothing, so the fleet the last successful
		// refresh reported is still what the gauge says.
		assertSamples(t, reg, map[group]float64{volumes: 1})
	})
}

func TestRun(t *testing.T) {
	db := storetest.NewDB(t)

	t.Run("refreshes before its first tick and returns once its context is done", func(t *testing.T) {
		truncate(t, db)
		volumes := group{platform: platform, cloud: cloud, resourceType: "volume", state: "available"}
		seed(t, db, volumes, "vol-run")

		r, reg := newRefresher(t, db.Store)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			// The interval is long enough that no tick can fire during the test,
			// so a sample can only come from the refresh Run does up front.
			r.Run(ctx, time.Hour)
		}()

		waitForSample(t, reg, volumes)
		cancel()

		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("Run() has not returned 10s after its context was cancelled")
		}
	})
}

// newRefresher builds a Refresher over s and a registry of its own, so the
// samples one subtest writes stay out of every other one. The registry is what
// the assertions gather from, since the gauge itself is unexported.
func newRefresher(t *testing.T, s *store.Store) (*metrics.Refresher, *prometheus.Registry) {
	t.Helper()

	reg := prometheus.NewRegistry()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return metrics.NewRefresher(s, metrics.New(reg), logger), reg
}

// refresh runs one refresh and fails the test unless it succeeded.
func refresh(t *testing.T, r *metrics.Refresher) {
	t.Helper()

	if err := r.Refresh(t.Context()); err != nil {
		t.Fatalf("Refresh() error = %v, want nil", err)
	}
}

// seed writes one projection row into g. It fills every column the schema
// requires; only the four the gauge is labeled with decide what is counted.
func seed(t *testing.T, db storetest.DB, g group, resourceID string) {
	t.Helper()

	if err := sqlcgen.New(db.Store.Pool()).UpsertCurrentResource(t.Context(), sqlcgen.UpsertCurrentResourceParams{
		Cloud:         g.cloud,
		Platform:      g.platform,
		ResourceType:  g.resourceType,
		ResourceID:    resourceID,
		ProjectID:     projectID,
		State:         g.state,
		Size:          []byte(`{}`),
		LastEventType: g.resourceType + ".create",
		LastEventAt:   pgtype.Timestamptz{Time: lastEventAt, Valid: true},
	}); err != nil {
		t.Fatalf("seeding the projection row of %s: %v", resourceID, err)
	}
}

// truncate empties the projection, so that the rows one subtest seeds are the
// only ones the next count sees.
func truncate(t *testing.T, db storetest.DB) {
	t.Helper()

	if _, err := db.Store.Pool().Exec(t.Context(), "TRUNCATE current_resources"); err != nil {
		t.Fatalf("emptying the projection: %v", err)
	}
}

// waitForSample blocks until reg carries a sample for g. It is how a test reads
// a gauge another goroutine writes.
func waitForSample(t *testing.T, reg *prometheus.Registry, g group) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, ok := samples(t, reg)[g]; ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %s sample for %+v after 10s, want the one Run refreshes up front", gaugeName, g)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// gaugeSeries reports how many tally_current_resources series reg carries, and
// -1 for a gather that failed. It takes no *testing.T, because it is read from
// a goroutine of its own, where the failure methods may not be called.
func gaugeSeries(reg *prometheus.Registry) int {
	families, err := reg.Gather()
	if err != nil {
		return -1
	}
	for _, family := range families {
		if family.GetName() == gaugeName {
			return len(family.GetMetric())
		}
	}
	return 0
}

// samples reads the tally_current_resources series off reg, keyed by the label
// combination each one carries.
func samples(t *testing.T, reg *prometheus.Registry) map[group]float64 {
	t.Helper()

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gathering the registry: %v", err)
	}

	found := map[group]float64{}
	for _, family := range families {
		if family.GetName() != gaugeName {
			continue
		}
		for _, metric := range family.GetMetric() {
			labels := map[string]string{}
			for _, pair := range metric.GetLabel() {
				labels[pair.GetName()] = pair.GetValue()
			}
			found[group{
				platform:     labels["platform"],
				cloud:        labels["cloud"],
				resourceType: labels["resource_type"],
				state:        labels["state"],
			}] = metric.GetGauge().GetValue()
		}
	}
	return found
}

// assertSamples fails the test unless the gauge carries exactly the samples want
// describes.
func assertSamples(t *testing.T, reg *prometheus.Registry, want map[group]float64) {
	t.Helper()

	got := samples(t, reg)
	if len(got) != len(want) {
		t.Errorf("%s has %d samples, want %d: %+v", gaugeName, len(got), len(want), got)
	}
	for g, value := range want {
		switch found, ok := got[g]; {
		case !ok:
			t.Errorf("no %s sample for %+v, want %v", gaugeName, g, value)
		case found != value:
			t.Errorf("%s for %+v = %v, want %v", gaugeName, g, found, value)
		}
	}
}
