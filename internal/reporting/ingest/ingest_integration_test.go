package ingest_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/b42labs/tally/internal/core/event"
	"github.com/b42labs/tally/internal/reporting/ingest"
	"github.com/b42labs/tally/internal/reporting/projection"
	"github.com/b42labs/tally/internal/reporting/registry"
	"github.com/b42labs/tally/internal/reporting/store/storetest"
)

// Every fixture reports the platform the migration chain seeds the resource
// types of, so the size checks meet the schemas an operator would meet.
const (
	platform = "openstack"
	projectA = "project-a"
	projectB = "project-b"
)

// The instants the fixtures use. They are whole seconds, so what Postgres
// stores compares equal to what the test passed in.
var (
	createTime = time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	updateTime = createTime.Add(time.Hour)
	deleteTime = createTime.Add(2 * time.Hour)
)

// TestIngest drives the pipeline against a real database. The subtests share
// one, so each of them works on a cloud and resource ids of its own.
func TestIngest(t *testing.T) {
	db := storetest.NewDB(t)
	lax := ingest.New(registry.New(), false, nil)
	strict := ingest.New(registry.New(), true, nil)

	t.Run("stores the source the caller names, not the one the item claims", func(t *testing.T) {
		res := resource{cloud: "os-ingest-source", resourceType: "volume", id: "vol-source"}
		claimed := res.event("vol-source-create", "volume.create", createTime,
			payloadOf("available", volumeSize(10)))
		claimed.Source = event.SourceReconciliation

		out := batch(t, db, lax, []json.RawMessage{item(t, claimed)}, event.SourceCollector, nil)

		assertOutcome(t, out, ingest.Outcome{Accepted: 1})
		if got := eventSource(t, db, claimed.EventID); got != string(event.SourceCollector) {
			t.Errorf("stored source = %q, want %q", got, event.SourceCollector)
		}
	})

	t.Run("reports every refused item of a mixed batch by index", func(t *testing.T) {
		res := resource{cloud: "os-ingest-mixed", resourceType: "volume", id: "vol-mixed"}
		bad := resource{cloud: res.cloud, resourceType: "instance", id: "inst-mixed"}

		malformed := item(t, res.event("vol-mixed-malformed", "volume-create", createTime,
			payloadOf("available", volumeSize(10))))
		oversized := item(t, bad.event("inst-mixed-create", "instance.create", createTime,
			payloadOf("active", map[string]any{
				"vcpus": "four", "ram_gb": 8, "disk_gb": 40, "flavor": "m1.large",
			})))
		items := []json.RawMessage{
			item(t, res.event("vol-mixed-create", "volume.create", createTime,
				payloadOf("available", volumeSize(10)))),
			malformed,
			oversized,
			item(t, res.event("vol-mixed-resize", "volume.resize", updateTime,
				payloadOf("available", volumeSize(20)))),
		}

		out := batch(t, db, lax, items, event.SourceCollector, nil)

		if out.Accepted != 2 || out.Duplicates != 0 {
			t.Errorf("Ingest() accepted %d and %d duplicates, want 2 and 0", out.Accepted, out.Duplicates)
		}
		if len(out.Rejected) != 2 {
			t.Fatalf("Ingest() rejected %v, want the malformed and the oversized item", out.Rejected)
		}
		assertRejected(t, out.Rejected[0], 1, "vol-mixed-malformed",
			`schema: event_type: "volume-create" must match ^[a-z0-9_]+(\.[a-z0-9_]+)+$`)
		assertRejected(t, out.Rejected[1], 2, "inst-mixed-create", "size_schema: ")

		assertDeadLettered(t, db, out.Rejected[0].Reason, malformed)
		assertDeadLettered(t, db, out.Rejected[1].Reason, oversized)
		if got := deadLetters(t, db, res.cloud); got != 2 {
			t.Errorf("dead-letter rows = %d, want one per refused item, 2", got)
		}
	})

	t.Run("counts a replayed batch as duplicates and leaves the rows alone", func(t *testing.T) {
		res := resource{cloud: "os-ingest-replay", resourceType: "volume", id: "vol-replay"}
		other := resource{cloud: res.cloud, resourceType: "volume", id: "vol-replay-second"}
		items := []json.RawMessage{
			item(t, res.event("vol-replay-create", "volume.create", createTime,
				payloadOf("available", volumeSize(10)))),
			item(t, res.event("vol-replay-resize", "volume.resize", updateTime,
				payloadOf("available", volumeSize(20)))),
			item(t, other.event("vol-replay-second-create", "volume.create", createTime,
				payloadOf("available", volumeSize(5)))),
		}

		assertOutcome(t, batch(t, db, lax, items, event.SourceCollector, nil), ingest.Outcome{Accepted: 3})
		before, storedBefore := resourceRows(t, db, res.cloud), storedEvents(t, db, res.cloud)

		assertOutcome(t, batch(t, db, lax, items, event.SourceCollector, nil), ingest.Outcome{Duplicates: 3})

		if got := storedEvents(t, db, res.cloud); got != storedBefore {
			t.Errorf("events rows = %d, want the %d the first batch stored", got, storedBefore)
		}
		if after := resourceRows(t, db, res.cloud); !reflect.DeepEqual(after, before) {
			t.Errorf("projection rows = %v, want them unchanged at %v", after, before)
		}
	})

	t.Run("dead-letters what the schema refuses and keeps its siblings", func(t *testing.T) {
		res := resource{cloud: "os-ingest-schema", resourceType: "volume", id: "vol-schema"}
		// The timestamp is a number rather than an instant, so the item does not
		// even decode. The event id is still readable, and reported.
		undecodable := json.RawMessage(`{"event_id": "vol-schema-undecodable", "timestamp": 42,
			"event_type": "volume.create", "platform": "openstack", "cloud": "os-ingest-schema",
			"resource_type": "volume", "resource_id": "vol-schema", "project_id": "project-a",
			"payload": {"state": "available"}}`)
		// A create has to report the size the resource starts with.
		sizeless := item(t, res.event("vol-schema-sizeless", "volume.create", createTime,
			payloadOf("available", nil)))
		// No payload at all, which every event but a delete needs one of.
		payloadless := json.RawMessage(`{"event_id": "vol-schema-payloadless",
			"timestamp": "2026-04-01T10:00:00Z", "event_type": "volume.update",
			"platform": "openstack", "cloud": "os-ingest-schema", "resource_type": "volume",
			"resource_id": "vol-schema", "project_id": "project-a"}`)
		items := []json.RawMessage{
			undecodable,
			item(t, res.event("vol-schema-create", "volume.create", createTime,
				payloadOf("available", volumeSize(10)))),
			sizeless,
			payloadless,
			item(t, res.event("vol-schema-resize", "volume.resize", updateTime,
				payloadOf("available", volumeSize(20)))),
		}

		out := batch(t, db, lax, items, event.SourceCollector, nil)

		if out.Accepted != 2 {
			t.Errorf("Ingest() accepted %d, want the 2 valid siblings", out.Accepted)
		}
		if len(out.Rejected) != 3 {
			t.Fatalf("Ingest() rejected %v, want the three items the schema refuses", out.Rejected)
		}
		assertRejected(t, out.Rejected[0], 0, "vol-schema-undecodable", "schema: ")
		assertRejected(t, out.Rejected[1], 2, "vol-schema-sizeless",
			"schema: payload.size: required on create events")
		assertRejected(t, out.Rejected[2], 3, "vol-schema-payloadless",
			"schema: payload.state: required on every event except delete")

		for i, raw := range []json.RawMessage{undecodable, sizeless, payloadless} {
			assertDeadLettered(t, db, out.Rejected[i].Reason, raw)
		}
	})

	t.Run("refuses an item carrying a NUL and keeps its siblings", func(t *testing.T) {
		res := resource{cloud: "os-ingest-nul", resourceType: "volume", id: "vol-nul"}
		// PostgreSQL holds a NUL in neither a text column nor a jsonb document,
		// so an item spelling one has to be refused on its own: reaching the
		// database with it fails every other item of the batch as well.
		inPayload := json.RawMessage(`{"event_id": "vol-nul-payload",
			"timestamp": "2026-04-01T09:00:00Z", "event_type": "volume.create",
			"platform": "openstack", "cloud": "os-ingest-nul", "resource_type": "volume",
			"resource_id": "vol-nul", "project_id": "project-a",
			"payload": {"state": "available", "size": {"size_gb": 10, "type": "ss\u0000d"}}}`)
		inField := json.RawMessage(`{"event_id": "vol-nul-field",
			"timestamp": "2026-04-01T09:00:00Z", "event_type": "volume.create",
			"platform": "openstack", "cloud": "os-ingest-nul", "resource_type": "volume",
			"resource_id": "vol-nul\u0000", "project_id": "project-a",
			"payload": {"state": "available", "size": {"size_gb": 10, "type": "ssd"}}}`)
		items := []json.RawMessage{
			inPayload,
			item(t, res.event("vol-nul-create", "volume.create", createTime,
				payloadOf("available", volumeSize(10)))),
			inField,
		}

		out := batch(t, db, lax, items, event.SourceCollector, nil)

		if out.Accepted != 1 {
			t.Errorf("Ingest() accepted %d, want the one sibling that carries no NUL", out.Accepted)
		}
		if len(out.Rejected) != 2 {
			t.Fatalf("Ingest() rejected %v, want both items carrying a NUL", out.Rejected)
		}
		assertRejected(t, out.Rejected[0], 0, "vol-nul-payload", "schema: the item carries a NUL")
		assertRejected(t, out.Rejected[1], 2, "vol-nul-field", "schema: the item carries a NUL")
		// The item itself cannot be stored either, so the row names its size
		// rather than holding bytes the column would refuse.
		raws := deadLetterRaws(t, db, out.Rejected[0].Reason)
		if len(raws) != 2 {
			t.Fatalf("dead-letter rows = %v, want one per refused item", raws)
		}
		for _, raw := range raws {
			if !strings.Contains(raw, `"not_stored"`) {
				t.Errorf("dead-lettered document = %s, want the note replacing the item", raw)
			}
		}
	})

	t.Run("keeps a refused item no larger than the dead-letter budget", func(t *testing.T) {
		res := resource{cloud: "os-ingest-oversized", resourceType: "volume", id: "vol-oversized"}
		// The event type is refused whatever else the item carries, and the
		// provider data pushes the item past what a dead letter keeps. The
		// refusal names the type, so no other subtest shares the reason.
		payload := payloadOf("available", nil)
		payload.Provider = map[string]any{"dump": strings.Repeat("x", 128<<10)}
		huge := res.event("vol-oversized-create", "volume#create", createTime, payload)

		out := batch(t, db, lax, []json.RawMessage{item(t, huge)}, event.SourceCollector, nil)

		if len(out.Rejected) != 1 {
			t.Fatalf("Ingest() rejected %v, want the item naming a bad event type", out.Rejected)
		}
		raws := deadLetterRaws(t, db, out.Rejected[0].Reason)
		if len(raws) != 1 {
			t.Fatalf("dead-letter rows = %v, want one", raws)
		}
		if !strings.Contains(raws[0], `"not_stored"`) {
			t.Errorf("dead-lettered document = %s, want the note replacing the item", raws[0])
		}
		if len(raws[0]) > 128 {
			t.Errorf("dead-lettered document is %d bytes, want the item replaced by a note",
				len(raws[0]))
		}
	})

	t.Run("audits a scope violation instead of dead-lettering it", func(t *testing.T) {
		const (
			credentialCloud = "os-ingest-scope"
			foreignCloud    = "os-ingest-scope-foreign"
		)
		res := resource{cloud: credentialCloud, resourceType: "volume", id: "vol-scope"}
		foreign := resource{cloud: foreignCloud, resourceType: "volume", id: "vol-scope-foreign"}

		foreignCloudEvent := foreign.event("vol-scope-foreign-cloud", "volume.create", createTime,
			payloadOf("available", volumeSize(10)))
		foreignPlatformEvent := res.event("vol-scope-foreign-platform", "volume.create", createTime,
			payloadOf("available", volumeSize(10)))
		foreignPlatformEvent.Platform = "aws"
		items := []json.RawMessage{
			item(t, foreignCloudEvent),
			item(t, foreignPlatformEvent),
			item(t, res.event("vol-scope-create", "volume.create", createTime,
				payloadOf("available", volumeSize(10)))),
		}

		out := batch(t, db, lax, items, event.SourceCollector,
			&ingest.Scope{Platform: platform, Cloud: credentialCloud})

		if out.Accepted != 1 {
			t.Errorf("Ingest() accepted %d, want the 1 event inside the scope", out.Accepted)
		}
		if len(out.Rejected) != 2 {
			t.Fatalf("Ingest() rejected %v, want both events outside the scope", out.Rejected)
		}
		assertRejected(t, out.Rejected[0], 0, foreignCloudEvent.EventID, "scope")
		assertRejected(t, out.Rejected[1], 1, foreignPlatformEvent.EventID, "scope")

		// A bare context carries no credential, so the log names the system.
		for _, e := range []event.Event{foreignCloudEvent, foreignPlatformEvent} {
			rows := auditRows(t, db, e.EventID)
			if len(rows) != 1 {
				t.Fatalf("audit rows of %s = %v, want exactly one", e.EventID, rows)
			}
			got := rows[0]
			if got.action != "events.scope_violation" || got.objectType != "events" {
				t.Errorf("audit row = %s on %s, want events.scope_violation on events",
					got.action, got.objectType)
			}
			if got.actor != "internal" {
				t.Errorf("audit actor = %q, want %q", got.actor, "internal")
			}
			want := map[string]any{
				"credential_platform": platform,
				"credential_cloud":    credentialCloud,
				"event_platform":      e.Platform,
				"event_cloud":         e.Cloud,
			}
			if !reflect.DeepEqual(got.details, want) {
				t.Errorf("audit details = %v, want %v", got.details, want)
			}
		}

		// Decision D5: the event body of a scope violation is not kept.
		if got := deadLetters(t, db, foreignCloud); got != 0 {
			t.Errorf("dead-letter rows = %d, want none for a scope violation", got)
		}
		if got := deadLetters(t, db, credentialCloud); got != 0 {
			t.Errorf("dead-letter rows = %d, want none for a scope violation", got)
		}
	})

	t.Run("dead-letters a size the registered schema refuses", func(t *testing.T) {
		res := resource{cloud: "os-ingest-size", resourceType: "instance", id: "inst-size"}
		// The seeded (openstack, instance) schema takes vcpus as an integer.
		refused := item(t, res.event("inst-size-create", "instance.create", createTime,
			payloadOf("active", map[string]any{
				"vcpus": "four", "ram_gb": 8, "disk_gb": 40, "flavor": "m1.large",
			})))

		out := batch(t, db, lax, []json.RawMessage{refused}, event.SourceCollector, nil)

		if out.Accepted != 0 || len(out.Rejected) != 1 {
			t.Fatalf("Ingest() = %+v, want the event refused", out)
		}
		assertRejected(t, out.Rejected[0], 0, "inst-size-create", "size_schema: ")
		assertDeadLettered(t, db, out.Rejected[0].Reason, refused)
	})

	t.Run("refuses an unregistered resource type in strict mode only", func(t *testing.T) {
		res := resource{cloud: "os-ingest-unregistered", resourceType: "black_hole", id: "bh-1"}
		accepted := item(t, res.event("bh-1-create", "black_hole.create", createTime,
			payloadOf("active", map[string]any{"mass_kg": 12})))
		refused := item(t, res.event("bh-1-resize", "black_hole.resize", updateTime,
			payloadOf("active", map[string]any{"mass_kg": 24})))

		assertOutcome(t, batch(t, db, lax, []json.RawMessage{accepted}, event.SourceCollector, nil),
			ingest.Outcome{Accepted: 1})

		out := batch(t, db, strict, []json.RawMessage{refused}, event.SourceCollector, nil)

		if out.Accepted != 0 || len(out.Rejected) != 1 {
			t.Fatalf("Ingest() = %+v, want the event refused", out)
		}
		assertRejected(t, out.Rejected[0], 0, "bh-1-resize",
			"size_schema: no schema registered for (openstack, black_hole)")
		assertDeadLettered(t, db, out.Rejected[0].Reason, refused)
	})

	t.Run("takes an event without a size in either mode", func(t *testing.T) {
		res := resource{cloud: "os-ingest-sizeless", resourceType: "black_hole", id: "bh-sizeless"}
		// The resource type is registered nowhere, so an event carrying a size
		// would be refused in strict mode. These carry none.
		modes := []struct {
			name     string
			pipeline *ingest.Pipeline
		}{{"lax", lax}, {"strict", strict}}
		for i, mode := range modes {
			t.Run(mode.name, func(t *testing.T) {
				update := item(t, res.event("bh-sizeless-"+mode.name, "black_hole.update",
					updateTime.Add(time.Duration(i)*time.Hour), payloadOf("paused", nil)))

				assertOutcome(t, batch(t, db, mode.pipeline, []json.RawMessage{update},
					event.SourceCollector, nil), ingest.Outcome{Accepted: 1})
			})
		}
	})

	t.Run("ingests reconciliation events without a scope", func(t *testing.T) {
		res := resource{cloud: "os-ingest-reconciliation", resourceType: "volume", id: "vol-sync"}
		create := res.event("vol-sync-create", "sync.create", createTime,
			payloadOf("available", volumeSize(10)))
		// The event names a cloud no credential is issued for, which is what a
		// nil scope is about: reconciliation reports for every cloud it syncs.
		remove := res.event("vol-sync-delete", "sync.delete", deleteTime, event.PayloadEnvelope{})
		remove.ProjectID = projectB
		items := []json.RawMessage{item(t, create), item(t, remove)}

		// Both calls run in the caller's transaction, which is how the
		// reconciliation orchestrator will drive one sync run.
		var first, second ingest.Outcome
		if err := db.Store.WithTx(t.Context(), func(tx pgx.Tx) error {
			var err error
			if first, err = lax.Ingest(t.Context(), tx, items, event.SourceReconciliation, nil); err != nil {
				return err
			}
			second, err = lax.Ingest(t.Context(), tx, items, event.SourceReconciliation, nil)
			return err
		}); err != nil {
			t.Fatalf("Ingest() error = %v, want nil", err)
		}

		assertOutcome(t, first, ingest.Outcome{Accepted: 2})
		assertOutcome(t, second, ingest.Outcome{Duplicates: 2})
		if got := eventSource(t, db, create.EventID); got != string(event.SourceReconciliation) {
			t.Errorf("stored source = %q, want %q", got, event.SourceReconciliation)
		}
		state, ok := resourceState(t, db, res.key())
		if !ok {
			t.Fatal("no projection row for the reconciled resource, want one")
		}
		if state != "deleted" {
			t.Errorf("state = %q, want %q", state, "deleted")
		}
	})

	t.Run("touches nothing for an empty batch", func(t *testing.T) {
		var out ingest.Outcome
		if err := db.Store.WithTx(t.Context(), func(tx pgx.Tx) error {
			var err error
			out, err = lax.Ingest(t.Context(), tx, nil, event.SourceCollector, nil)
			return err
		}); err != nil {
			t.Fatalf("Ingest() error = %v, want nil", err)
		}
		assertOutcome(t, out, ingest.Outcome{})
	})
}

