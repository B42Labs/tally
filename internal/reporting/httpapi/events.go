package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/b42labs/tally/internal/core/event"
	"github.com/b42labs/tally/internal/reporting/audit"
	"github.com/b42labs/tally/internal/reporting/auth"
	"github.com/b42labs/tally/internal/reporting/httpapi/problem"
	"github.com/b42labs/tally/internal/reporting/ingest"
	"github.com/b42labs/tally/internal/reporting/store/sqlcgen"
)

// maxBatchItems is how many events one call may submit. The cap is checked here
// rather than declared in the contract, because the contract's answer to a
// violation is a 400 and a batch that is merely too large is a 413.
const maxBatchItems = 1000

// The audit trail one ingest call leaves.
const (
	auditObjectEvents = "events"
	actionIngest      = auditObjectEvents + ".ingest"
)

// IngestEvents stores a submitted batch of events.
//
// It answers 200 whenever the batch was readable and stored, whatever the
// individual items did: an item the pipeline refuses is kept server-side and
// named in the answer, so a collector may drop the whole batch from its buffer
// once the call returns. What is not a 200 is what a collector has to retry.
func (s *server) IngestEvents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	items, err := readBatch(r.Body)
	if err != nil {
		// The validator read this body and checked it against the contract
		// before the request got here, so a failure now means the two decoders
		// disagree about it rather than that the caller sent something wrong.
		writeInternal(ctx, w, "reading a batch the contract accepted", err,
			"the batch could not be read")
		return
	}
	if len(items) > maxBatchItems {
		problem.Write(w, http.StatusRequestEntityTooLarge, problem.TypePayloadTooLarge,
			"Payload too large", fmt.Sprintf("a batch carries at most %d events", maxBatchItems))
		return
	}
	// An empty batch has nothing to store and nothing to record, so it never
	// opens a transaction.
	if len(items) == 0 {
		writeJSON(w, ingestResult(ingest.Outcome{}))
		return
	}

	scope, actor := ingestCredential(ctx)

	// The batch and the audit row naming it share one transaction, so a batch
	// the log does not account for never reaches the database.
	var outcome ingest.Outcome
	if err := s.store.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		if outcome, err = s.pipeline.Ingest(ctx, tx, items, event.SourceCollector, scope); err != nil {
			return err
		}
		return audit.Log(ctx, sqlcgen.New(tx), audit.Entry{
			Actor:      actor,
			Action:     actionIngest,
			ObjectType: auditObjectEvents,
			Details: map[string]any{
				"accepted":   outcome.Accepted,
				"duplicates": outcome.Duplicates,
				"rejected":   len(outcome.Rejected),
			},
		})
	}); err != nil {
		writeInternal(ctx, w, "ingesting a batch of events", err,
			"the batch could not be stored")
		return
	}

	writeJSON(w, ingestResult(outcome))
}

// readBatch splits a request body into the items of one call. A body whose
// first non-space byte opens an array is the batch form; anything else is the
// single event a collector sends when it has one.
//
// The items stay raw: which of them is well-formed is the pipeline's to decide,
// per item, and an item this function rejected would take its siblings with it.
func readBatch(body io.Reader) ([]json.RawMessage, error) {
	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("reading the request body: %w", err)
	}

	trimmed := bytes.TrimLeft(raw, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return []json.RawMessage{trimmed}, nil
	}

	var items []json.RawMessage
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return nil, fmt.Errorf("decoding the batch: %w", err)
	}
	return items, nil
}

// ingestCredential reads what the request authenticated with: the pair its
// items are checked against, and who the audit row names. A request served
// while authentication is disabled carries none, which reports for every cloud
// on behalf of the service itself.
func ingestCredential(ctx context.Context) (*ingest.Scope, string) {
	principal, ok := auth.IngestFromContext(ctx)
	if !ok {
		return nil, audit.ActorInternal
	}
	return &ingest.Scope{Platform: principal.Platform, Cloud: principal.Cloud},
		principal.CredentialID.String()
}

// ingestResult renders what the pipeline did as the answer the contract
// promises. The rejected member is an array even when nothing was rejected, so
// a client iterates it without checking for null first.
func ingestResult(outcome ingest.Outcome) IngestResult {
	rejected := make([]RejectedEvent, len(outcome.Rejected))
	for i, item := range outcome.Rejected {
		rejected[i] = RejectedEvent{Index: item.Index, EventId: item.EventID, Reason: item.Reason}
	}
	return IngestResult{
		Accepted:   outcome.Accepted,
		Duplicates: outcome.Duplicates,
		Rejected:   rejected,
	}
}
