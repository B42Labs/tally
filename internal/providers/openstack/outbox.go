package openstack

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync/atomic"
	"time"

	// The CGO-free SQLite driver, registered under the name "sqlite". It keeps
	// the collector binary statically cross-compilable.
	_ "modernc.org/sqlite"
)

// outboxDSN carries the four pragmas as DSN parameters instead of running them
// as statements after the open. database/sql hands out pooled connections and
// opens new ones under load, and synchronous and busy_timeout are per-connection
// settings: a connection the pool adds later would come up without them.
// journal_mode and auto_vacuum are properties of the file itself and would
// survive, but auto_vacuum is fixed when the file is created and cannot be
// changed afterwards without a VACUUM that rewrites the whole file, so it has to
// be here on the very first open of a volume.
const outboxDSN = "file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)" +
	"&_pragma=busy_timeout(5000)&_pragma=auto_vacuum(incremental)"

// outboxDDL is the buffer's entire schema. created_at is text because SQLite has
// no date type and RFC 3339 sorts the same way it compares.
const outboxDDL = `CREATE TABLE IF NOT EXISTS outbox(
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	event_json TEXT NOT NULL,
	created_at TEXT NOT NULL
)`

// outboxSchemaVersion is the shape of the table above, written into the file's
// user_version. CREATE TABLE IF NOT EXISTS is a no-op against a table of any
// other shape, because SQLite compares nothing but the name, so the version is
// what makes a file this build cannot use say so at startup rather than on
// every insert.
const outboxSchemaVersion = 1

// Outbox is the collector's durable buffer between the consumer and the sender.
// A mapped event is committed here before its notification is acknowledged, so
// the broker never drops a message Tally has not written down.
//
// Two goroutines share one handle: the consumer inserts, the sender reads a
// batch and deletes it once the Reporting API has taken it. WAL mode lets that
// read run while an insert commits, and the busy timeout absorbs the moments
// where SQLite still serializes the two.
//
// synchronous is FULL rather than the WAL default NORMAL because the
// acknowledgement is a promise. NORMAL leaves a commit in the operating system's
// cache, which a crashing process survives but a power loss does not, and by
// then the broker has already given the message away. FULL costs an fsync per
// insert and buys back the guarantee the acknowledgement rests on.
type Outbox struct {
	db *sql.DB
	// depth mirrors the row count. The consumer compares it against its
	// backpressure bound once per message, and at that bound the table holds a
	// million rows, where a COUNT(*) is a scan and this is a load.
	depth atomic.Int64
	// now is the clock, replaced in tests.
	now func() time.Time
}

// OpenOutbox opens the buffer at path, creates the file and the schema when they
// do not exist yet, and picks up the events the file already carries.
//
// path belongs on a volume that outlives the container: between the
// acknowledgement and the delivery, an event lives here and nowhere else.
func OpenOutbox(path string) (*Outbox, error) {
	db, err := sql.Open("sqlite", fmt.Sprintf(outboxDSN, path))
	if err != nil {
		return nil, fmt.Errorf("opening the outbox at %s: %w", path, err)
	}

	ctx := context.Background()

	// sql.Open does not touch the file, so this is the statement that reports a
	// missing directory, a read-only volume, or a corrupt database.
	if _, err := db.ExecContext(ctx, outboxDDL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("creating the outbox table at %s: %w", path, err)
	}
	if err := stampSchemaVersion(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("checking the outbox schema at %s: %w", path, err)
	}

	var depth int64
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM outbox`).Scan(&depth); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("counting the buffered events at %s: %w", path, err)
	}

	box := &Outbox{db: db, now: time.Now}
	box.depth.Store(depth)
	return box, nil
}

// stampSchemaVersion marks a file this build created and refuses one another
// build's schema wrote. A fresh file reports version 0 and is stamped; a file
// this build knows passes; anything else is a volume written by a version that
// is not this one, in either direction, and reading it would surface as a
// failure per message instead of one at startup.
func stampSchemaVersion(ctx context.Context, db *sql.DB) error {
	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("reading the schema version: %w", err)
	}
	switch version {
	case 0:
		// The pragma takes no parameter, so the version is formatted in. It is a
		// constant of this package and not a value from outside it.
		if _, err := db.ExecContext(ctx,
			fmt.Sprintf(`PRAGMA user_version = %d`, outboxSchemaVersion)); err != nil {
			return fmt.Errorf("marking the schema version: %w", err)
		}
	case outboxSchemaVersion:
	default:
		return fmt.Errorf("the file carries schema version %d, this build needs %d",
			version, outboxSchemaVersion)
	}
	return nil
}

// Row is one buffered event: the id the sender deletes it by, and the event
// document as it was inserted.
type Row struct {
	// ID orders the buffer and addresses the row in DeleteBatch.
	ID int64
	// EventJSON is the canonical event, byte for byte as Insert received it.
	EventJSON []byte
}

// Insert commits one mapped event. It returns after the row is on disk, which is
// what makes acknowledging the notification next a safe thing to do.
func (o *Outbox) Insert(ctx context.Context, eventJSON []byte) error {
	if _, err := o.db.ExecContext(ctx,
		`INSERT INTO outbox(event_json, created_at) VALUES (?, ?)`,
		string(eventJSON), o.now().UTC().Format(time.RFC3339),
	); err != nil {
		return fmt.Errorf("buffering an event: %w", err)
	}
	o.depth.Add(1)
	return nil
}

// Batch reads up to limit buffered events, oldest first. An empty buffer yields
// an empty batch and no error, which is the ordinary answer for a sender polling
// a quiet collector.
func (o *Outbox) Batch(ctx context.Context, limit int) ([]Row, error) {
	rows, err := o.db.QueryContext(ctx,
		`SELECT id, event_json FROM outbox ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("reading a batch from the outbox: %w", err)
	}
	defer func() { _ = rows.Close() }()

	batch := []Row{}
	for rows.Next() {
		var row Row
		if err := rows.Scan(&row.ID, &row.EventJSON); err != nil {
			return nil, fmt.Errorf("reading a buffered event: %w", err)
		}
		batch = append(batch, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading a batch from the outbox: %w", err)
	}
	return batch, nil
}

