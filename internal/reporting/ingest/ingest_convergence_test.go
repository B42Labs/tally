package ingest_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/b42labs/tally/internal/core/event"
	"github.com/b42labs/tally/internal/reporting/ingest"
	"github.com/b42labs/tally/internal/reporting/registry"
	"github.com/b42labs/tally/internal/reporting/store/storetest"
)

// deadlockDetected is the SQLSTATE Postgres kills one of two transactions that
// wait on each other's locks with. Sorting the resource keys of a batch is what
// keeps it from happening, so an error carrying it is the failure this file is
// about.
const deadlockDetected = "40P01"

// historySteps is how many events the fixed history holds.
const historySteps = 5

// TestIngestConverges holds the pipeline to the property the projection is
// built on: the row a resource ends up with follows from its events, not from
// the order or the batches they arrived in.
func TestIngestConverges(t *testing.T) {
	db := storetest.NewDB(t)
	pipeline := ingest.New(registry.New(), false)

	t.Run("folds every arrival order of one history into the same row", func(t *testing.T) {
		const (
			cloud   = "os-ingest-permutation"
			inOrder = "vol-in-order"
		)

		// One event per batch, so nothing but the projection reconciles the order
		// the events arrive in. The row of in-order ingestion is what every other
		// order has to reach.
		ingestOrder(t, db, pipeline, resource{cloud: cloud, resourceType: "volume", id: inOrder},
			inOrderSteps())
		orders := permutations(historySteps)
		if len(orders) != 120 {
			t.Fatalf("orders = %d, want every ordering of %d events, 120", len(orders), historySteps)
		}
		for _, order := range orders {
			res := resource{cloud: cloud, resourceType: "volume", id: "vol-" + orderName(order)}
			ingestOrder(t, db, pipeline, res, order)
		}

		rows := snapshots(t, db, cloud)
		if len(rows) != len(orders)+1 {
			t.Fatalf("projection rows = %d, want one per order plus the reference, %d",
				len(rows), len(orders)+1)
		}
		want := rows[inOrder]
		for id, row := range rows {
			if row != want {
				t.Errorf("row of %s = %s, want the row of in-order ingestion, %s", id, row, want)
			}
		}
	})

	t.Run("takes concurrent batches over the same keys without deadlocking", func(t *testing.T) {
		const (
			concurrent = "os-ingest-concurrent"
			sequential = "os-ingest-sequential"
			writers    = 4
		)
		ids := []string{"vol-a", "vol-b", "vol-c", "vol-d"}

		// Every writer takes every fourth event of every resource, so all four
		// batches name all four resources, and each writer lists them rotated by
		// one: what fixes the order the locks are taken in is the pipeline, not
		// the order the items were submitted in.
		batches := make([][]json.RawMessage, writers)
		for w := range writers {
			for offset := range ids {
				res := resource{cloud: concurrent, resourceType: "volume", id: ids[(w+offset)%len(ids)]}
				events := history(res)
				for step := w; step < len(events); step += writers {
					batches[w] = append(batches[w], item(t, events[step]))
				}
			}
		}

		outcomes := make([]ingest.Outcome, writers)
		errs := make([]error, writers)
		var wg sync.WaitGroup
		for w := range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[w] = db.Store.WithTx(t.Context(), func(tx pgx.Tx) error {
					var err error
					outcomes[w], err = pipeline.Ingest(t.Context(), tx, batches[w],
						event.SourceCollector, nil)
					return err
				})
			}()
		}
		wg.Wait()

		for w, err := range errs {
			var pgErr *pgconn.PgError
			switch {
			case err == nil:
			case errors.As(err, &pgErr) && pgErr.Code == deadlockDetected:
				t.Fatalf("writer %d deadlocked (SQLSTATE %s): %v", w, deadlockDetected, err)
			default:
				t.Fatalf("writer %d: Ingest() error = %v, want nil", w, err)
			}
			if outcomes[w].Accepted != len(batches[w]) {
				t.Errorf("writer %d accepted %d events, want its whole batch of %d",
					w, outcomes[w].Accepted, len(batches[w]))
			}
		}

		// The same history, delivered one event at a time in the order it
		// happened, is the result the concurrent writers have to converge on.
		for _, id := range ids {
			ingestOrder(t, db, pipeline, resource{cloud: sequential, resourceType: "volume", id: id},
				inOrderSteps())
		}

		got, want := snapshots(t, db, concurrent), snapshots(t, db, sequential)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("projection rows after concurrent ingestion = %v, want %v", got, want)
		}
	})

	t.Run("takes concurrent batches over the same events without deadlocking", func(t *testing.T) {
		const (
			cloud   = "os-ingest-overlapping"
			writers = 4
		)
		ids := []string{"vol-a", "vol-b", "vol-c", "vol-d"}

		// Every writer submits the whole history of every resource, listed in an
		// order of its own. Two transactions inserting the same event wait on
		// each other's row, so what keeps them from waiting in a cycle is the
		// pipeline sorting the batch, not the order it was submitted in.
		var items []json.RawMessage
		for _, id := range ids {
			for _, e := range history(resource{cloud: cloud, resourceType: "volume", id: id}) {
				items = append(items, item(t, e))
			}
		}
		batches := make([][]json.RawMessage, writers)
		for w := range writers {
			// A starting point of its own per writer, reversed for half of them:
			// no two writers list the shared events in the same order.
			rotate := w * len(items) / writers
			ordered := append(slices.Clone(items[rotate:]), items[:rotate]...)
			if w%2 == 1 {
				slices.Reverse(ordered)
			}
			batches[w] = ordered
		}

		outcomes := make([]ingest.Outcome, writers)
		errs := make([]error, writers)
		var wg sync.WaitGroup
		for w := range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[w] = db.Store.WithTx(t.Context(), func(tx pgx.Tx) error {
					var err error
					outcomes[w], err = pipeline.Ingest(t.Context(), tx, batches[w],
						event.SourceCollector, nil)
					return err
				})
			}()
		}
		wg.Wait()

		accepted := 0
		for w, err := range errs {
			var pgErr *pgconn.PgError
			switch {
			case err == nil:
			case errors.As(err, &pgErr) && pgErr.Code == deadlockDetected:
				t.Fatalf("writer %d deadlocked (SQLSTATE %s): %v", w, deadlockDetected, err)
			default:
				t.Fatalf("writer %d: Ingest() error = %v, want nil", w, err)
			}
			if got := outcomes[w].Accepted + outcomes[w].Duplicates; got != len(items) {
				t.Errorf("writer %d took %d of its %d items, want all of them",
					w, got, len(items))
			}
			accepted += outcomes[w].Accepted
		}
		if accepted != len(items) {
			t.Errorf("events stored across %d writers = %d, want the %d distinct ones",
				writers, accepted, len(items))
		}
	})
}

