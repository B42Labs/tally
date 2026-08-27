package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/b42labs/tally/internal/core/adjustment"
	"github.com/b42labs/tally/internal/reporting/audit"
	"github.com/b42labs/tally/internal/reporting/httpapi/problem"
	"github.com/b42labs/tally/internal/reporting/projects"
	"github.com/b42labs/tally/internal/reporting/store/sqlcgen"
)

// The audit trail the three writes of the relation graph leave. A close is its
// own verb rather than an update: it is what ends a relation, and the row it
// leaves is what an operator reads that end off.
const (
	auditObjectRelations = "project_relations"
	actionCreateRelation = auditObjectRelations + ".create"
	actionUpdateRelation = auditObjectRelations + ".update"
	actionCloseRelation  = auditObjectRelations + ".close"
)

// unknownRelationDetail answers every request addressing a relation this API
// does not hold under the project of the path. A relation that leaves another
// project is answered with it too: a relation is addressed under the project it
// leaves, so one that leaves another project is absent here rather than
// someone else's row.
const unknownRelationDetail = "this relation is not stored for this project"

// duplicateRelationDetail answers a creation whose triple is already related.
// It names the triple rather than the row, because that is what the caller has
// to change to get through: the same triple is created again once the open
// relation over it is closed.
const duplicateRelationDetail = "a relation of this type between these projects is already active"

// The details that answer a relation a caller has to correct before it can be
// stored: one end that is the other, a target outside the registry, an end
// instant that is not after the start, a relation that would make attribution
// circular, and an adjustments array the schema refuses.
const (
	selfRelationDetail       = "a relation cannot leave and reach the same project"
	closeBeforeStart         = "valid_to has to be after valid_from"
	cycleDetail              = "this relation would close a cycle over the relation types that attribute cost"
	invalidAdjustmentsDetail = "the pricing adjustments of this relation do not match the adjustments schema"
)

// The unique index that holds one open relation per triple, as Postgres reports
// it when a creation collides with it. Matching the name and not the code alone
// is what keeps another unique violation from being answered as a duplicate
// relation.
const activeRelationConstraint = "uq_relations_active"

// The details that answer a failure of one of these routes a caller can do
// nothing about. Which operation failed is what they tell apart, so each is
// spelled once rather than at every writeInternal of that operation.
const (
	relationsDetail       = "the relations could not be read"
	relatedProjectsDetail = "the related projects could not be read"
	createRelationDetail  = "the relation could not be created"
	updateRelationDetail  = "the relation could not be updated"
	closeRelationDetail   = "the relation could not be closed"
)

// What a write of the relation graph refuses its request for. The checks run
// inside the transaction and the answer is written after it, so each of them
// leaves the transaction as one of these and is routed by errors.Is once the
// rollback has happened.
var (
	errUnknownSource    = errors.New("the project the relation leaves is not registered")
	errSelfRelation     = errors.New("the relation leaves and reaches the same project")
	errUnknownTarget    = errors.New("the project the relation reaches is not registered")
	errClosedBeforeOpen = errors.New("the relation would end no later than it starts")
)

