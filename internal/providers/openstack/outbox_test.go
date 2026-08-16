package openstack

import (
	"bytes"
	"context"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// openOutboxAt opens the buffer at path and closes it when the test ends.
// Closing twice is harmless, so a test may close it early to reopen the file.
func openOutboxAt(t *testing.T, path string) *Outbox {
	t.Helper()

	box, err := OpenOutbox(path)
	if err != nil {
		t.Fatalf("OpenOutbox() error = %v, want nil", err)
	}
	t.Cleanup(func() {
		if err := box.Close(); err != nil {
			t.Errorf("Close() error = %v, want nil", err)
		}
	})
	return box
}

// newOutbox opens an empty buffer on a fresh file.
func newOutbox(t *testing.T) *Outbox {
	t.Helper()

	return openOutboxAt(t, filepath.Join(t.TempDir(), "outbox.db"))
}

// insertEvent buffers one event, failing the test if the insert does not commit.
func insertEvent(t *testing.T, box *Outbox, eventJSON string) {
	t.Helper()

	if err := box.Insert(context.Background(), []byte(eventJSON)); err != nil {
		t.Fatalf("Insert(%s) error = %v, want nil", eventJSON, err)
	}
}

// readBatch reads up to limit buffered events.
func readBatch(t *testing.T, box *Outbox, limit int) []Row {
	t.Helper()

	batch, err := box.Batch(context.Background(), limit)
	if err != nil {
		t.Fatalf("Batch(%d) error = %v, want nil", limit, err)
	}
	return batch
}

// batchIDs lists a batch's ids, which is what DeleteBatch is given.
func batchIDs(batch []Row) []int64 {
	list := make([]int64, len(batch))
	for i, row := range batch {
		list[i] = row.ID
	}
	return list
}

// TestOpenOutboxAppliesEveryPragma guards the settings the durability of an
// acknowledgement rests on. They travel in the DSN so that every connection the
// pool opens carries them, and this reads them back through the pool.
func TestOpenOutboxAppliesEveryPragma(t *testing.T) {
	box := newOutbox(t)

	tests := []struct {
		pragma string
		want   string
	}{
		{pragma: "journal_mode", want: "wal"},
		{pragma: "synchronous", want: "2"}, // 2 is FULL, the WAL default 1 is NORMAL
		{pragma: "busy_timeout", want: "5000"},
		// 2 is incremental. SQLite fixes the mode when the file is created and
		// changing it afterwards means a VACUUM that rewrites the whole file, so
		// this one is a decision every deployed volume is stuck with.
		{pragma: "auto_vacuum", want: "2"},
	}

	for _, tc := range tests {
		t.Run(tc.pragma, func(t *testing.T) {
			var got string
			if err := box.db.QueryRow(`PRAGMA ` + tc.pragma).Scan(&got); err != nil {
				t.Fatalf("PRAGMA %s error = %v, want nil", tc.pragma, err)
			}
			if got != tc.want {
				t.Errorf("PRAGMA %s = %q, want %q", tc.pragma, got, tc.want)
			}
		})
	}
}

// TestOpenOutboxCarriesTheSchemaVersion covers the upgrade path a persistent
// volume has: CREATE TABLE IF NOT EXISTS is a no-op against a table of any other
// shape, because SQLite compares nothing but the name, so the version in the
// file is what makes a mismatch a startup failure instead of a failure per
// message.
func TestOpenOutboxCarriesTheSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outbox.db")
	box := openOutboxAt(t, path)

	var version int
	if err := box.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("PRAGMA user_version error = %v, want nil", err)
	}
	if version != outboxSchemaVersion {
		t.Errorf("PRAGMA user_version = %d, want %d", version, outboxSchemaVersion)
	}

	// A file another build's schema wrote, in either direction.
	if _, err := box.db.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatalf("setting the schema version: %v", err)
	}
	if err := box.Close(); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}

	reopened, err := OpenOutbox(path)
	if err == nil {
		_ = reopened.Close()
		t.Fatal("OpenOutbox() error = nil, want it to refuse a schema this build does not know")
	}
	if !strings.Contains(err.Error(), "99") {
		t.Errorf("OpenOutbox() error = %q, want it to name the version the file carries", err)
	}
}

