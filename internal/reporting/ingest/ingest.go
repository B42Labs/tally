// Package ingest is the batch pipeline every event reaches the reporting
// database through. It decodes an item, validates it, checks it against the
// scope its credential may report for, checks its size against the registered
// schema, stores it, and folds what it stored into the projection. All of that
// runs in the caller's transaction, so a batch lands whole or not at all.
//
// A refused item does not fail the batch. Schema and size violations are
// dead-lettered into rejected_events and reported per item, which is what lets
// a collector drop the batch from its buffer once the call returns. A scope
// violation is a security event rather than schema drift, so it leaves an audit
// row and no dead-letter row (decision D5).
//
// The source a stored event reports is the one the caller passes, never the one
// the item claims: a collector cannot mark its events as reconciliation output.
// That is also what lets reconciliation reuse this pipeline, by calling Ingest
// with event.SourceReconciliation and no scope.
//
// The events of a batch are inserted in sorted order, and the resource keys
// they belong to are folded in sorted order too, so two batches that overlap
// take the row locks of the events they share and the advisory locks of the
// resources they share in the same order. Without that, two concurrent batches
// naming the same two events or the same two resources in opposite orders
// deadlock (SQLSTATE 40P01) and Postgres kills one of them.
//
// The normative specification is roadmap/01-phase-1-core-platform-openstack.md,
// WP 1.6.
package ingest

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/b42labs/tally/internal/core/event"
	"github.com/b42labs/tally/internal/reporting/audit"
	"github.com/b42labs/tally/internal/reporting/auth"
	"github.com/b42labs/tally/internal/reporting/projection"
	"github.com/b42labs/tally/internal/reporting/registry"
	"github.com/b42labs/tally/internal/reporting/store/sqlcgen"
)

// reasonScope is what a scope violation is reported as. It carries no detail:
// which platform and cloud the credential may report for is not something the
// answer to a rejected batch has to spell out.
const reasonScope = "scope"

// The audit row a scope violation leaves. The action names the events table and
// the verb, which is the form every action in the log takes.
const (
	auditObjectType      = "events"
	actionScopeViolation = auditObjectType + ".scope_violation"
)

// Scope is the platform and cloud an ingest credential may report for. A nil
// *Scope skips the check, which is how reconciliation ingests events for every
// cloud it syncs.
type Scope struct {
	Platform string
	Cloud    string
}

// Rejected names one refused item and why. Index is the item's position in the
// submitted batch, so a collector can map the reason back to what it sent even
// when the item carried no readable event id.
type Rejected struct {
	Index   int
	EventID string
	Reason  string
}

// Outcome is what one batch did: how many events it stored, how many the
// database already held, and which items it refused.
type Outcome struct {
	Accepted   int
	Duplicates int
	Rejected   []Rejected
}

// Hook runs once per newly stored event, after the batch was folded into the
// projection and inside the ingest transaction. An error it returns fails the
// whole batch, so what a hook writes lands together with the events it saw or
// not at all.
//
// A hook sees reconciliation-sourced events as well as collected ones, so a
// hook that is only about what a collector reported filters on e.Source.
type Hook interface {
	OnEvent(ctx context.Context, e event.Stored) error
}

// Pipeline ingests batches. Its zero value is not usable; New builds one. It is
// safe for concurrent use.
type Pipeline struct {
	registry          *registry.Registry
	requireSizeSchema bool
	hook              Hook
}

// New returns a Pipeline that validates sizes through reg. requireSizeSchema is
// what an unregistered (platform, resource_type) means: false accepts the event
// unvalidated, so onboarding a resource type is not blocked on registering its
// schema first, and true refuses it, which is what a production deployment runs
// (decision D9).
//
// hook is called over the events a batch stores. A nil hook calls nothing,
// which is what Phase 1 ingests with.
func New(reg *registry.Registry, requireSizeSchema bool, hook Hook) *Pipeline {
	return &Pipeline{registry: reg, requireSizeSchema: requireSizeSchema, hook: hook}
}

