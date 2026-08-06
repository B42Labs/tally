// Package projection folds ingested events into current_resources, the one row
// per (cloud, resource_type, resource_id) that says what a resource is right
// now. The row is derived state: the events table is the source of truth, and
// every row here can be thrown away and folded again from it.
//
// Two paths write a row. Apply folds a batch of just-ingested events onto the
// row as it stands, which costs one read and one write per resource key. That
// works only while no event of the batch is older than what the row already
// folded, because the incremental fold cannot slot an event underneath the ones
// already applied. A late event therefore sends Apply to Replay, which folds the
// resource's whole history through internal/core/timeline. Both paths write the
// same row for the same history, which is what keeps the order events arrive in
// out of the result.
//
// Both paths take the transaction-scoped advisory lock on the resource key
// before they read anything, so two transactions that touch the same resource
// fold one after the other, and a Rebuild waits for the ingest transaction
// holding the key instead of writing over its result.
//
// Rows are never deleted. A deleted resource keeps its row with state "deleted"
// and deleted_at set, which is the candidate index later phases scan.
//
// The normative specification is roadmap/01-phase-1-core-platform-openstack.md,
// WP 1.7.
package projection

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/b42labs/tally/internal/core/event"
	"github.com/b42labs/tally/internal/core/timeline"
	"github.com/b42labs/tally/internal/reporting/store"
	"github.com/b42labs/tally/internal/reporting/store/sqlcgen"
)

// stateDeleted is the state a deleted resource keeps. The core sets it itself
// instead of reading it off the delete event, which carries no state.
const stateDeleted = "deleted"

// emptySize is what the NOT NULL size column holds for a resource whose history
// has not reported a size yet.
const emptySize = "{}"

// rebuildBatchSize is how many resource keys one Rebuild transaction replays. It
// bounds how long a transaction holds its locks, and a run that dies partway
// leaves every batch before it rebuilt, so the run can simply be repeated.
const rebuildBatchSize = 100

// Key identifies the resource a projection row belongs to. It is the primary key
// of current_resources and the string the advisory lock hashes.
type Key struct {
	Cloud        string
	ResourceType string
	ResourceID   string
}

// id renders the key the way LockResource concatenates it, which is what the
// error messages of this package name a resource by.
func (k Key) id() string {
	return k.Cloud + ":" + k.ResourceType + ":" + k.ResourceID
}

// Filter narrows a Rebuild to part of the fleet. An empty field filters nothing,
// so the zero Filter rebuilds every resource the events table knows.
type Filter struct {
	Cloud        string
	ResourceType string
}

// Apply folds newEvents onto the projection row of key. Callers group the events
// they just inserted per resource key and call this once per key from the same
// transaction, so that the events and the row they imply commit together.
//
// The fold is incremental as long as no event of the batch predates the row's
// last_event_at. One that does is a late event, which the incremental fold
// cannot place, so the call turns into a Replay of the whole history. An empty
// batch changes nothing.
func Apply(ctx context.Context, tx pgx.Tx, key Key, newEvents []event.Stored) error {
	if len(newEvents) == 0 {
		return nil
	}

	q := sqlcgen.New(tx)
	if err := lock(ctx, q, key); err != nil {
		return err
	}
	current, found, err := loadRow(ctx, q, key)
	if err != nil {
		return err
	}

	ordered := slices.Clone(newEvents)
	slices.SortStableFunc(ordered, timeline.Compare)

	if found && ordered[0].Timestamp.Before(current.lastEventAt) {
		return Replay(ctx, tx, key)
	}

	for _, e := range ordered {
		if err := current.fold(e); err != nil {
			return err
		}
	}
	return upsert(ctx, q, key, current)
}

