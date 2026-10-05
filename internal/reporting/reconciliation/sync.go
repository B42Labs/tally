package reconciliation

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/b42labs/tally/internal/core/event"
	"github.com/b42labs/tally/internal/core/ids"
	"github.com/b42labs/tally/internal/reporting/ingest"
	"github.com/b42labs/tally/internal/reporting/metrics"
	"github.com/b42labs/tally/internal/reporting/store"
	"github.com/b42labs/tally/internal/reporting/store/sqlcgen"
)

// The statuses a finished run leaves behind. A row holding neither is a run
// that never finished, which is what the column's default 'running' says.
const (
	statusCompleted = "completed"
	statusFailed    = "failed"
)

// stateDeleted is the state the projection row of a deleted resource holds. The
// diff reads it to tell a resource that is gone from one the platform still has.
const stateDeleted = "deleted"

// The kinds a correction comes in. Each is the last segment of the event type
// and the last component of the synthetic event id, so the id of a correction
// and the effect it has cannot drift apart.
const (
	kindCreate = "create"
	kindUpdate = "update"
	kindDelete = "delete"
)

// The reasons a run defers a correction. Each is a key of Deferred in the
// stored stats and the reason label of the deferred counter.
const (
	reasonTransitional = "transitional"
	reasonRecent       = "recent"
)

// correctionBatchSize is how many corrections one transaction carries. The
// ingest pipeline takes a transaction-scoped advisory lock per resource, so a
// batch is also how many of the shared lock table's slots the run holds at
// once, and the first sync of a fleet emits one correction per resource it
// holds. projection.Rebuild bounds its replay at the same size for the same
// reason.
const correctionBatchSize = 100

// maxRecordedErrors is how many reasons one run keeps in Stats.Errors. Nothing
// bounds how many an adapter can produce — one per unreadable observation and
// one per refused correction — and the slice is marshaled into a jsonb column,
// answered to the caller, and joined into one error string.
const maxRecordedErrors = 100

// maxErrorBytes is how much of one reason is kept. Nothing bounds how long one
// is either: an HTTP client hands the response body back verbatim, so a
// platform answering a listing with a multi-megabyte error page writes that page
// into the run's row and its log line. The head of it is what an operator works
// from, the way the dead letter next door keeps the head of a refused item.
const maxErrorBytes = 4 << 10

// completionBudget bounds the two writes a run ends with. They run detached
// from the caller's context, which strips its deadline along with its
// cancellation: without a bound of their own they would wait on a stalled pool
// forever, and the run would never reach the unlock behind them, leaving the
// cloud's advisory lock held for the life of the process.
const completionBudget = 10 * time.Second

// Stats is what one run did. It is stored as the sync_runs stats column and
// returned to the caller unchanged, so the row and the answer say the same
// thing.
type Stats struct {
	// Created, Updated, and Deleted count the corrections a batch the database
	// committed carried, one per synthetic event. A batch that was rolled back is
	// in none of them: the row would otherwise report corrections that never
	// happened.
	Created int `json:"created"`
	Updated int `json:"updated"`
	Deleted int `json:"deleted"`
	// Deferred counts the corrections the run found and left to a later run. It
	// is set before the first batch is ingested, so a run that fails while
	// ingesting still records it.
	Deferred Deferred `json:"deferred"`
	// Errors is what went wrong: a type the adapter could not enumerate, an
	// observation it reported incompletely, a correction the pipeline refused,
	// or the error that ended the run. A run with an error here is 'failed'. It
	// carries the first maxRecordedErrors reasons only, because nothing bounds
	// how many an adapter can produce.
	Errors []string `json:"errors"`
	// ErrorCount is how many reasons the run recorded, the ones past the cap
	// included. It is what says that Errors is a sample rather than the whole
	// record.
	ErrorCount int `json:"error_count"`
}

// Deferred counts the corrections a run did not book, by the reason it waited.
// A resource deferred in five runs counts in each of the five: the counts are
// of corrections a run did not book, not of distinct resources.
type Deferred struct {
	Transitional int `json:"transitional"`
	Recent       int `json:"recent"`
}

// add counts one correction deferred for reason.
func (d *Deferred) add(reason string) {
	switch reason {
	case reasonTransitional:
		d.Transitional++
	case reasonRecent:
		d.Recent++
	}
}