// CreateProjectRelation relates the project of the path to another one and
// answers the relation under the id the other operations address it by.
//
// Everything the creation decides on happens in one transaction: the two
// projects have to be registered, the walk that keeps attribution a forest runs
// against the graph the insert lands in, and the audit row naming the relation
// is written beside it. A creation the log does not account for never reaches
// the database.
//
// The triple being active already is decided by the database rather than by a
// read in front of the insert: two racing creations of one triple would both
// pass such a read, and one of them would still have to be refused here.
func (s *server) CreateProjectRelation(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	ctx := r.Context()

	// The metadata is kept as it arrived rather than decoded into a map and
	// marshalled again: the column takes the document itself.
	var body struct {
		TargetID     uuid.UUID       `json:"target_id"`
		RelationType string          `json:"relation_type"`
		Metadata     json.RawMessage `json:"metadata"`
		ValidFrom    *time.Time      `json:"valid_from"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		// The validator checked this body against the contract before the
		// request got here, so a failure now is this service disagreeing with
		// itself rather than a caller error.
		writeInternal(ctx, w, "decoding a relation the contract accepted", err,
			createRelationDetail)
		return
	}
	metadata := body.Metadata
	if len(metadata) == 0 {
		// The column is NOT NULL and the contract answers metadata as an object,
		// so a creation that carries none stores the empty one.
		metadata = []byte("{}")
	}

	// The adjustments are held to the schema before any transaction opens
	// (decision D2): a document the schema refuses stores no relation and
	// leaves no audit row, and it is refused at the operator's terminal rather
	// than in a billing run weeks later.
	if err := adjustment.ValidateMetadata(metadata); err != nil {
		refuseAdjustments(ctx, w, err, "reading the adjustments of a relation the contract accepted",
			createRelationDetail)
		return
	}

	// Only a type that attributes cost is walked, and only such a type is worth
	// serializing the creations of.
	attributing := slices.Contains(s.attributingTypes, body.RelationType)

	var stored sqlcgen.ProjectRelation
	if err := s.store.WithTx(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		if attributing {
			// Two racing creations each walk a graph the other has not written
			// to yet, so both could pass the walk and close a cycle together.
			// The lock is held until this transaction ends, which puts the
			// second walk behind the first insert.
			if err := q.LockAttributingRelations(ctx); err != nil {
				return fmt.Errorf("locking the attributing relations: %w", err)
			}
		}

		if _, err := q.GetProject(ctx, id); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errUnknownSource
			}
			return fmt.Errorf("reading the project %s a relation leaves: %w", id, err)
		}
		if body.TargetID == id {
			// The table's CHECK (source_id <> target_id) refuses this as well,
			// but as a failed write rather than as an answer naming the member
			// the caller has to change.
			return errSelfRelation
		}
		if _, err := q.GetProject(ctx, body.TargetID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errUnknownTarget
			}
			return fmt.Errorf("reading the project %s a relation reaches: %w", body.TargetID, err)
		}
		if attributing {
			if err := projects.GuardCycle(ctx, q, id, body.TargetID, s.attributingTypes); err != nil {
				return err
			}
		}

		var err error
		if stored, err = q.InsertProjectRelation(ctx, sqlcgen.InsertProjectRelationParams{
			SourceID:     id,
			TargetID:     body.TargetID,
			RelationType: body.RelationType,
			Metadata:     metadata,
			ValidFrom:    filterInstant(body.ValidFrom),
		}); err != nil {
			return fmt.Errorf("relating %s to %s: %w", id, body.TargetID, err)
		}
		return audit.Log(ctx, q, audit.Entry{
			Actor:      queryActor(ctx),
			Action:     actionCreateRelation,
			ObjectType: auditObjectRelations,
			ObjectID:   stored.ID.String(),
		})
	}); err != nil {
		switch {
		case errors.Is(err, errUnknownSource):
			problem.Write(w, http.StatusNotFound, problem.TypeNotFound,
				"Not found", unknownProjectDetail)
		case errors.Is(err, errSelfRelation):
			problem.Write(w, http.StatusUnprocessableEntity, problem.TypeValidation,
				"Validation failed", selfRelationDetail,
				problem.FieldError{Loc: "body.target_id", Msg: "this is the project the relation leaves"})
		case errors.Is(err, errUnknownTarget):
			problem.Write(w, http.StatusUnprocessableEntity, problem.TypeValidation,
				"Validation failed", unknownProjectDetail)
		case errors.Is(err, projects.ErrCycle):
			problem.Write(w, http.StatusUnprocessableEntity, problem.TypeRelationCycle,
				"Relation cycle", cycleDetail)
		case violatesUnique(err, activeRelationConstraint):
			problem.Write(w, http.StatusConflict, problem.TypeConflict,
				"Conflict", duplicateRelationDetail)
		default:
			writeInternal(ctx, w, "creating a relation", err, createRelationDetail)
		}
		return
	}

	item, err := relationOf(stored)
	if err != nil {
		writeInternal(ctx, w, "rendering a relation", err, createRelationDetail)
		return
	}
	// A 201 names where the created thing lives, which is the route that reads
	// it back.
	w.Header().Set("Location", "/api/v1/projects/"+id.String()+"/relations/"+item.Id.String())
	writeJSONStatus(w, http.StatusCreated, item)
}

// ListProjectRelations answers the relations of one project as they stood at
// one instant, which is now when the request names no instant.
//
// The project is read before the relations are: a project with no relation
// valid at that instant answers the empty list, and one the registry does not
// hold answers 404, so the two are told apart.
func (s *server) ListProjectRelations(w http.ResponseWriter, r *http.Request, id uuid.UUID,
	params ListProjectRelationsParams,
) {
	ctx := r.Context()

	if !s.holdsProject(w, r, id, relationsDetail) {
		return
	}

	// The contract bounds the direction to the three values below, so what
	// reaches here is one of them or nothing at all.
	outgoing, incoming := true, true
	if params.Direction != nil {
		outgoing = *params.Direction != Incoming
		incoming = *params.Direction != Outgoing
	}

	rows, err := s.queries.ListProjectRelations(ctx, sqlcgen.ListProjectRelationsParams{
		Outgoing:     outgoing,
		ProjectID:    id,
		Incoming:     incoming,
		At:           pgtype.Timestamptz{Time: instantOr(params.At), Valid: true},
		RelationType: filterText(params.RelationType),
	})
	if err != nil {
		writeInternal(ctx, w, "listing the relations of a project", err, relationsDetail)
		return
	}

	items := make([]Relation, len(rows))
	for i, row := range rows {
		if items[i], err = relationOf(row); err != nil {
			writeInternal(ctx, w, "decoding a stored relation", err, relationsDetail)
			return
		}
	}
	writeJSON(w, RelationList{Items: items})
}

// UpdateProjectRelation changes the metadata or the end of one relation and
// answers it as it now stands. The row and the audit row naming it share one
// transaction, so a change the log does not account for never reaches the
// database.
//
// A member the request leaves out arrives at the query as NULL, which its
// COALESCE reads as leaving the column alone. Setting valid_to on a relation
// that is already closed corrects the instant it was closed at; the contract
// types the member as not nullable, so reopening a closed relation is refused
// in front of this.
func (s *server) UpdateProjectRelation(w http.ResponseWriter, r *http.Request,
	id uuid.UUID, relationID uuid.UUID,
) {
	ctx := r.Context()

	var body struct {
		Metadata json.RawMessage `json:"metadata"`
		ValidTo  *time.Time      `json:"valid_to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeInternal(ctx, w, "decoding an update the contract accepted", err,
			updateRelationDetail)
		return
	}

	var stored sqlcgen.ProjectRelation
	if err := s.store.WithTx(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		current, err := q.GetProjectRelation(ctx, sqlcgen.GetProjectRelationParams{
			ID:       relationID,
			SourceID: id,
		})
		if err != nil {
			return fmt.Errorf("reading the relation %s of %s: %w", relationID, id, err)
		}
		// A relation valid from its start up to its own start was never valid,
		// and one ending before it starts is no span at all. The stored
		// valid_from is what decides, so the check reads the row rather than
		// trusting what the request thinks the relation starts at.
		if body.ValidTo != nil && !body.ValidTo.After(current.ValidFrom.Time) {
			return errClosedBeforeOpen
		}

		if stored, err = q.UpdateProjectRelation(ctx, sqlcgen.UpdateProjectRelationParams{
			ID:       relationID,
			SourceID: id,
			Metadata: body.Metadata,
			ValidTo:  filterInstant(body.ValidTo),
		}); err != nil {
			return fmt.Errorf("updating the relation %s of %s: %w", relationID, id, err)
		}
		return audit.Log(ctx, q, audit.Entry{
			Actor:      queryActor(ctx),
			Action:     actionUpdateRelation,
			ObjectType: auditObjectRelations,
			ObjectID:   stored.ID.String(),
		})
	}); err != nil {
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			problem.Write(w, http.StatusNotFound, problem.TypeNotFound,
				"Not found", unknownRelationDetail)
		case errors.Is(err, errClosedBeforeOpen):
			problem.Write(w, http.StatusUnprocessableEntity, problem.TypeValidation,
				"Validation failed", closeBeforeStart)
		default:
			writeInternal(ctx, w, "updating a relation", err, updateRelationDetail)
		}
		return
	}

	item, err := relationOf(stored)
	if err != nil {
		writeInternal(ctx, w, "rendering a relation", err, updateRelationDetail)
		return
	}
	writeJSON(w, item)
}