// Ingest runs the per-item pipeline over items, stores what survives it, and
// folds the stored events into the projection, all through tx. source is what
// the stored rows report regardless of what the items claim, and scope is the
// pair the items are checked against, nil for a caller that reports for every
// cloud.
//
// An error means the batch did not happen and the caller rolls tx back. What a
// single item does wrong is in the Outcome instead: a batch of nothing but
// refused items still commits, carrying the dead-letter and audit rows they
// left behind. An empty batch touches the database not at all.
func (p *Pipeline) Ingest(ctx context.Context, tx pgx.Tx, items []json.RawMessage, source event.Source, scope *Scope) (Outcome, error) {
	var out Outcome
	if len(items) == 0 {
		return out, nil
	}

	q := sqlcgen.New(tx)
	screened := make([]event.Event, 0, len(items))
	for i, raw := range items {
		e, reason, err := p.screen(ctx, q, raw, scope)
		if err != nil {
			return Outcome{}, err
		}
		if reason != "" {
			out.Rejected = append(out.Rejected, Rejected{Index: i, EventID: e.EventID, Reason: reason})
			continue
		}

		// Where an event came from is the caller's to state: what the body claims
		// never decides what the row says.
		e.Source = source
		screened = append(screened, e)
	}
	// Two batches that overlap must not insert the events they share in opposite
	// orders: the second inserter waits on the first transaction's row, so the
	// two would wait on each other.
	slices.SortFunc(screened, compareEvent)

	grouped := map[projection.Key][]event.Stored{}
	// What the hook is called over: the events this batch stored, in the order
	// they were inserted in. A duplicate is not one of them, and neither is a
	// refused item.
	var newlyStored []event.Stored
	for _, e := range screened {
		stored, inserted, err := insert(ctx, q, e)
		if err != nil {
			return Outcome{}, err
		}
		if !inserted {
			out.Duplicates++
			continue
		}
		out.Accepted++
		newlyStored = append(newlyStored, stored)

		key := projection.Key{Cloud: e.Cloud, ResourceType: e.ResourceType, ResourceID: e.ResourceID}
		grouped[key] = append(grouped[key], stored)
	}

	for _, key := range slices.SortedFunc(maps.Keys(grouped), projection.CompareKey) {
		if err := projection.Apply(ctx, tx, key, grouped[key]); err != nil {
			return Outcome{}, fmt.Errorf("folding the batch into the projection: %w", err)
		}
	}

	if p.hook != nil {
		for _, e := range newlyStored {
			if err := p.hook.OnEvent(ctx, e); err != nil {
				return Outcome{}, fmt.Errorf("running the post-ingest hook for event %s: %w", e.EventID, err)
			}
		}
	}
	return out, nil
}

// screen runs the checks one item passes before it is stored: decode and
// validate, scope, size. It returns the event the item carries and the reason
// it was refused, which is empty for an item that survives.
//
// Refusing an item is not an error. What the refusal leaves behind, a
// dead-letter row or an audit row, is written here, and the error return is for
// what takes the whole batch down.
func (p *Pipeline) screen(ctx context.Context, q *sqlcgen.Queries, raw json.RawMessage, scope *Scope) (event.Event, string, error) {
	var e event.Event
	if carriesNUL(raw) {
		// The item never reaches the decoder: a NUL is refused here rather than
		// at the database, where it would take the whole batch down instead of
		// the one item carrying it.
		e.EventID = probeEventID(raw)
		return e, reasonNUL, deadLetter(ctx, q, reasonNUL, raw)
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		// Nothing decoded, so the only event id there is to report is whatever the
		// item spells at the top level.
		e.EventID = probeEventID(raw)
		reason := fmt.Sprintf("schema: %s", err)
		return e, reason, deadLetter(ctx, q, reason, raw)
	}
	if err := e.Validate(); err != nil {
		reason := fmt.Sprintf("schema: %s", err)
		return e, reason, deadLetter(ctx, q, reason, raw)
	}

	if scope != nil && (e.Platform != scope.Platform || e.Cloud != scope.Cloud) {
		return e, reasonScope, auditScopeViolation(ctx, q, *scope, e)
	}

	// An event without a size reports that the size did not change, so there is
	// nothing to validate.
	if e.Payload.Size == nil {
		return e, "", nil
	}
	reason, err := p.checkSize(ctx, q, e)
	if err != nil {
		return e, "", err
	}
	if reason == "" {
		return e, "", nil
	}
	return e, reason, deadLetter(ctx, q, reason, raw)
}

// checkSize validates the event's size object against the schema registered for
// its (platform, resource_type) and returns the reason to refuse the event,
// empty when there is none. An unregistered pair is refused in strict mode only.
func (p *Pipeline) checkSize(ctx context.Context, q *sqlcgen.Queries, e event.Event) (string, error) {
	schema, registered, err := p.registry.Validator(ctx, q, e.Platform, e.ResourceType)
	if err != nil {
		return "", fmt.Errorf("validating the size of event %s: %w", e.EventID, err)
	}
	if !registered {
		if !p.requireSizeSchema {
			return "", nil
		}
		return fmt.Sprintf("size_schema: no schema registered for (%s, %s)", e.Platform, e.ResourceType), nil
	}

	size, err := instance(e.Payload.Size)
	if err != nil {
		return "", fmt.Errorf("reading the size of event %s: %w", e.EventID, err)
	}
	if err := schema.Validate(size); err != nil {
		return fmt.Sprintf("size_schema: %s", err), nil
	}
	return "", nil
}

// insert stores e and pairs it with the received_at the database assigned. The
// second result reports whether the row is new: the insert does nothing when
// (event_id, timestamp) is already stored, which is what makes a replayed batch
// a batch of duplicates rather than a failure.
func insert(ctx context.Context, q *sqlcgen.Queries, e event.Event) (event.Stored, bool, error) {
	payload, err := json.Marshal(e.Payload)
	if err != nil {
		return event.Stored{}, false, fmt.Errorf("marshaling the payload of event %s: %w", e.EventID, err)
	}

	receivedAt, err := q.InsertEvent(ctx, sqlcgen.InsertEventParams{
		EventID:      e.EventID,
		Timestamp:    pgtype.Timestamptz{Time: e.Timestamp, Valid: true},
		EventType:    e.EventType,
		Platform:     e.Platform,
		Cloud:        e.Cloud,
		ResourceType: e.ResourceType,
		ResourceID:   e.ResourceID,
		ProjectID:    e.ProjectID,
		Source:       string(e.Source),
		Payload:      payload,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return event.Stored{}, false, nil
		}
		return event.Stored{}, false, fmt.Errorf("inserting event %s: %w", e.EventID, err)
	}
	return event.Stored{Event: e, ReceivedAt: receivedAt.Time}, true, nil
}