// Result is what a sync run reports back.
type Result struct {
	// RunID is the id of the sync_runs row. It is also what the run's synthetic
	// event ids are derived from, which is what makes them stable per run.
	RunID string
	// Stats is the tally of the run, the same one the row carries.
	Stats Stats
}

// Syncer reconciles the clouds it was configured with. Its zero value is not
// usable; New builds one. It is safe for concurrent use, and two runs of one
// cloud are kept apart by an advisory lock rather than by a mutex, so that the
// guarantee holds across replicas as well.
type Syncer struct {
	db       *store.Store
	pipeline *ingest.Pipeline
	clouds   map[string]CloudConfig
	adapters map[string]Adapter
	now      func() time.Time
	settle   time.Duration
	metrics  *metrics.Metrics
}

// New builds a Syncer over the clouds cfg names and the adapters that observe
// them, indexing the clouds by name so that a sync resolves its configuration
// without walking the list.
//
// now is where every poll-time timestamp comes from. It is injected rather than
// read from the clock, so that a test can state which instant a correction the
// platform gave no timestamp for is dated at.
//
// settle is the window before a run inside which a change is left to the
// collector: a run books no correction for a resource the platform changed, or
// the projection recorded an event for, within it. 0 turns the window off; a
// resource the platform reports as in transition is deferred either way.
//
// m counts the runs and what they reconciled. A nil m records nothing.
func New(db *store.Store, pipeline *ingest.Pipeline, cfg Config, adapters map[string]Adapter,
	now func() time.Time, settle time.Duration, m *metrics.Metrics,
) *Syncer {
	clouds := make(map[string]CloudConfig, len(cfg.Clouds))
	for _, cloud := range cfg.Clouds {
		clouds[cloud.Cloud] = cloud
	}
	return &Syncer{
		db: db, pipeline: pipeline, clouds: clouds, adapters: adapters, now: now, settle: settle,
		metrics: m,
	}
}