// resource is the fixture a subtest works on: one resource of one cloud, which
// every event it builds names.
type resource struct {
	cloud        string
	resourceType string
	id           string
}

// event builds one event of the resource, as a collector would send it.
func (r resource) event(eventID, eventType string, ts time.Time, payload event.PayloadEnvelope) event.Event {
	return event.Event{
		EventID:      eventID,
		Timestamp:    ts,
		EventType:    eventType,
		Platform:     platform,
		Cloud:        r.cloud,
		ResourceType: r.resourceType,
		ResourceID:   r.id,
		ProjectID:    projectA,
		Source:       event.SourceCollector,
		Payload:      payload,
	}
}

// key is the projection key of the resource.
func (r resource) key() projection.Key {
	return projection.Key{Cloud: r.cloud, ResourceType: r.resourceType, ResourceID: r.id}
}

// item renders an event as the batch item a collector submits.
func item(t *testing.T, e event.Event) json.RawMessage {
	t.Helper()

	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshaling event %s: %v", e.EventID, err)
	}
	return raw
}

// payloadOf is a payload reporting a state and, when size is not nil, a new
// size.
func payloadOf(state string, size map[string]any) event.PayloadEnvelope {
	return event.PayloadEnvelope{State: &state, Size: size}
}

// volumeSize is a size the seeded (openstack, volume) schema accepts.
func volumeSize(gb float64) map[string]any {
	return map[string]any{"size_gb": gb, "type": "ssd"}
}