func TestOpenOutboxRejectsAnUnusablePath(t *testing.T) {
	// A directory that was never created: sql.Open would accept this, so the
	// error can only come from the statement OpenOutbox runs afterwards.
	path := filepath.Join(t.TempDir(), "missing", "outbox.db")

	box, err := OpenOutbox(path)
	if err == nil {
		_ = box.Close()
		t.Fatal("OpenOutbox() error = nil, want an error")
	}
	if box != nil {
		t.Errorf("OpenOutbox() = %v, want nil alongside the error", box)
	}
}

func TestInsertAndBatchRoundTripEventsInIDOrder(t *testing.T) {
	box := newOutbox(t)
	events := []string{
		`{"event_id":"first","event_type":"compute.instance.create.end"}`,
		`{"event_id":"second","event_type":"volume.create.end"}`,
		`{"event_id":"third","event_type":"image.delete"}`,
	}
	for _, event := range events {
		insertEvent(t, box, event)
	}

	batch := readBatch(t, box, 10)

	if len(batch) != len(events) {
		t.Fatalf("Batch() returned %d rows, want %d", len(batch), len(events))
	}
	for i, row := range batch {
		if !bytes.Equal(row.EventJSON, []byte(events[i])) {
			t.Errorf("row %d EventJSON = %s, want %s", i, row.EventJSON, events[i])
		}
		if i > 0 && row.ID <= batch[i-1].ID {
			t.Errorf("row %d id = %d, want it above the previous id %d", i, row.ID, batch[i-1].ID)
		}
	}
}

// TestBatchCapsTheResultAtTheLimit pins the bound the sender's POST size rests
// on: a buffer far deeper than one batch still yields one batch, oldest first.
func TestBatchCapsTheResultAtTheLimit(t *testing.T) {
	box := newOutbox(t)
	for _, event := range []string{`{"event_id":"a"}`, `{"event_id":"b"}`, `{"event_id":"c"}`} {
		insertEvent(t, box, event)
	}

	batch := readBatch(t, box, 2)

	if len(batch) != 2 {
		t.Fatalf("Batch(2) returned %d rows, want 2", len(batch))
	}
	if got := string(batch[0].EventJSON); got != `{"event_id":"a"}` {
		t.Errorf("first row = %s, want the oldest event", got)
	}
	if got := string(batch[1].EventJSON); got != `{"event_id":"b"}` {
		t.Errorf("second row = %s, want the second oldest event", got)
	}
}

// TestBatchOnAnEmptyOutboxReturnsNoRows covers the state the sender spends most
// of its time in: nothing buffered is not an error.
func TestBatchOnAnEmptyOutboxReturnsNoRows(t *testing.T) {
	box := newOutbox(t)

	batch, err := box.Batch(context.Background(), 500)
	if err != nil {
		t.Fatalf("Batch() error = %v, want nil", err)
	}
	if batch == nil {
		t.Error("Batch() = nil, want an empty slice")
	}
	if len(batch) != 0 {
		t.Errorf("Batch() returned %d rows, want 0", len(batch))
	}
	if got := box.Depth(); got != 0 {
		t.Errorf("Depth() = %d, want 0", got)
	}
	if got := box.OldestBufferedSeconds(); got != 0 {
		t.Errorf("OldestBufferedSeconds() = %v, want 0", got)
	}
}

// TestDeleteBatchRemovesOnlyTheGivenIDs is what keeps the collector from losing
// an event: the consumer keeps inserting while a batch is in flight, and the
// delete that follows the 200 must not reach those rows.
func TestDeleteBatchRemovesOnlyTheGivenIDs(t *testing.T) {
	box := newOutbox(t)
	for _, event := range []string{`{"event_id":"a"}`, `{"event_id":"b"}`, `{"event_id":"c"}`} {
		insertEvent(t, box, event)
	}
	delivered := readBatch(t, box, 500)
	insertEvent(t, box, `{"event_id":"d"}`)

	if err := box.DeleteBatch(context.Background(), batchIDs(delivered)); err != nil {
		t.Fatalf("DeleteBatch() error = %v, want nil", err)
	}

	remaining := readBatch(t, box, 500)
	if len(remaining) != 1 {
		t.Fatalf("Batch() returned %d rows after the delete, want the one inserted meanwhile", len(remaining))
	}
	if got := string(remaining[0].EventJSON); got != `{"event_id":"d"}` {
		t.Errorf("remaining row = %s, want the event inserted while the batch was in flight", got)
	}
	if got := box.Depth(); got != 1 {
		t.Errorf("Depth() = %d, want 1", got)
	}
}