// Sync reconciles one cloud: it asks the adapter what the platform holds, diffs
// that against the projection, and ingests the difference as synthetic events
// through the ordinary pipeline.
//
// at is the instant the run is told it happens at. A nil at leaves the run on
// the clock the Syncer was built with, which is what every run does by default.
// A non-nil at is the run's one instant instead: every poll-time correction is
// dated at it, the sync_runs row is started at it, and the adapter is told the
// run is at it, so the window the adapter bounds itself by and the instants the
// corrections carry come from the same clock. The row's started_at has to carry
// it because that column is where the next run of this cloud starts its window,
// which is also why the instants told to the runs of one cloud must not go
// backwards.
//
// A run that recorded anything in Stats.Errors is 'failed' in sync_runs and
// comes back with a non-nil error, whether that error ended the run or only
// spoiled part of it. The Result is returned in either case, so that a caller
// can report the run id and the tally of a run that went badly. What such a run
// did observe is kept: an enumeration failure is missing information, and the
// corrections it did not keep the run from are facts either way.
//
// An adapter that implements SizeNamer has the cloud's size names replaced once
// the listing finished. A failure there is recorded like any other and fails
// the run, but it costs no resource type its completeness: the names say
// nothing about what the cloud holds.
func (s *Syncer) Sync(ctx context.Context, cloud string, at *time.Time) (Result, error) {
	entry, ok := s.clouds[cloud]
	if !ok {
		return Result{}, fmt.Errorf("syncing %s: %w", cloud, ErrUnknownCloud)
	}
	adapter, ok := s.adapters[entry.Adapter]
	if !ok {
		return Result{}, fmt.Errorf("syncing %s: no adapter named %q is registered", cloud, entry.Adapter)
	}

	// The run's own clock. A told instant is read once and answered for the rest
	// of the run, so that every stamp the run writes says the same thing however
	// long the run takes.
	now := s.now
	if at != nil {
		instant := at.UTC()
		now = func() time.Time { return instant }
	}

	// The lock lives for the session rather than for a transaction, so the run
	// holds a connection of its own for as long as it works.
	conn, err := s.db.Pool().Acquire(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("acquiring a connection for the sync of %s: %w", cloud, err)
	}
	locked, err := sqlcgen.New(conn).TrySyncLock(ctx, cloud)
	if err != nil {
		conn.Release()
		return Result{}, fmt.Errorf("locking the sync of %s: %w", cloud, err)
	}
	if !locked {
		conn.Release()
		return Result{}, fmt.Errorf("syncing %s: %w", cloud, ErrAlreadyRunning)
	}
	defer release(ctx, conn, cloud)

	q := sqlcgen.New(s.db.Pool())
	// The run row is written outside a transaction, so that a run in flight can
	// be seen at 'running' while it works rather than appearing when it ends.
	//
	// A told run states its started_at rather than leaving the column to now():
	// the next run of this cloud starts its window at that value, so a told run
	// whose row said now() would leave the window open from the wall clock rather
	// than from the instant it reconciled at.
	var startedAt pgtype.Timestamptz
	if at != nil {
		startedAt = pgtype.Timestamptz{Time: now(), Valid: true}
	}
	id, err := q.InsertSyncRun(ctx, sqlcgen.InsertSyncRunParams{Cloud: cloud, StartedAt: startedAt})
	if err != nil {
		return Result{}, fmt.Errorf("recording the sync run of %s: %w", cloud, err)
	}
	runID := id.String()
	// The empty slice rather than a nil one: the stored stats say "errors": []
	// for a clean run, which is a document an operator can read the same way
	// whatever the run did.
	stats := Stats{Errors: []string{}}

	// Once the row exists, no path may leave it at 'running'. Every error return
	// from here on goes through abort, which records the error in the row and
	// hands the caller the run id and the tally alongside it.
	abort := func(err error) (Result, error) {
		recordError(&stats, err.Error())
		if cerr := s.finish(ctx, id, cloud, stats); cerr != nil {
			err = errors.Join(err, cerr)
		}
		return Result{RunID: runID, Stats: stats}, err
	}

	since, err := lastCompleted(ctx, q, cloud)
	if err != nil {
		return abort(err)
	}
	// A deletion a run deferred happened before that run started, so the deleted
	// listing reaches back by the settle window to name it again. One the last
	// run booked is named again as well and corrects nothing.
	if since != nil && s.settle > 0 {
		reach := since.Add(-s.settle)
		since = &reach
	}

	// The instant the run looks at the platform. The adapter measures its deleted
	// listing from it and the diff its settle window, so both place the run at
	// the same instant.
	runAt := now().UTC()
	observed, incomplete, err := collect(ctx, adapter, entry.AdapterConfig, since, runAt, &stats)
	if err != nil {
		return abort(fmt.Errorf("listing the resources of %s: %w", cloud, err))
	}

	if namer, ok := adapter.(SizeNamer); ok {
		if err := s.storeSizeNames(ctx, namer, entry); err != nil {
			recordError(&stats, err.Error())
		}
	}

	types, err := adapter.ResourceTypes(entry.AdapterConfig)
	if err != nil {
		return abort(fmt.Errorf("reading the resource types of %s: %w", cloud, err))
	}
	// Only a type the run reached the end of can say that a row it did not name
	// is gone. The set is the positive one for that reason: a type that holds
	// nothing is still a type that was enumerated.
	//
	// An observation the adapter reported without naming a type is missing
	// information about the inventory as a whole rather than about one type of
	// it: there is no type to hold back, so no type may conclude that a row it
	// did not name is gone. collect records such an item under the empty key,
	// which screen refuses as a resource type, and the whole pass steps aside.
	enumerated := make(map[string]bool, len(types))
	if !incomplete[""] {
		for _, resourceType := range types {
			if !incomplete[resourceType] {
				enumerated[resourceType] = true
			}
		}
	}

	rows, err := q.ListCurrentResourcesByCloud(ctx, cloud)
	if err != nil {
		return abort(fmt.Errorf("loading the projection of %s: %w", cloud, err))
	}

	events, deferred, err := diff(runID, entry, observed, rows, enumerated, now, runAt, s.settle)
	if err != nil {
		return abort(err)
	}
	stats.Deferred = deferred
	// The corrections go in one transaction per batch rather than one for the
	// whole run, the way projection.Rebuild replays in batches and for the same
	// reason: the pipeline takes an advisory lock per resource that the
	// transaction holds until it commits, and the first sync of a fleet emits a
	// correction per resource. A run that fails partway keeps the batches it
	// committed, so the next one has less to do rather than exactly as much.
	for batch := range slices.Chunk(events, correctionBatchSize) {
		items := make([]json.RawMessage, len(batch))
		for i, e := range batch {
			raw, err := json.Marshal(e)
			if err != nil {
				return abort(fmt.Errorf("marshaling the correction %s: %w", e.EventID, err))
			}
			items[i] = raw
		}

		var outcome ingest.Outcome
		if err := s.db.WithTx(ctx, func(tx pgx.Tx) error {
			var err error
			outcome, err = s.pipeline.Ingest(ctx, tx, items, event.SourceReconciliation, nil)
			return err
		}); err != nil {
			return abort(fmt.Errorf("ingesting the corrections of %s: %w", cloud, err))
		}

		// Counted once the batch is committed rather than once it is built: a
		// batch the database rolled back wrote nothing, and a row claiming those
		// corrections is a record of a fleet that never went through them.
		for _, e := range batch {
			switch event.Categorize(e.EventType) {
			case event.CategoryCreate:
				stats.Created++
			case event.CategoryUpdate:
				stats.Updated++
			case event.CategoryDelete:
				stats.Deleted++
			}
		}
		// A refused correction is the run's problem rather than the batch's: it
		// was dead-lettered, the corrections around it landed, and the reason is
		// what an operator works from.
		for _, rejected := range outcome.Rejected {
			recordError(&stats,
				fmt.Sprintf("event %s rejected: %s", rejected.EventID, rejected.Reason))
		}
	}

	result := Result{RunID: runID, Stats: stats}
	if err := s.finish(ctx, id, cloud, stats); err != nil {
		return result, err
	}
	if stats.ErrorCount > 0 {
		return result, fmt.Errorf("the sync of %s did not finish clean: %s",
			cloud, strings.Join(stats.Errors, "; "))
	}
	return result, nil
}