// batch runs one Ingest call in a transaction of its own, which is how the
// handler calls it.
func batch(t *testing.T, db storetest.DB, p *ingest.Pipeline, items []json.RawMessage,
	source event.Source, scope *ingest.Scope,
) ingest.Outcome {
	t.Helper()

	var out ingest.Outcome
	if err := db.Store.WithTx(t.Context(), func(tx pgx.Tx) error {
		var err error
		out, err = p.Ingest(t.Context(), tx, items, source, scope)
		return err
	}); err != nil {
		t.Fatalf("Ingest() error = %v, want nil", err)
	}
	return out
}

// assertOutcome fails the test unless the batch did exactly what want describes.
func assertOutcome(t *testing.T, got, want ingest.Outcome) {
	t.Helper()

	if !reflect.DeepEqual(got, want) {
		t.Errorf("Ingest() = %+v, want %+v", got, want)
	}
}

// assertRejected fails the test unless got names the item at index and refuses
// it for a reason starting with reasonPrefix. The reasons this package writes
// itself are given in full; the ones carrying a validator's own message are not
// this package's to spell out.
func assertRejected(t *testing.T, got ingest.Rejected, index int, eventID, reasonPrefix string) {
	t.Helper()

	if got.Index != index || got.EventID != eventID {
		t.Errorf("rejected item = %+v, want index %d and event id %q", got, index, eventID)
	}
	if !strings.HasPrefix(got.Reason, reasonPrefix) {
		t.Errorf("rejected reason = %q, want it prefixed %q", got.Reason, reasonPrefix)
	}
}