// TestDeleteBatchReturnsTheFreedPagesToTheFilesystem covers what the file costs
// after an outage. In incremental mode SQLite moves the pages a delete frees
// onto the freelist and hands them back only when PRAGMA incremental_vacuum
// runs, so without that call a buffer that filled toward BufferMaxEvents leaves
// the file at its peak size forever: the volume stays full once the backlog has
// drained, and the next incident hits the volume limit instead of the bound.
func TestDeleteBatchReturnsTheFreedPagesToTheFilesystem(t *testing.T) {
	box := newOutbox(t)
	// Enough rows that the freed pages are worth measuring against a fresh file's
	// handful of them.
	event := `{"event_id":"a","payload":{"filler":"` + strings.Repeat("z", 4096) + `"}}`
	for range 200 {
		insertEvent(t, box, event)
	}
	buffered := readBatch(t, box, 500)
	peak := pageCount(t, box)

	if err := box.DeleteBatch(context.Background(), batchIDs(buffered)); err != nil {
		t.Fatalf("DeleteBatch() error = %v, want nil", err)
	}

	if drained := pageCount(t, box); drained >= peak {
		t.Errorf("the file holds %d pages after the delete, want fewer than the %d it peaked at",
			drained, peak)
	}
	// What one pass leaves the next one takes, so a drained buffer ends up with no
	// pages waiting on the freelist at all.
	if got := freelistCount(t, box); got != 0 {
		t.Errorf("%d pages are on the freelist, want them returned to the filesystem", got)
	}
}

// pageCount is how many pages the file holds, which is its size divided by the
// page size.
func pageCount(t *testing.T, box *Outbox) int {
	t.Helper()

	return readIntPragma(t, box, "page_count")
}

// freelistCount is how many of those pages are free and waiting to be reclaimed.
func freelistCount(t *testing.T, box *Outbox) int {
	t.Helper()

	return readIntPragma(t, box, "freelist_count")
}

func readIntPragma(t *testing.T, box *Outbox, pragma string) int {
	t.Helper()

	var value int
	if err := box.db.QueryRow(`PRAGMA ` + pragma).Scan(&value); err != nil {
		t.Fatalf("PRAGMA %s error = %v, want nil", pragma, err)
	}
	return value
}

func TestDeleteBatchIgnoresRowsItNoLongerHolds(t *testing.T) {
	box := newOutbox(t)
	insertEvent(t, box, `{"event_id":"a"}`)

	t.Run("no ids delete nothing", func(t *testing.T) {
		if err := box.DeleteBatch(context.Background(), nil); err != nil {
			t.Fatalf("DeleteBatch(nil) error = %v, want nil", err)
		}
		if got := box.Depth(); got != 1 {
			t.Errorf("Depth() = %d, want the row untouched at 1", got)
		}
	})

	// A repeated delete must leave the depth on the row count rather than
	// counting rows that are already gone a second time.
	t.Run("unknown ids leave the depth alone", func(t *testing.T) {
		if err := box.DeleteBatch(context.Background(), []int64{4711, 4712}); err != nil {
			t.Fatalf("DeleteBatch() error = %v, want nil", err)
		}
		if got := box.Depth(); got != 1 {
			t.Errorf("Depth() = %d, want 1", got)
		}
	})
}

// TestDepthIsSeededFromTheFile covers a restart: the events a crashed collector
// left behind count against the backpressure bound of the one that replaces it.
func TestDepthIsSeededFromTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outbox.db")
	box := openOutboxAt(t, path)
	for _, event := range []string{`{"event_id":"a"}`, `{"event_id":"b"}`} {
		insertEvent(t, box, event)
	}
	if err := box.Close(); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}

	reopened := openOutboxAt(t, path)

	if got := reopened.Depth(); got != 2 {
		t.Errorf("Depth() = %d, want the 2 rows the file already held", got)
	}
}

func TestDepthTracksInsertsAndDeletes(t *testing.T) {
	box := newOutbox(t)

	insertEvent(t, box, `{"event_id":"a"}`)
	insertEvent(t, box, `{"event_id":"b"}`)
	if got := box.Depth(); got != 2 {
		t.Fatalf("Depth() = %d after two inserts, want 2", got)
	}

	batch := readBatch(t, box, 1)
	if err := box.DeleteBatch(context.Background(), batchIDs(batch)); err != nil {
		t.Fatalf("DeleteBatch() error = %v, want nil", err)
	}

	if got := box.Depth(); got != 1 {
		t.Errorf("Depth() = %d after deleting one of two rows, want 1", got)
	}
}