// DeleteBatch removes the events one 200 answer covered. It addresses them by id
// instead of by a bound on id, so the rows the consumer inserted while the batch
// was in flight stay buffered. An empty list of ids deletes nothing.
//
// It runs after a 200 and only then: a failed POST leaves the batch where it is
// and the next round offers it again.
func (o *Outbox) DeleteBatch(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}

	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	statement := `DELETE FROM outbox WHERE id IN (` +
		strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + `)`

	result, err := o.db.ExecContext(ctx, statement, args...)
	if err != nil {
		return fmt.Errorf("deleting %d delivered events: %w", len(ids), err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("counting the deleted events: %w", err)
	}
	// The correction comes from the statement rather than from len(ids), so that
	// ids the table no longer holds do not pull the depth below the row count.
	o.depth.Add(-deleted)

	// Incremental auto-vacuum only moves the freed pages onto the freelist; this
	// is what hands them back to the filesystem. Without it an outage that filled
	// the buffer toward BufferMaxEvents leaves the file at its peak size forever,
	// and the volume it sits on is sized for one outage, not for every outage the
	// collector has ever seen. The bound keeps a batch delete from turning into a
	// long write, and what one pass leaves the next one takes. The delete has
	// already committed, so a failure here costs disk rather than events.
	if _, err := o.db.ExecContext(ctx, `PRAGMA incremental_vacuum(1000)`); err != nil {
		slog.Default().Warn("reclaiming the freed outbox pages failed, the file keeps its size",
			"error", err)
	}
	return nil
}

// Depth is the number of buffered events. The consumer weighs it against its
// bound once per message, so it answers from memory and never queries.
func (o *Outbox) Depth() int64 {
	return o.depth.Load()
}

// OldestBufferedSeconds is the age of the oldest buffered event, and 0 when the
// buffer is empty. It backs the gauge that shows how far delivery has fallen
// behind: a depth that holds steady under load is ordinary, an age that keeps
// climbing is not.
//
// The oldest event is the one with the smallest id, because the sender deletes
// in that order. There is no context parameter since Prometheus calls this
// through a GaugeFunc.
//
// A buffer that cannot be read reports NaN and not 0, because 0 is the value
// that means drained: the query's one-second bound is shorter than the busy
// timeout, so what it gives up on is a buffer under the write contention of a
// backlog, which is exactly when the gauge must not read as healthy. Prometheus
// stores NaN, and no alert expression matches it.
func (o *Outbox) OldestBufferedSeconds() float64 {
	// A scrape waits on this query, so it gives up long before the scrape does.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	var createdAt string
	switch err := o.db.QueryRowContext(ctx,
		`SELECT created_at FROM outbox ORDER BY id LIMIT 1`,
	).Scan(&createdAt); {
	case errors.Is(err, sql.ErrNoRows):
		return 0
	case err != nil:
		return math.NaN()
	}
	inserted, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return math.NaN()
	}
	return o.now().Sub(inserted).Seconds()
}

// Ping checks that the buffer answers. It is what the probes report on: a
// collector whose outbox is unusable can neither acknowledge a notification nor
// deliver one.
//
// It reads the outbox table rather than a constant expression. A constant would
// prove that the driver answers and nothing else, while what the probe is asked
// about is whether events can be written down and read back, which a file whose
// schema this build cannot use answers differently.
func (o *Outbox) Ping(ctx context.Context) error {
	// LIMIT 1 rather than a count: the table holds up to a million rows, and a
	// probe runs every few seconds.
	var id int64
	if err := o.db.QueryRowContext(ctx, `SELECT id FROM outbox LIMIT 1`).Scan(&id); err != nil &&
		!errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("pinging the outbox: %w", err)
	}
	return nil
}

// Close releases the handle. Buffered events stay in the file and are picked up
// by the next start.
func (o *Outbox) Close() error {
	if err := o.db.Close(); err != nil {
		return fmt.Errorf("closing the outbox: %w", err)
	}
	return nil
}