// DeleteProjectRelation ends one relation by closing it at this instant. The
// row stays, so a read at an earlier instant still finds the relation.
//
// A relation that is already closed is answered the same 204 and keeps the
// instant it was closed at: what the caller asked for is a relation that is no
// longer valid, and it is not. Nothing changed, so no audit row is written
// either, which is what makes the log count the closes rather than the calls.
func (s *server) DeleteProjectRelation(w http.ResponseWriter, r *http.Request,
	id uuid.UUID, relationID uuid.UUID,
) {
	ctx := r.Context()

	if err := s.store.WithTx(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		closed, err := q.CloseProjectRelation(ctx, sqlcgen.CloseProjectRelationParams{
			ID:       relationID,
			SourceID: id,
		})
		if err != nil {
			return fmt.Errorf("closing the relation %s of %s: %w", relationID, id, err)
		}
		if closed == 0 {
			// The close matches an open relation of this project alone, so a
			// row count of zero is either a relation that is closed already or
			// none this project has. The read is what tells the two apart.
			if _, err := q.GetProjectRelation(ctx, sqlcgen.GetProjectRelationParams{
				ID:       relationID,
				SourceID: id,
			}); err != nil {
				return fmt.Errorf("reading the relation %s of %s: %w", relationID, id, err)
			}
			return nil
		}
		return audit.Log(ctx, q, audit.Entry{
			Actor:      queryActor(ctx),
			Action:     actionCloseRelation,
			ObjectType: auditObjectRelations,
			ObjectID:   relationID.String(),
		})
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			problem.Write(w, http.StatusNotFound, problem.TypeNotFound,
				"Not found", unknownRelationDetail)
			return
		}
		writeInternal(ctx, w, "closing a relation", err, closeRelationDetail)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListRelatedProjects answers every project the walk out of one project reaches
// over the relations valid at one instant, which is now when the request names
// no instant.
//
// The walk yields ids, and the projects behind them are read in one batch
// rather than one query per hop: the contract answers whole projects, and a
// walk ten deep would otherwise cost a read per project it reached.
func (s *server) ListRelatedProjects(w http.ResponseWriter, r *http.Request, id uuid.UUID,
	params ListRelatedProjectsParams,
) {
	ctx := r.Context()

	if !s.holdsProject(w, r, id, relatedProjectsDetail) {
		return
	}

	// The contract bounds the depth between one and ten and defaults it to one,
	// so what reaches here is either inside those bounds or absent.
	depth := 1
	if params.Depth != nil {
		depth = *params.Depth
	}

	reached, err := projects.Traverse(ctx, s.queries, id, depth, params.RelationType,
		instantOr(params.At))
	if err != nil {
		writeInternal(ctx, w, "walking the relations of a project", err, relatedProjectsDetail)
		return
	}

	// The walk visits a project once, so the ids it yields are already distinct
	// and the batch read holds one row per reached project.
	ids := make([]uuid.UUID, len(reached))
	for i, related := range reached {
		ids[i] = related.ProjectID
	}
	rows, err := s.queries.GetProjectsByIDs(ctx, ids)
	if err != nil {
		writeInternal(ctx, w, "reading the projects a walk reached", err, relatedProjectsDetail)
		return
	}
	byID := make(map[uuid.UUID]Project, len(rows))
	for _, row := range rows {
		item, err := projectOf(row)
		if err != nil {
			writeInternal(ctx, w, "decoding a stored project", err, relatedProjectsDetail)
			return
		}
		byID[row.ID] = item
	}

	items := make([]RelatedProject, len(reached))
	for i, related := range reached {
		project, held := byID[related.ProjectID]
		if !held {
			// Both ends of a relation are foreign keys into the registry, so a
			// walk cannot reach a project the batch read misses.
			writeInternal(ctx, w, "walking the relations of a project",
				fmt.Errorf("the walk reached the unregistered project %s", related.ProjectID),
				relatedProjectsDetail)
			return
		}
		items[i] = RelatedProject{
			Project:      project,
			RelationType: related.RelationType,
			Depth:        related.Depth,
			Path:         related.Path,
		}
	}
	writeJSON(w, RelatedProjectList{Items: items})
}

// holdsProject reports whether the registry holds the project a relation read
// is about, and answers the request itself when it does not. Both reads ask
// before they walk anything, so that a project with nothing to answer is told
// apart from one this API does not know.
func (s *server) holdsProject(w http.ResponseWriter, r *http.Request, id uuid.UUID, detail string) bool {
	ctx := r.Context()

	if _, err := s.queries.GetProject(ctx, id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			problem.Write(w, http.StatusNotFound, problem.TypeNotFound,
				"Not found", unknownProjectDetail)
			return false
		}
		writeInternal(ctx, w, "reading the project a relation read is about", err, detail)
		return false
	}
	return true
}