// assertDeadLettered fails the test unless one rejected_events row holds raw
// under reason. The raw column is JSONB, which stores the item as the document
// it is rather than as the bytes it was written in, so the comparison is the
// equality of that column.
func assertDeadLettered(t *testing.T, db storetest.DB, reason string, raw json.RawMessage) {
	t.Helper()

	var found int
	if err := db.Store.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM rejected_events WHERE reason = $1 AND raw = $2::jsonb`,
		reason, string(raw),
	).Scan(&found); err != nil {
		t.Fatalf("counting the dead-letter rows: %v", err)
	}
	if found != 1 {
		t.Errorf("dead-letter rows holding %s under %q = %d, want 1", raw, reason, found)
	}
}

// deadLetterRaws is the raw column of every dead-letter row written under
// reason, as the document Postgres renders it as.
func deadLetterRaws(t *testing.T, db storetest.DB, reason string) []string {
	t.Helper()

	rows, err := db.Store.Pool().Query(t.Context(),
		`SELECT raw::text FROM rejected_events WHERE reason = $1`, reason)
	if err != nil {
		t.Fatalf("reading the dead-letter rows of %q: %v", reason, err)
	}
	defer rows.Close()

	var raws []string
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scanning a dead-letter row: %v", err)
		}
		raws = append(raws, raw)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the dead-letter rows of %q: %v", reason, err)
	}
	return raws
}

// deadLetters is how many items of cloud were dead-lettered. A refused item
// leaves no events row, so the raw document is the only place its cloud
// survives.
func deadLetters(t *testing.T, db storetest.DB, cloud string) int {
	t.Helper()

	var found int
	if err := db.Store.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM rejected_events WHERE raw->>'cloud' = $1`, cloud,
	).Scan(&found); err != nil {
		t.Fatalf("counting the dead-letter rows of %s: %v", cloud, err)
	}
	return found
}

