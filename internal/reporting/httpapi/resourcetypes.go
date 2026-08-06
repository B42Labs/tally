package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/b42labs/tally/internal/reporting/audit"
	"github.com/b42labs/tally/internal/reporting/auth"
	"github.com/b42labs/tally/internal/reporting/httpapi/problem"
	"github.com/b42labs/tally/internal/reporting/registry"
	"github.com/b42labs/tally/internal/reporting/store/sqlcgen"
)

// The audit trail a registration leaves.
const (
	auditObjectResourceTypes = "resource_types"
	actionRegisterType       = auditObjectResourceTypes + ".register"
)

// ListResourceTypes answers with every registered resource type.
func (s *server) ListResourceTypes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	rows, err := s.queries.ListResourceTypes(ctx)
	if err != nil {
		writeInternal(ctx, w, "listing the resource types", err,
			"the resource types could not be read")
		return
	}

	items := make([]ResourceType, len(rows))
	for i, row := range rows {
		if items[i], err = renderResourceType(row); err != nil {
			writeInternal(ctx, w, "rendering a resource type", err,
				"the resource types could not be read")
			return
		}
	}

	// The cursor stays null: the registry holds one row per resource type, so
	// the answer is never long enough to page.
	writeJSON(w, ResourceTypeList{Items: items})
}

// GetResourceType answers with one registered resource type.
func (s *server) GetResourceType(w http.ResponseWriter, r *http.Request, platform, resourceType string) {
	ctx := r.Context()

	row, err := s.queries.GetResourceType(ctx, sqlcgen.GetResourceTypeParams{
		Platform:     platform,
		ResourceType: resourceType,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			problem.Write(w, http.StatusNotFound, problem.TypeNotFound,
				"Not found", "this resource type is not registered")
			return
		}
		writeInternal(ctx, w, "reading a resource type", err,
			"the resource type could not be read")
		return
	}

	item, err := renderResourceType(row)
	if err != nil {
		writeInternal(ctx, w, "rendering a resource type", err,
			"the resource type could not be read")
		return
	}
	writeJSON(w, item)
}

// PutResourceType registers the size schema of one resource type, replacing
// whatever was registered for it before.
//
// The document is compiled before it is stored and nothing is written when it
// does not compile, so what this endpoint accepts is exactly what the ingest
// pipeline can apply later. The compiler's message goes back to the caller: the
// schema is the operator's own document, and naming the keyword at fault is
// what makes the refusal actionable.
func (s *server) PutResourceType(w http.ResponseWriter, r *http.Request, platform, resourceType string) {
	ctx := r.Context()

	// The schema is kept as it arrived rather than re-encoded from the generated
	// model, so an operator reads back the document they sent, member order and
	// all.
	var body struct {
		SizeSchema json.RawMessage `json:"size_schema"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		// The validator checked this body against the contract before the
		// request got here, so a failure now is this service disagreeing with
		// itself rather than a caller error.
		writeInternal(ctx, w, "decoding a registration the contract accepted", err,
			"the registration could not be read")
		return
	}
	if _, err := registry.Compile(body.SizeSchema); err != nil {
		problem.Write(w, http.StatusBadRequest, problem.TypeValidation,
			"Validation failed", err.Error())
		return
	}

	// The row and the audit row naming it share one transaction, so a
	// registration the log does not account for never reaches the database.
	var stored sqlcgen.ResourceType
	if err := s.store.WithTx(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		var err error
		if stored, err = q.UpsertResourceType(ctx, sqlcgen.UpsertResourceTypeParams{
			Platform:     platform,
			ResourceType: resourceType,
			SizeSchema:   body.SizeSchema,
		}); err != nil {
			return fmt.Errorf("registering (%s, %s): %w", platform, resourceType, err)
		}
		return audit.Log(ctx, q, audit.Entry{
			Actor:      queryActor(ctx),
			Action:     actionRegisterType,
			ObjectType: auditObjectResourceTypes,
			ObjectID:   platform + ":" + resourceType,
		})
	}); err != nil {
		writeInternal(ctx, w, "registering a resource type", err,
			"the resource type could not be registered")
		return
	}

	item, err := renderResourceType(stored)
	if err != nil {
		writeInternal(ctx, w, "rendering a resource type", err,
			"the resource type could not be read back")
		return
	}
	writeJSON(w, item)
}

// renderResourceType turns one stored row into the answer the contract
// promises. The stored document is decoded rather than passed through, because
// the contract types size_schema as an object and a row written past the API
// could hold anything.
func renderResourceType(row sqlcgen.ResourceType) (ResourceType, error) {
	var schema map[string]any
	if err := json.Unmarshal(row.SizeSchema, &schema); err != nil {
		return ResourceType{}, fmt.Errorf("decoding the size schema of (%s, %s): %w",
			row.Platform, row.ResourceType, err)
	}
	return ResourceType{
		Platform:     row.Platform,
		ResourceType: row.ResourceType,
		SizeSchema:   schema,
		UpdatedAt:    row.UpdatedAt.Time,
	}, nil
}

// queryActor is who the audit row of a write names: the api_tokens row the
// request authenticated with. While authentication is disabled the request
// carries the synthetic admin principal instead, whose token id is the zero
// UUID because no row backs it, and the change is recorded as the service's own.
func queryActor(ctx context.Context) string {
	principal, ok := auth.QueryFromContext(ctx)
	if !ok || principal.TokenID == uuid.Nil {
		return audit.ActorInternal
	}
	return principal.TokenID.String()
}
