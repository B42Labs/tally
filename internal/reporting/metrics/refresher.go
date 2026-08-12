package metrics

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/b42labs/tally/internal/reporting/store"
	"github.com/b42labs/tally/internal/reporting/store/sqlcgen"
)

// Refresher keeps the tally_current_resources gauge in step with the
// projection. The gauge reports a fleet, which is a state rather than a count of
// things that happened, so it is derived by counting the current_resources
// table instead of by adding to it as events are folded: a value read off the
// table cannot drift from the resources the API serves, and a restart picks it
// up again without replaying anything.
//
// A projection row is never deleted. A resource that is gone keeps its row with
// state "deleted", so the gauge carries a series per deleted group as well.
type Refresher struct {
	db     *store.Store
	m      *Metrics
	logger *slog.Logger

	// written is what the last refresh left on the gauge. A group that is gone
	// is deleted against it, which is what lets the fresh values go in before
	// anything is removed: a scrape landing inside a refresh then sees the
	// previous fleet or the current one, never the empty one a Reset would leave
	// behind for as long as it takes to write the samples again.
	written map[group]int64
}

// group is one label combination of tally_current_resources, in the order the
// vector declares the labels in.
type group [4]string

// refreshBudget bounds one count. The refresher runs on the process context,
// which carries no deadline, and the pool sets no statement timeout: a count
// that meets a lock on current_resources would otherwise block for as long as
// the lock is held, holding one of the pool's connections and never reaching
// the loop's next tick — the gauge would go on reporting a fleet from before
// the stall, with nothing logged to say so.
const refreshBudget = 30 * time.Second

// NewRefresher builds a Refresher over db's pool. m and logger must both be
// non-nil: a service that runs without instrumentation does not build a
// Refresher at all.
func NewRefresher(db *store.Store, m *Metrics, logger *slog.Logger) *Refresher {
	return &Refresher{db: db, m: m, logger: logger}
}

// Refresh counts the projection rows per platform, cloud, resource type, and
// state, and writes one gauge sample per group. A group that has no rows any
// more is deleted rather than left standing at the value some earlier refresh
// put there, and an empty table leaves the gauge without series at all: no
// resources, no samples.
//
// The samples are written first and the vanished groups removed after, so no
// scrape sees a gauge that is missing a group the projection still holds. A
// refresh that fails returns its error and touches the gauge not at all, so a
// database that is briefly unreachable does not make it report an empty fleet.
//
// The resource type and the state a row carries were written by an ingested
// event, so they are bounded here the way the counters bound them. Two rows
// whose bounded labels collapse into one group are summed rather than the
// second overwriting the first.
func (r *Refresher) Refresh(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, refreshBudget)
	defer cancel()

	rows, err := sqlcgen.New(r.db.Pool()).CountCurrentResources(ctx)
	if err != nil {
		return fmt.Errorf("counting the current resources: %w", err)
	}

	written := make(map[group]int64, len(rows))
	for _, row := range rows {
		written[group{
			row.Platform,
			row.Cloud,
			r.m.limiter.bound(labelResourceType, row.ResourceType),
			r.m.limiter.bound(labelState, row.State),
		}] += row.Resources
	}

	for g, resources := range written {
		r.m.currentResources.WithLabelValues(g[0], g[1], g[2], g[3]).Set(float64(resources))
	}
	for g := range r.written {
		if _, ok := written[g]; !ok {
			r.m.currentResources.DeleteLabelValues(g[0], g[1], g[2], g[3])
		}
	}
	r.written = written
	return nil
}

// Run refreshes once and then on every tick of interval, until ctx is done. It
// is meant to run in a goroutine of its own for the life of the process.
//
// A refresh that fails is logged at warn level and the loop goes on. The next
// tick tries again, and until one succeeds the gauge keeps what the last
// successful refresh wrote.
func (r *Refresher) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		if err := r.Refresh(ctx); err != nil {
			r.logger.Warn("could not refresh the current-resources gauge", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