// auditEntry is an audit_log row as the assertions read it back.
type auditEntry struct {
	actor      string
	action     string
	objectType string
	details    map[string]any
}

// auditRows returns every audit_log row written about objectID.
func auditRows(t *testing.T, db storetest.DB, objectID string) []auditEntry {
	t.Helper()

	rows, err := db.Store.Pool().Query(t.Context(),
		`SELECT actor, action, object_type, details FROM audit_log WHERE object_id = $1 ORDER BY id`,
		objectID)
	if err != nil {
		t.Fatalf("reading the audit rows of %s: %v", objectID, err)
	}
	defer rows.Close()

	var found []auditEntry
	for rows.Next() {
		var entry auditEntry
		if err := rows.Scan(&entry.actor, &entry.action, &entry.objectType, &entry.details); err != nil {
			t.Fatalf("scanning an audit row: %v", err)
		}
		found = append(found, entry)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the audit rows of %s: %v", objectID, err)
	}
	return found
}

// eventSource is the source column of the stored event.
func eventSource(t *testing.T, db storetest.DB, eventID string) string {
	t.Helper()

	var source string
	if err := db.Store.Pool().QueryRow(t.Context(),
		`SELECT source FROM events WHERE event_id = $1`, eventID).Scan(&source); err != nil {
		t.Fatalf("reading the stored event %s: %v", eventID, err)
	}
	return source
}