// finish ends a run: it writes the final row and, once that write landed,
// counts what the run did. A run counts once, when its final row is written, so
// a completion the database refused leaves the run in neither the sync_runs
// table nor the counters. Both paths out of a run go through here, which is
// what keeps the row and the counters saying the same thing.
func (s *Syncer) finish(ctx context.Context, id uuid.UUID, cloud string, stats Stats) error {
	if err := s.complete(ctx, id, stats); err != nil {
		return err
	}
	s.metrics.SyncRunFinished(cloud, runStatus(stats))
	// One call per action and per reason, the zero counts included: a finished
	// run makes every series appear, so a run that deleted nothing reports a
	// delete count of zero rather than no series at all.
	s.metrics.ResourcesReconciled(cloud, "created", stats.Created)
	s.metrics.ResourcesReconciled(cloud, "updated", stats.Updated)
	s.metrics.ResourcesReconciled(cloud, "deleted", stats.Deleted)
	s.metrics.ResourcesDeferred(cloud, reasonTransitional, stats.Deferred.Transitional)
	s.metrics.ResourcesDeferred(cloud, reasonRecent, stats.Deferred.Recent)
	s.metrics.SyncErrorsRecorded(cloud, stats.ErrorCount)
	return nil
}

// runStatus is the status a finished run ends at. The sync_runs row and the
// status label of the run counter both come from here, so the two cannot drift.
func runStatus(stats Stats) string {
	if stats.ErrorCount > 0 {
		return statusFailed
	}
	return statusCompleted
}

// complete writes the run's final row at the status runStatus derives from the
// errors the run recorded, so that the two cannot disagree.
//
// The write runs on a context detached from the caller's. A run ends because
// its deadline passed as readily as because it finished, and a completion that
// the expired context refuses would leave the row at 'running' for a run that
// is over. It gets a deadline of its own, because the detached context has
// none: this write has to acquire a connection while the run still holds one,
// and waiting for it without a bound would hold the run's advisory lock forever.
func (s *Syncer) complete(ctx context.Context, id uuid.UUID, stats Stats) error {
	raw, err := json.Marshal(stats)
	if err != nil {
		return fmt.Errorf("marshaling the stats of sync run %s: %w", id, err)
	}

	done, cancel := context.WithTimeout(context.WithoutCancel(ctx), completionBudget)
	defer cancel()
	if err := sqlcgen.New(s.db.Pool()).CompleteSyncRun(done,
		sqlcgen.CompleteSyncRunParams{ID: id, Status: runStatus(stats), Stats: raw},
	); err != nil {
		return fmt.Errorf("completing sync run %s: %w", id, err)
	}
	return nil
}