// history is the five events one resource lives through, in the order they
// happened: it is created, resized, paused, handed to another project, and
// deleted. The event ids carry the cloud, so the same history can run in a
// second cloud without meeting the first one's events as duplicates.
func history(res resource) []event.Event {
	prefix := res.cloud + "-" + res.id
	at := func(step int) time.Time { return createTime.Add(time.Duration(step) * time.Hour) }

	// The resource changes hands, which every event from then on reports.
	transfer := res.event(prefix+"-transfer", "volume.update", at(3), payloadOf("paused", nil))
	transfer.ProjectID = projectB
	remove := res.event(prefix+"-delete", "volume.delete", at(4), event.PayloadEnvelope{})
	remove.ProjectID = projectB

	return []event.Event{
		res.event(prefix+"-create", "volume.create", at(0), payloadOf("available", volumeSize(10))),
		res.event(prefix+"-resize", "volume.resize", at(1), payloadOf("available", volumeSize(20))),
		res.event(prefix+"-pause", "volume.update", at(2), payloadOf("paused", nil)),
		transfer,
		remove,
	}
}

// ingestOrder delivers the history of res one event per batch, in the order the
// steps name, each batch in a transaction of its own.
func ingestOrder(t *testing.T, db storetest.DB, p *ingest.Pipeline, res resource, order []int) {
	t.Helper()

	events := history(res)
	for _, step := range order {
		assertOutcome(t, batch(t, db, p, []json.RawMessage{item(t, events[step])},
			event.SourceCollector, nil), ingest.Outcome{Accepted: 1})
	}
}

// inOrderSteps is the history delivered in the order it happened.
func inOrderSteps() []int {
	steps := make([]int, historySteps)
	for i := range steps {
		steps[i] = i
	}
	return steps
}

// permutations returns every ordering of the numbers 0..n-1, which is every
// order the events of one history can arrive in.
func permutations(n int) [][]int {
	if n == 0 {
		return [][]int{{}}
	}

	var all [][]int
	for _, shorter := range permutations(n - 1) {
		for pos := 0; pos <= len(shorter); pos++ {
			all = append(all, slices.Insert(slices.Clone(shorter), pos, n-1))
		}
	}
	return all
}

// orderName renders an order as the digits it visits, so the resource id of a
// failing row names the order that produced it.
func orderName(order []int) string {
	var name strings.Builder
	for _, step := range order {
		name.WriteString(strconv.Itoa(step))
	}
	return name.String()
}

// snapshots reads the projection rows of cloud as the JSON document Postgres
// renders them as, without the three key columns. What is left is everything the
// fold derived, so the same history compares as one string across resources and
// clouds.
func snapshots(t *testing.T, db storetest.DB, cloud string) map[string]string {
	t.Helper()

	rows, err := db.Store.Pool().Query(t.Context(),
		`SELECT resource_id,
		        (to_jsonb(c) - 'cloud'::text - 'resource_type'::text - 'resource_id'::text)::text
		 FROM current_resources c WHERE cloud = $1`, cloud)
	if err != nil {
		t.Fatalf("reading the projection rows of %s: %v", cloud, err)
	}
	defer rows.Close()

	found := map[string]string{}
	for rows.Next() {
		var id, snapshot string
		if err := rows.Scan(&id, &snapshot); err != nil {
			t.Fatalf("scanning a projection row: %v", err)
		}
		found[id] = snapshot
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the projection rows of %s: %v", cloud, err)
	}
	return found
}