// Replay folds the full history of key into its projection row, which is how a
// late event and an operator-triggered rebuild both reach a correct row.
//
// A key without history leaves the row untouched: there is nothing to derive a
// snapshot from.
//
// The advisory lock is taken here as well. It is reentrant within a transaction,
// so the Apply path that lands here pays nothing for it, and taking it is what
// keeps a rebuild from folding a resource another transaction is ingesting.
func Replay(ctx context.Context, tx pgx.Tx, key Key) error {
	q := sqlcgen.New(tx)
	if err := lock(ctx, q, key); err != nil {
		return err
	}

	rows, err := q.ListEventsForResource(ctx, sqlcgen.ListEventsForResourceParams{
		Cloud:        key.Cloud,
		ResourceType: key.ResourceType,
		ResourceID:   key.ResourceID,
	})
	if err != nil {
		return fmt.Errorf("loading the history of %s: %w", key.id(), err)
	}
	if len(rows) == 0 {
		return nil
	}

	history := make([]event.Stored, len(rows))
	for i, row := range rows {
		stored, err := decode(row)
		if err != nil {
			return err
		}
		history[i] = stored
	}

	// The query orders the history, so its final event is what the row reports as
	// the last one seen.
	last := history[len(history)-1]
	tl := timeline.Build(history)

	current := snapshot{
		platform:      last.Platform,
		projectID:     last.ProjectID,
		size:          []byte(emptySize),
		createdAt:     tl.CreatedAt,
		deletedAt:     tl.DeletedAt,
		lastEventType: last.EventType,
		lastEventAt:   last.Timestamp,
	}
	// The size comes from the events rather than from the intervals: a resource
	// created and deleted at one instant leaves a zero-duration interval, which
	// bills nothing and is therefore dropped, but the size its history reported
	// is still what the row has to carry. The incremental fold takes the size of
	// the last event that reported one, so this does too.
	for _, e := range history {
		if e.Payload.Size == nil {
			continue
		}
		size, err := json.Marshal(e.Payload.Size)
		if err != nil {
			return fmt.Errorf("marshaling the size of %s: %w", key.id(), err)
		}
		current.size = size
	}
	// A history that starts without a create leaves CreatedAt nil, and a history
	// whose changes all fall on one instant leaves no interval behind.
	if n := len(tl.Intervals); n > 0 {
		current.state = tl.Intervals[n-1].State
	}
	if tl.DeletedAt != nil {
		current.state = stateDeleted
	}
	payload, err := json.Marshal(last.Payload)
	if err != nil {
		return fmt.Errorf("marshaling the payload of event %s: %w", last.EventID, err)
	}
	current.lastPayload = payload

	return upsert(ctx, q, key, current)
}