// release gives the run's connection back after unlocking the cloud on it. It
// covers the successful return, the error return, and a panic alike, because a
// lock nobody releases keeps every later run of the cloud out.
//
// The unlock runs on a detached context for the same reason the completion
// does: a run whose context expired still holds the lock, and an unlock the
// dead context refuses would send the session back into the pool holding it. A
// session that could not be unlocked is therefore closed rather than reused,
// which is what makes the database drop the session-scoped lock with it. The
// detached context is bounded, because an unlock that waits forever on a
// database that stopped answering never reaches that close either.
func release(ctx context.Context, conn *pgxpool.Conn, cloud string) {
	detached, cancel := context.WithTimeout(context.WithoutCancel(ctx), completionBudget)
	defer cancel()
	if err := sqlcgen.New(conn).UnlockSync(detached, cloud); err != nil {
		_ = conn.Conn().Close(detached)
	}
	conn.Release()
}

// lastCompleted is where an incremental run starts: the started_at of the last
// run of this cloud that completed. A cloud that never completed one gets no
// bound, which is what makes its first run a full one.
//
// A failed run does not move the bound. What it could not enumerate is exactly
// what it may have missed, so the next run walks that window again.
func lastCompleted(ctx context.Context, q *sqlcgen.Queries, cloud string) (*time.Time, error) {
	startedAt, err := q.GetLastCompletedSyncStartedAt(ctx, cloud)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading the last completed sync of %s: %w", cloud, err)
	}
	since := startedAt.Time
	return &since, nil
}

// collect drains the adapter's stream into the observations the diff works on.
// A later report of a resource replaces an earlier one, so an adapter that
// lists a resource twice states its final word rather than two facts.
//
// at is the instant this run is at, and it is the run's own clock rather than
// the adapter's: a window the adapter bounds itself by is measured from the
// same instant the run dates its corrections at.
//
// The second result is the set of resource types that stayed incomplete, which
// the missed-delete pass leaves alone. An error the stream yields is either an
// EnumerationError, which costs its type and nothing else, or an error about
// the run as a whole, which ends it: a platform that stopped answering says
// nothing about what it holds, and reading that as an empty inventory would
// delete a fleet.
func collect(ctx context.Context, adapter Adapter, cfg map[string]any, since *time.Time,
	at time.Time, stats *Stats,
) (map[resourceKey]ObservedResource, map[string]bool, error) {
	observed := map[resourceKey]ObservedResource{}
	incomplete := map[string]bool{}

	for obs, err := range adapter.ListResources(ctx, cfg, since, at) {
		if err != nil {
			var enumErr *EnumerationError
			if !errors.As(err, &enumErr) {
				return nil, nil, err
			}
			incomplete[enumErr.ResourceType] = true
			recordError(stats, err.Error())
			continue
		}
		if reason := screen(obs); reason != "" {
			// The type counts as incomplete as well: an item this run could not
			// read is an item it did not see, and the row it would have matched
			// must not be deleted for an absence the adapter caused. An item that
			// names no type is recorded under the empty key, which holds every
			// type back: it is missing information about the inventory as a whole
			// rather than about one type of it.
			recordError(stats, reason)
			incomplete[obs.ResourceType] = true
			continue
		}
		observed[resourceKey{ResourceType: obs.ResourceType, ResourceID: obs.ResourceID}] = obs
	}
	// A stream that stopped because the run's deadline passed enumerated nothing
	// conclusive. An adapter that returns on a cancelled context without yielding
	// an error would otherwise hand back a partial inventory that reads as a
	// complete one, and every resource it did not reach as a missed delete.
	if err := ctx.Err(); err != nil {
		return nil, nil, fmt.Errorf("the resource stream ended early: %w", err)
	}
	return observed, incomplete, nil
}