// TestOldestBufferedSecondsReportsTheOldestRow pins the gauge that tells an
// operator how far delivery has fallen behind. The clock is pinned so the age is
// exact, and a newer row must not shorten it.
func TestOldestBufferedSecondsReportsTheOldestRow(t *testing.T) {
	box := newOutbox(t)
	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	box.now = func() time.Time { return base }
	insertEvent(t, box, `{"event_id":"a"}`)
	box.now = func() time.Time { return base.Add(30 * time.Second) }
	insertEvent(t, box, `{"event_id":"b"}`)
	box.now = func() time.Time { return base.Add(90 * time.Second) }

	if got := box.OldestBufferedSeconds(); got != 90 {
		t.Errorf("OldestBufferedSeconds() = %v, want 90 for the oldest of the two rows", got)
	}

	// Deleting the oldest row hands the age to the row behind it.
	oldest := readBatch(t, box, 1)
	if err := box.DeleteBatch(context.Background(), batchIDs(oldest)); err != nil {
		t.Fatalf("DeleteBatch() error = %v, want nil", err)
	}
	if got := box.OldestBufferedSeconds(); got != 60 {
		t.Errorf("OldestBufferedSeconds() = %v, want 60 once the older row is delivered", got)
	}

	remaining := readBatch(t, box, 500)
	if err := box.DeleteBatch(context.Background(), batchIDs(remaining)); err != nil {
		t.Fatalf("DeleteBatch() error = %v, want nil", err)
	}
	if got := box.OldestBufferedSeconds(); got != 0 {
		t.Errorf("OldestBufferedSeconds() = %v, want 0 for an empty buffer", got)
	}
}

// TestOutboxRowsSurviveCloseAndReopen is the property the acknowledgement is
// traded for: what the collector wrote down is still there afterwards, unchanged.
func TestOutboxRowsSurviveCloseAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outbox.db")
	events := []string{
		`{"event_id":"first","quantity":"2.5"}`,
		`{"event_id":"second","quantity":"0.000001"}`,
	}

	box := openOutboxAt(t, path)
	for _, event := range events {
		insertEvent(t, box, event)
	}
	if err := box.Close(); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}

	batch := readBatch(t, openOutboxAt(t, path), 500)

	if len(batch) != len(events) {
		t.Fatalf("Batch() returned %d rows after the reopen, want %d", len(batch), len(events))
	}
	for i, row := range batch {
		if !bytes.Equal(row.EventJSON, []byte(events[i])) {
			t.Errorf("row %d EventJSON = %s, want %s", i, row.EventJSON, events[i])
		}
	}
}

func TestPingFailsOnAClosedOutbox(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outbox.db")
	box := openOutboxAt(t, path)

	if err := box.Ping(context.Background()); err != nil {
		t.Fatalf("Ping() error = %v, want nil on an open outbox", err)
	}

	if err := box.Close(); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}
	if err := box.Ping(context.Background()); err == nil {
		t.Error("Ping() error = nil, want an error on a closed outbox")
	}
}

// TestPingReadsTheOutboxTable is what the probe behind it is worth: a driver
// that answers a constant expression proves nothing about the buffer, and a
// readiness probe that passes on a file the collector cannot store an event in
// is blind to the one class of failure it exists to catch.
func TestPingReadsTheOutboxTable(t *testing.T) {
	box := newOutbox(t)
	if _, err := box.db.Exec(`DROP TABLE outbox`); err != nil {
		t.Fatalf("dropping the outbox table: %v", err)
	}

	if err := box.Ping(context.Background()); err == nil {
		t.Error("Ping() error = nil, want an error once the outbox table is gone")
	}
}

// TestOldestBufferedSecondsReportsNaNWhenTheBufferCannotBeRead keeps the lag
// gauge from going quiet at the worst moment: 0 is the value that means drained,
// so a buffer that cannot be read has to report something an alert does not
// match rather than the healthiest value there is.
func TestOldestBufferedSecondsReportsNaNWhenTheBufferCannotBeRead(t *testing.T) {
	box := newOutbox(t)
	insertEvent(t, box, `{"event_id":"a"}`)
	if err := box.Close(); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}

	if got := box.OldestBufferedSeconds(); !math.IsNaN(got) {
		t.Errorf("OldestBufferedSeconds() = %v, want NaN for a buffer that cannot be read", got)
	}
}