// Rebuild replays every resource key the filter selects and returns how many it
// replayed. It is the operational guarantee behind the projection: whatever a
// row holds, the history it is derived from can produce it again.
//
// The keys are listed outside a transaction and replayed in batches, one
// transaction each. A run that fails partway returns the keys it got through
// along with the error, and repeating it costs only the work it redoes.
func Rebuild(ctx context.Context, s *store.Store, filter Filter) (int, error) {
	rows, err := sqlcgen.New(s.Pool()).ListResourceKeys(ctx, sqlcgen.ListResourceKeysParams{
		Cloud:        text(filter.Cloud),
		ResourceType: text(filter.ResourceType),
	})
	if err != nil {
		return 0, fmt.Errorf("listing the resource keys: %w", err)
	}

	keys := make([]Key, len(rows))
	for i, row := range rows {
		keys[i] = Key{Cloud: row.Cloud, ResourceType: row.ResourceType, ResourceID: row.ResourceID}
	}
	// The query orders the keys under the database's collation, which sorts
	// punctuation and case differently from the byte-wise order a batch of
	// events is folded in. Sorting them here is what makes a rebuild and a
	// concurrent batch take the locks of the keys they share in one order.
	slices.SortFunc(keys, CompareKey)

	rebuilt := 0
	for batch := range slices.Chunk(keys, rebuildBatchSize) {
		if err := s.WithTx(ctx, func(tx pgx.Tx) error {
			for _, key := range batch {
				if err := Replay(ctx, tx, key); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return rebuilt, fmt.Errorf("rebuilding the projections: %w", err)
		}
		rebuilt += len(batch)
	}
	return rebuilt, nil
}

// CompareKey orders two resource keys byte-wise over their three fields. It is
// exported because every path that folds several resources has to take their
// advisory locks in this one order: two transactions locking the keys they
// share in opposite orders deadlock, and Postgres kills one of them.
func CompareKey(a, b Key) int {
	return cmp.Or(
		strings.Compare(a.Cloud, b.Cloud),
		strings.Compare(a.ResourceType, b.ResourceType),
		strings.Compare(a.ResourceID, b.ResourceID),
	)
}

// snapshot is a projection row while it is being folded. It holds size and
// payload as the JSON the columns take, so a row read back from the database
// carries them through the fold untouched.
type snapshot struct {
	platform      string
	projectID     string
	state         string
	size          []byte
	createdAt     *time.Time
	deletedAt     *time.Time
	lastEventType string
	lastEventAt   time.Time
	lastPayload   []byte
}

// fold applies one event to the snapshot. It is the incremental path, so it
// takes e for the newest event the resource has: nothing here reconsiders what
// earlier events decided.
func (s *snapshot) fold(e event.Stored) error {
	switch event.Categorize(e.EventType) {
	case event.CategoryCreate:
		ts := e.Timestamp
		s.createdAt, s.deletedAt = &ts, nil
		if err := s.applyPayload(e.Payload); err != nil {
			return err
		}
	case event.CategoryDelete:
		ts := e.Timestamp
		s.deletedAt = &ts
		s.state = stateDeleted
	case event.CategoryUpdate:
		if err := s.applyPayload(e.Payload); err != nil {
			return err
		}
	}

	payload, err := json.Marshal(e.Payload)
	if err != nil {
		return fmt.Errorf("marshaling the payload of event %s: %w", e.EventID, err)
	}
	s.platform, s.projectID = e.Platform, e.ProjectID
	s.lastEventType, s.lastEventAt, s.lastPayload = e.EventType, e.Timestamp, payload
	return nil
}

// applyPayload takes over the state and the size the payload reports. A member
// the payload leaves out means the resource kept it, which is how timeline reads
// a payload as well.
func (s *snapshot) applyPayload(p event.PayloadEnvelope) error {
	if p.State != nil {
		s.state = *p.State
	}
	if p.Size == nil {
		return nil
	}
	size, err := json.Marshal(p.Size)
	if err != nil {
		return fmt.Errorf("marshaling the size: %w", err)
	}
	s.size = size
	return nil
}

// lock takes the transaction-scoped advisory lock on key. It blocks until every
// other transaction folding the same resource has committed or rolled back.
func lock(ctx context.Context, q *sqlcgen.Queries, key Key) error {
	if err := q.LockResource(ctx, sqlcgen.LockResourceParams{
		Column1: key.Cloud,
		Column2: key.ResourceType,
		Column3: key.ResourceID,
	}); err != nil {
		return fmt.Errorf("locking %s: %w", key.id(), err)
	}
	return nil
}

// loadRow reads the projection row of key FOR UPDATE and reports whether there
// is one. A key without a row starts from an empty snapshot, whose size is the
// empty object the NOT NULL column needs.
func loadRow(ctx context.Context, q *sqlcgen.Queries, key Key) (snapshot, bool, error) {
	row, err := q.GetCurrentResourceForUpdate(ctx, sqlcgen.GetCurrentResourceForUpdateParams{
		Cloud:        key.Cloud,
		ResourceType: key.ResourceType,
		ResourceID:   key.ResourceID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return snapshot{size: []byte(emptySize)}, false, nil
		}
		return snapshot{}, false, fmt.Errorf("loading the projection row of %s: %w", key.id(), err)
	}

	return snapshot{
		platform:      row.Platform,
		projectID:     row.ProjectID,
		state:         row.State,
		size:          row.Size,
		createdAt:     timePtr(row.CreatedAt),
		deletedAt:     timePtr(row.DeletedAt),
		lastEventType: row.LastEventType,
		lastEventAt:   row.LastEventAt.Time,
		lastPayload:   row.LastPayload,
	}, true, nil
}

// upsert writes the folded snapshot as the projection row of key.
func upsert(ctx context.Context, q *sqlcgen.Queries, key Key, s snapshot) error {
	if err := q.UpsertCurrentResource(ctx, sqlcgen.UpsertCurrentResourceParams{
		Cloud:         key.Cloud,
		Platform:      s.platform,
		ResourceType:  key.ResourceType,
		ResourceID:    key.ResourceID,
		ProjectID:     s.projectID,
		State:         s.state,
		Size:          s.size,
		CreatedAt:     timestamptz(s.createdAt),
		DeletedAt:     timestamptz(s.deletedAt),
		LastEventType: s.lastEventType,
		LastEventAt:   pgtype.Timestamptz{Time: s.lastEventAt, Valid: true},
		LastPayload:   s.lastPayload,
	}); err != nil {
		return fmt.Errorf("writing the projection row of %s: %w", key.id(), err)
	}
	return nil
}

// decode turns one events row into the stored event the fold works on. A row
// whose payload column is NULL carries the empty envelope, which reports neither
// a state nor a size.
func decode(row sqlcgen.Event) (event.Stored, error) {
	stored := event.Stored{
		Event: event.Event{
			EventID:      row.EventID,
			Timestamp:    row.Timestamp.Time,
			EventType:    row.EventType,
			Platform:     row.Platform,
			Cloud:        row.Cloud,
			ResourceType: row.ResourceType,
			ResourceID:   row.ResourceID,
			ProjectID:    row.ProjectID,
			Source:       event.Source(row.Source),
		},
		ReceivedAt: row.ReceivedAt.Time,
	}
	if len(row.Payload) > 0 {
		if err := json.Unmarshal(row.Payload, &stored.Payload); err != nil {
			return event.Stored{}, fmt.Errorf("decoding the payload of event %s: %w", row.EventID, err)
		}
	}
	return stored, nil
}

// text maps a filter field to the query parameter: an empty field is the absence
// of a filter and becomes NULL, which the query reads as "every value".
func text(s string) pgtype.Text {
	return pgtype.Text{String: s, Valid: s != ""}
}

// timestamptz maps an optional instant to the column value.
func timestamptz(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

// timePtr maps a nullable column to the optional instant the fold carries.
func timePtr(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	t := ts.Time
	return &t
}