// deadLetter keeps the refused item together with the reason it was refused, so
// an operator can see what was dropped after the collector stopped holding on
// to it. The item is stored as it arrived, as far as the column can hold it.
func deadLetter(ctx context.Context, q *sqlcgen.Queries, reason string, raw json.RawMessage) error {
	if err := q.InsertRejectedEvent(ctx, sqlcgen.InsertRejectedEventParams{
		Reason: reason,
		Raw:    storable(raw),
	}); err != nil {
		return fmt.Errorf("dead-lettering a refused item: %w", err)
	}
	return nil
}

// maxDeadLetterBytes is how much of a refused item is kept. Past it the reason
// is what an operator works from: nothing rate-limits how much a credential may
// have refused, and rejected_events is a plain table on the volume the events
// themselves live on.
const maxDeadLetterBytes = 64 << 10

// storable is what the jsonb column can hold of a refused item. An item that is
// too large, that carries a NUL, or that is not a JSON document at all is
// replaced by a note naming its size, because a dead-letter row the database
// refuses fails the batch that the dead letter exists to keep alive.
func storable(raw json.RawMessage) json.RawMessage {
	if len(raw) <= maxDeadLetterBytes && !carriesNUL(raw) && json.Valid(raw) {
		return raw
	}
	return json.RawMessage(fmt.Sprintf(`{"not_stored":{"bytes":%d}}`, len(raw)))
}

// reasonNUL is what an item carrying a NUL character is refused with.
const reasonNUL = "schema: the item carries a NUL character, which cannot be stored"

// nulEscape is how a NUL reaches this pipeline: JSON has to escape control
// characters, and encoding/json decodes the escape into a real NUL without
// complaint. PostgreSQL holds one in neither a text column nor a jsonb
// document.
const nulEscape = `u0000`

// carriesNUL reports whether the item spells a NUL character anywhere. An odd
// number of backslashes in front of the escape is what makes it one rather than
// the literal six characters, and a raw NUL byte is one as well: it is not
// valid JSON, but it still reaches the dead-letter insert.
func carriesNUL(raw json.RawMessage) bool {
	if bytes.IndexByte(raw, 0) >= 0 {
		return true
	}
	for i := 0; ; {
		found := bytes.Index(raw[i:], []byte(nulEscape))
		if found < 0 {
			return false
		}
		at := i + found
		backslashes := 0
		for b := at - 1; b >= 0 && raw[b] == '\\'; b-- {
			backslashes++
		}
		if backslashes%2 == 1 {
			return true
		}
		i = at + len(nulEscape)
	}
}

// auditScopeViolation records that a credential reported an event for a
// platform or cloud it may not report for. The row names both sides, because
// the item itself is not kept anywhere (decision D5).
func auditScopeViolation(ctx context.Context, q *sqlcgen.Queries, scope Scope, e event.Event) error {
	actor := audit.ActorInternal
	if principal, ok := auth.IngestFromContext(ctx); ok {
		actor = principal.CredentialID.String()
	}

	if err := audit.Log(ctx, q, audit.Entry{
		Actor:      actor,
		Action:     actionScopeViolation,
		ObjectType: auditObjectType,
		ObjectID:   e.EventID,
		Details: map[string]any{
			"credential_platform": scope.Platform,
			"credential_cloud":    scope.Cloud,
			"event_platform":      e.Platform,
			"event_cloud":         e.Cloud,
		},
	}); err != nil {
		return fmt.Errorf("auditing the scope violation of event %s: %w", e.EventID, err)
	}
	return nil
}

// instance renders a size object the way the validator takes it. jsonschema
// works on documents its own UnmarshalJSON decoded, which reads numbers as
// json.Number, so a size reporting "vcpus": 4 meets an integer keyword as the
// integer it was written as rather than as a float64.
func instance(size map[string]any) (any, error) {
	raw, err := json.Marshal(size)
	if err != nil {
		return nil, fmt.Errorf("marshaling the size: %w", err)
	}
	return jsonschema.UnmarshalJSON(bytes.NewReader(raw))
}

// probeEventID reads the event id out of an item that did not decode, so the
// per-item outcome can still name what the collector sent. An item that spells
// none is reported without one.
func probeEventID(raw json.RawMessage) string {
	var probe struct {
		EventID string `json:"event_id"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return ""
	}
	return probe.EventID
}

// compareEvent orders two events by the key the events table is unique over,
// which is the order the rows of one batch are inserted in.
func compareEvent(a, b event.Event) int {
	return cmp.Or(
		strings.Compare(a.EventID, b.EventID),
		a.Timestamp.Compare(b.Timestamp),
	)
}