// storedEvents is how many events the cloud holds.
func storedEvents(t *testing.T, db storetest.DB, cloud string) int {
	t.Helper()

	var found int
	if err := db.Store.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM events WHERE cloud = $1`, cloud).Scan(&found); err != nil {
		t.Fatalf("counting the events of %s: %v", cloud, err)
	}
	return found
}

// resourceRows reads every projection row of cloud as the JSON document
// Postgres renders it as, which compares two rows over all their columns at
// once.
func resourceRows(t *testing.T, db storetest.DB, cloud string) map[string]string {
	t.Helper()

	rows, err := db.Store.Pool().Query(t.Context(),
		`SELECT resource_id, to_jsonb(c)::text FROM current_resources c WHERE cloud = $1`, cloud)
	if err != nil {
		t.Fatalf("reading the projection rows of %s: %v", cloud, err)
	}
	defer rows.Close()

	found := map[string]string{}
	for rows.Next() {
		var id, row string
		if err := rows.Scan(&id, &row); err != nil {
			t.Fatalf("scanning a projection row: %v", err)
		}
		found[id] = row
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the projection rows of %s: %v", cloud, err)
	}
	return found
}

// resourceState reads the state of a projection row and whether there is one.
func resourceState(t *testing.T, db storetest.DB, key projection.Key) (string, bool) {
	t.Helper()

	var state string
	err := db.Store.Pool().QueryRow(t.Context(),
		`SELECT state FROM current_resources
		 WHERE cloud = $1 AND resource_type = $2 AND resource_id = $3`,
		key.Cloud, key.ResourceType, key.ResourceID).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false
	}
	if err != nil {
		t.Fatalf("reading the projection row of %s: %v", key.ResourceID, err)
	}
	return state, true
}