// refuseAdjustments answers a metadata document whose pricing adjustments the
// schema refuses: one field error per violation, each located at the element
// and the member that has to change, so a caller corrects all of them in one
// go rather than one per round trip.
//
// Any other failure is this service disagreeing with itself: the contract has
// already refused every metadata that is not an object, so nothing a caller
// sent fails here in another way. It is logged and answered as an internal
// failure.
func refuseAdjustments(ctx context.Context, w http.ResponseWriter, err error, message, detail string) {
	var invalid *adjustment.InvalidError
	if !errors.As(err, &invalid) {
		writeInternal(ctx, w, message, err, detail)
		return
	}

	// A refused write leaves no audit row, so the refusal is logged with the
	// caller it came from. The violations are counted rather than named: they
	// go to the caller in the answer, and the count is what says how hard
	// someone is trying.
	Logger(ctx).Warn("refusing pricing adjustments the schema does not accept",
		"actor", queryActor(ctx), "violations", len(invalid.Violations))

	errs := make([]problem.FieldError, len(invalid.Violations))
	for i, violation := range invalid.Violations {
		errs[i] = problem.FieldError{
			// The contract validator spells a body location with dots, so the
			// pointer below the member is respelled the same way. It is empty
			// when the array as a whole is refused.
			Loc: "body.metadata." + adjustment.MetadataKey + strings.ReplaceAll(violation.Location, "/", "."),
			Msg: violation.Message,
		}
	}
	problem.Write(w, http.StatusUnprocessableEntity, problem.TypeValidation,
		"Validation failed", invalidAdjustmentsDetail, errs...)
}