// storeSizeNames replaces the size names stored for the cloud of entry with the
// ones namer lists, in one transaction. A listing that failed is no
// information rather than an empty list, and one naming an entry incompletely
// is an adapter bug: either leaves the stored rows as they are, and so does a
// write the database refused. A listing that succeeded empty deletes them.
func (s *Syncer) storeSizeNames(ctx context.Context, namer SizeNamer, entry CloudConfig) error {
	names, err := namer.SizeNames(ctx, entry.AdapterConfig)
	if err != nil {
		return fmt.Errorf("listing the size names of %s: %w", entry.Cloud, err)
	}

	// Sorted, so a listing with two incomplete entries names the same one on
	// every run, and the rows are inserted in one order.
	values := slices.SortedFunc(maps.Keys(names), compareSizeValue)
	for _, value := range values {
		if name := names[value]; value.ResourceType == "" || value.Member == "" ||
			value.Value == "" || name == "" {
			return fmt.Errorf("storing the size names of %s: the adapter reported an incomplete size name "+
				"(resource type %q, member %q, value %q, name %q)",
				entry.Cloud, value.ResourceType, value.Member, value.Value, name)
		}
	}

	if err := s.db.WithTx(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		if err := q.DeleteSizeNames(ctx, entry.Cloud); err != nil {
			return fmt.Errorf("deleting the stored names: %w", err)
		}
		for _, value := range values {
			if err := q.InsertSizeName(ctx, sqlcgen.InsertSizeNameParams{
				Cloud:        entry.Cloud,
				ResourceType: value.ResourceType,
				Member:       value.Member,
				Value:        value.Value,
				Name:         names[value],
			}); err != nil {
				return fmt.Errorf("inserting the name of %q: %w", value.Value, err)
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("storing the size names of %s: %w", entry.Cloud, err)
	}
	return nil
}

// recordError keeps one reason the run has to report, and counts it either way.
// Past the cap only the count grows: the entries are stored, answered, and
// joined into one string, and nothing bounds how many an adapter produces.
//
// A platform's error text is bytes this service did not choose — an HTTP client
// hands the response body back verbatim — so a reason is bounded in length as
// well as in number. PostgreSQL holds no NUL in a jsonb document either, so an
// unscreened one would fail the write that records why the run ended and leave
// the row at 'running' for a run that is over. It is spelled out rather than
// dropped, so what the platform sent stays legible; the encoder already
// replaces every other byte that is not UTF-8.
func recordError(stats *Stats, msg string) {
	stats.ErrorCount++
	if len(stats.Errors) >= maxRecordedErrors {
		return
	}
	if len(msg) > maxErrorBytes {
		// The cut can fall inside a character the platform sent. The encoder
		// replaces what is left of it, the way it replaces every other byte that is
		// not UTF-8.
		msg = msg[:maxErrorBytes] + "… (truncated)"
	}
	stats.Errors = append(stats.Errors, strings.ReplaceAll(msg, "\x00", `\x00`))
}

// screen reports why an observation cannot be diffed, and the empty string for
// one that can. What it refuses is an adapter bug rather than a fact about the
// platform: a correction that names no resource, or hands a live resource to no
// project, is worse than the drift it was meant to fix.
func screen(obs ObservedResource) string {
	if obs.ResourceType == "" {
		return fmt.Sprintf("the adapter reported the resource %q without a resource type", obs.ResourceID)
	}
	if obs.ResourceID == "" {
		return fmt.Sprintf("the adapter reported a %s without a resource id", obs.ResourceType)
	}
	// A deleted resource is reported by its key alone: what state it was in and
	// who owned it is what the projection row already holds.
	if obs.DeletedAt != nil {
		return ""
	}
	if obs.State == "" {
		return fmt.Sprintf("the adapter reported the live %s %s without a state",
			obs.ResourceType, obs.ResourceID)
	}
	if obs.ProjectID == "" {
		return fmt.Sprintf("the adapter reported the live %s %s without a project id",
			obs.ResourceType, obs.ResourceID)
	}
	return ""
}

// resourceKey identifies one resource within a cloud. Both sides of the diff
// are keyed by it, which is how an observation and the projection row it
// belongs to meet.
type resourceKey struct {
	ResourceType string
	ResourceID   string
}

// diff is the correction the observation implies for the projection: what the
// platform holds that the projection does not, what it holds differently, and
// what it no longer holds.
//
// enumerated is the set of resource types the run reached the end of. A row of
// any other type is left alone, however absent it was from the observation.
//
// now is the run's clock, which is where a correction the platform gave no
// instant for is dated.
//
// at is the instant the run looked at the platform, and settle the window
// before it. A difference on a resource the platform reports as in transition,
// or one whose change the platform or the projection dates inside the window,
// is no correction this run books: the collector's notification of it may still
// be on its way. The second result counts those, by the reason deferral gives.
// The projection's own instant counts because the rows are read after the
// listing: a resource created after the platform answered and booked by the
// collector before the rows were read is a live row the observation does not
// name.
func diff(runID string, entry CloudConfig, observed map[resourceKey]ObservedResource,
	rows []sqlcgen.ListCurrentResourcesByCloudRow, enumerated map[string]bool,
	now func() time.Time, at time.Time, settle time.Duration,
) ([]event.Event, Deferred, error) {
	stored := make(map[resourceKey]sqlcgen.ListCurrentResourcesByCloudRow, len(rows))
	for _, row := range rows {
		stored[resourceKey{ResourceType: row.ResourceType, ResourceID: row.ResourceID}] = row
	}

	var events []event.Event
	var deferred Deferred
	// Both passes walk their keys in one order, so that a run over one
	// observation always emits the same batch in the same sequence.
	for _, key := range slices.SortedFunc(maps.Keys(observed), compareResourceKey) {
		obs := observed[key]
		row, known := stored[key]
		state := obs.State
		var lastEvent *time.Time
		if known {
			lastEvent = &row.LastEventAt.Time
		}

		if obs.DeletedAt != nil {
			// A deletion the projection already holds, or one of a resource it
			// never held at all, corrects nothing. The owner is the row's: the
			// resource is gone, so who it belonged to is settled history.
			if known && row.State != stateDeleted {
				if reason := deferral(false, at, settle, obs.DeletedAt, lastEvent); reason != "" {
					deferred.add(reason)
					continue
				}
				events = append(events,
					syntheticEvent(runID, entry, key, kindDelete,
						correctedAt(*obs.DeletedAt, row), row.ProjectID))
			}
			continue
		}

		if !known || row.State == stateDeleted {
			if reason := deferral(obs.Transitional, at, settle, obs.ChangedAt, lastEvent); reason != "" {
				deferred.add(reason)
				continue
			}
			// A resource the projection does not hold live starts a life here,
			// whether the run missed its creation or the resource has come back.
			// The platform's own instant is used when it exposes one; poll time is
			// the concept's accepted approximation when it does not.
			//
			// Only a resource the projection never held may be dated at that
			// instant. One whose row already holds a delete cannot be revived by a
			// create ordered before it: the fold takes the state from the newest
			// lifecycle event, which is then still that delete, so the correction
			// would land, count, and change nothing, and every later run would
			// write another one.
			ts := now().UTC()
			if obs.CreatedAt != nil && !known {
				ts = *obs.CreatedAt
			}
			if known {
				ts = correctedAt(ts, row)
			}
			// A create reports the size the resource starts with. An adapter that
			// reported none leaves the resource sizeless rather than the event
			// invalid, so the correction still lands.
			size := obs.Size
			if size == nil {
				size = map[string]any{}
			}
			create := syntheticEvent(runID, entry, key, kindCreate, ts, obs.ProjectID)
			create.Payload = event.PayloadEnvelope{State: &state, Size: size}
			events = append(events, create)
			continue
		}

		same, err := sameSize(obs.Size, row.Size)
		if err != nil {
			return nil, Deferred{}, fmt.Errorf("comparing the size of %s %s: %w",
				key.ResourceType, key.ResourceID, err)
		}
		if row.State == obs.State && row.ProjectID == obs.ProjectID && same {
			continue
		}
		if reason := deferral(obs.Transitional, at, settle, obs.ChangedAt, lastEvent); reason != "" {
			deferred.add(reason)
			continue
		}
		// A resource that changed hands drifted like one that changed size, so
		// the correction names the owner the platform reports.
		update := syntheticEvent(runID, entry, key, kindUpdate,
			correctedAt(now().UTC(), row), obs.ProjectID)
		// A size the adapter did not report is a size that did not change, and an
		// event without one leaves the projection the size it holds.
		update.Payload = event.PayloadEnvelope{State: &state, Size: obs.Size}
		events = append(events, update)
	}

	// What the projection holds live and the observation did not name at all. A
	// resource the adapter reported as deleted is not one of those: it already
	// got its correction at the instant the platform gave, and a second one
	// would carry the same synthetic id at poll time and bury that instant.
	for _, key := range slices.SortedFunc(maps.Keys(stored), compareResourceKey) {
		row := stored[key]
		if row.State == stateDeleted || !enumerated[key.ResourceType] {
			continue
		}
		if _, seen := observed[key]; seen {
			continue
		}
		// The platform gives no instant for a resource it no longer lists, so the
		// projection's is the only evidence of a change in flight.
		if reason := deferral(false, at, settle, &row.LastEventAt.Time); reason != "" {
			deferred.add(reason)
			continue
		}
		events = append(events,
			syntheticEvent(runID, entry, key, kindDelete, correctedAt(now().UTC(), row), row.ProjectID))
	}
	return events, deferred, nil
}

// deferral is why a run leaves a correction to a later run, and the empty
// string for one it books. Transitional is checked first, so one correction
// counts once. An instant is recent when it lies inside the settle window
// before at, or after at: a platform clock ahead of this host, or a told
// instant behind the cloud's present, dates a change the run cannot place
// before itself. A nil instant is never recent.
func deferral(transitional bool, at time.Time, settle time.Duration, instants ...*time.Time) string {
	if transitional {
		return reasonTransitional
	}
	if settle <= 0 {
		return ""
	}
	for _, instant := range instants {
		if instant != nil && instant.After(at.Add(-settle)) {
			return reasonRecent
		}
	}
	return ""
}

// correctedAt is when a correction of row is dated. It starts from the instant
// the platform gave where the platform gave one and from poll time where it did
// not, and neither is a guarantee of falling past what the row already holds: a
// platform whose clock runs ahead of this one dates its events in this service's
// future, and one that reports a deletion of a resource this run only just
// discovered dates it before the poll the discovery was approximated at.
//
// A correction the fold does not order last decides nothing. Both fold paths
// take a resource's state from its newest event, so a correction slotted
// underneath one would land, count, and change nothing, and every later run
// would mint another event id for the same drift. One that would fall there is
// dated one instant past the row's newest event instead, which is the resolution
// the column holds.
func correctedAt(ts time.Time, row sqlcgen.ListCurrentResourcesByCloudRow) time.Time {
	if ts.Before(row.LastEventAt.Time) {
		return row.LastEventAt.Time.Add(time.Microsecond).UTC()
	}
	return ts
}

// syntheticEvent is the shell every correction shares: the id the run derives
// for it, the coordinates of the cloud it belongs to, and the source that marks
// it as this framework's output rather than a collector's.
//
// A delete carries no payload. The core forces the state of a deleted resource
// itself, and a size is not something a resource that is gone reports.
func syntheticEvent(runID string, entry CloudConfig, key resourceKey, kind string,
	ts time.Time, projectID string,
) event.Event {
	return event.Event{
		EventID:      ids.SyntheticEventID(runID, entry.Cloud, key.ResourceType, key.ResourceID, kind),
		Timestamp:    ts,
		EventType:    "sync." + kind,
		Platform:     entry.Platform,
		Cloud:        entry.Cloud,
		ResourceType: key.ResourceType,
		ResourceID:   key.ResourceID,
		ProjectID:    projectID,
		Source:       event.SourceReconciliation,
	}
}

// sameSize reports whether the observed size says what the projection row
// already holds. The comparison runs over the decoded documents rather than
// over their bytes, so a size an adapter built from an int and the JSON number
// the database returns for it are one size rather than drift.
//
// An adapter that reported no size reported no evidence: that reads as
// unchanged, never as a size that became empty.
func sameSize(observed map[string]any, stored []byte) (bool, error) {
	if observed == nil {
		return true, nil
	}

	raw, err := json.Marshal(observed)
	if err != nil {
		return false, fmt.Errorf("marshaling the observed size: %w", err)
	}
	var reported, held any
	if err := json.Unmarshal(raw, &reported); err != nil {
		return false, fmt.Errorf("decoding the observed size: %w", err)
	}
	if err := json.Unmarshal(stored, &held); err != nil {
		return false, fmt.Errorf("decoding the stored size: %w", err)
	}
	return reflect.DeepEqual(reported, held), nil
}

// compareSizeValue orders two size values byte-wise over their three fields,
// which is the order a run stores its size names in.
func compareSizeValue(a, b SizeValue) int {
	return cmp.Or(
		strings.Compare(a.ResourceType, b.ResourceType),
		strings.Compare(a.Member, b.Member),
		strings.Compare(a.Value, b.Value),
	)
}

// compareResourceKey orders two keys byte-wise over their two fields, which is
// the order the corrections of one run are emitted in.
func compareResourceKey(a, b resourceKey) int {
	return cmp.Or(
		strings.Compare(a.ResourceType, b.ResourceType),
		strings.Compare(a.ResourceID, b.ResourceID),
	)
}