// relationOf renders one relation row as the answer the contract promises. The
// instants come out in UTC, the zone this API states them in, and the metadata
// is decoded rather than passed through, because the contract types it as an
// object and a row written past this API could hold anything.
//
// An open relation renders valid_to as null, which is what the column holding
// NULL means: it has no end yet, rather than one that has passed.
func relationOf(row sqlcgen.ProjectRelation) (Relation, error) {
	var metadata map[string]any
	if err := json.Unmarshal(row.Metadata, &metadata); err != nil {
		return Relation{}, fmt.Errorf("decoding the metadata of %s: %w", row.ID, err)
	}

	item := Relation{
		Id:           row.ID,
		SourceId:     row.SourceID,
		TargetId:     row.TargetID,
		RelationType: row.RelationType,
		Metadata:     metadata,
		ValidFrom:    row.ValidFrom.Time.UTC(),
		CreatedAt:    row.CreatedAt.Time.UTC(),
	}
	if row.ValidTo.Valid {
		at := row.ValidTo.Time.UTC()
		item.ValidTo = &at
	}
	return item, nil
}

// instantOr resolves which instant a point-in-time read describes: the one the
// request names, or now when it names none.
func instantOr(at *time.Time) time.Time {
	if at == nil {
		return time.Now()
	}
	return *at
}
