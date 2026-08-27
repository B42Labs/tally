package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/b42labs/tally/internal/reporting/audit"
	"github.com/b42labs/tally/internal/reporting/httpapi/problem"
	"github.com/b42labs/tally/internal/reporting/projects"
	"github.com/b42labs/tally/internal/reporting/store/sqlcgen"
)

// The audit trail the update of the registry leaves. The registration writes
// its own row from projects.Register.
const (
	auditObjectProjects = projects.AuditObject
	actionUpdateProject = auditObjectProjects + ".update"
)

// projectCursorKeys is how many parts the sort key of the project list has: the
// two columns of the registry's unique key, in the order ORDER BY names them.
const projectCursorKeys = 2

// unknownProjectDetail answers every read and every write addressing a project
// this registry does not hold.
const unknownProjectDetail = "this project is not registered"

// duplicateProjectDetail answers a registration whose key pair the registry
// already holds. It names the pair rather than the row, because that pair is
// what the caller has to change to get through.
const duplicateProjectDetail = "a project with this cloud and external id is already registered"

// virtualKeyDetail answers a registration that breaks decision D1. It names the
// rule the caller has to satisfy, because either member of the pair may be the
// one to change.
const virtualKeyDetail = "a meta or partner project carries its platform as its cloud, and no other project carries meta or partner as its cloud"

// The code Postgres reports a unique violation under. violatesUnique matches it
// together with the name of the constraint that was violated.
const uniqueViolation = "23505"

// The details that answer a failure of one of these routes a caller can do
// nothing about. Which operation failed is what they tell apart, so each is
// spelled once rather than at every writeInternal of that operation.
const (
	projectsDetail      = "the projects could not be read"
	projectDetail       = "the project could not be read"
	createProjectDetail = "the project could not be registered"
	updateProjectDetail = "the project could not be updated"
)

// CreateProject registers one project and answers it under the id the other
// operations address it by.
//
// The row and the audit row naming it share one transaction, so a registration
// the log does not account for never reaches the database. What a virtual
// project carries as its cloud and whether the pair is a duplicate are both
// decided by projects.Register, the second over the unique key on
// (cloud, external_id) in the database rather than in a read before the insert:
// two racing registrations of one pair would both pass such a read, and one of
// them would still have to be refused here.
func (s *server) CreateProject(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// The metadata is kept as it arrived rather than decoded into a map and
	// marshalled again: the column takes the document itself.
	var body struct {
		Platform   string          `json:"platform"`
		Cloud      string          `json:"cloud"`
		ExternalID string          `json:"external_id"`
		Name       *string         `json:"name"`
		Metadata   json.RawMessage `json:"metadata"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		// The validator checked this body against the contract before the
		// request got here, so a failure now is this service disagreeing with
		// itself rather than a caller error.
		writeInternal(ctx, w, "decoding a registration the contract accepted", err,
			createProjectDetail)
		return
	}

	var stored sqlcgen.Project
	if err := s.store.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		stored, err = projects.Register(ctx, sqlcgen.New(tx), queryActor(ctx),
			projects.Registration{
				Platform:   body.Platform,
				Cloud:      body.Cloud,
				ExternalID: body.ExternalID,
				Name:       filterText(body.Name),
				Metadata:   body.Metadata,
			})
		return err
	}); err != nil {
		switch {
		case errors.Is(err, projects.ErrVirtualKey):
			problem.Write(w, http.StatusUnprocessableEntity, problem.TypeValidation,
				"Validation failed", virtualKeyDetail,
				problem.FieldError{
					Loc: "body.cloud",
					Msg: "has to equal the platform of a virtual project",
				})
		case errors.Is(err, projects.ErrAlreadyRegistered):
			problem.Write(w, http.StatusConflict, problem.TypeConflict,
				"Conflict", duplicateProjectDetail)
		default:
			writeInternal(ctx, w, "registering a project", err, createProjectDetail)
		}
		return
	}

	item, err := projectOf(stored)
	if err != nil {
		writeInternal(ctx, w, "rendering a project", err, createProjectDetail)
		return
	}
	// A 201 names where the created thing lives, which is the route that reads
	// it back.
	w.Header().Set("Location", "/api/v1/projects/"+item.Id.String())
	writeJSONStatus(w, http.StatusCreated, item)
}

// ListProjects answers one page of the registry, narrowed by the filters the
// request carries. The page is read one row longer than it is served, which is
// what trimPage turns into the cursor decision.
//
// Nothing here reads a principal or resolves a scope. The registry is the
// organizational structure a project scope is built out of, so there is no
// project-scoped view of it: the dispatch table puts the reads behind read_all,
// and every request that arrives may read the whole table.
func (s *server) ListProjects(w http.ResponseWriter, r *http.Request, params ListProjectsParams) {
	ctx := r.Context()

	var cursorCloud, cursorExternalID pgtype.Text
	if params.Cursor != nil {
		keys, err := decodeCursor(*params.Cursor, projectCursorKeys)
		if err != nil {
			refuseCursor(ctx, w, err)
			return
		}
		cursorCloud = pgtype.Text{String: keys[0], Valid: true}
		cursorExternalID = pgtype.Text{String: keys[1], Valid: true}
	}

	limit := pageLimit(params.Limit)

	rows, err := s.queries.ListProjects(ctx, sqlcgen.ListProjectsParams{
		Platform:         filterText(params.Platform),
		Cloud:            filterText(params.Cloud),
		ExternalID:       filterText(params.ExternalId),
		CursorCloud:      cursorCloud,
		CursorExternalID: cursorExternalID,
		PageSize:         int32(limit) + 1,
	})
	if err != nil {
		writeInternal(ctx, w, "listing projects", err, projectsDetail)
		return
	}

	rows, more := trimPage(rows, limit)
	items := make([]Project, len(rows))
	for i, row := range rows {
		if items[i], err = projectOf(row); err != nil {
			writeInternal(ctx, w, "decoding a stored project", err, projectsDetail)
			return
		}
	}

	list := ProjectList{Items: items}
	if more {
		// The cursor names the last item served, so the next page starts at the
		// row after it. The two keys are the columns themselves, so nothing has
		// to be formatted for the round trip.
		last := items[len(items)-1]
		cursor := encodeCursor([]string{last.Cloud, last.ExternalId})
		list.NextCursor = &cursor
	}
	writeJSON(w, list)
}

// GetProject answers with one registered project.
func (s *server) GetProject(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	ctx := r.Context()

	row, err := s.queries.GetProject(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			problem.Write(w, http.StatusNotFound, problem.TypeNotFound,
				"Not found", unknownProjectDetail)
			return
		}
		writeInternal(ctx, w, "reading a project", err, projectDetail)
		return
	}

	item, err := projectOf(row)
	if err != nil {
		writeInternal(ctx, w, "rendering a project", err, projectDetail)
		return
	}
	writeJSON(w, item)
}

// UpdateProject changes the name or the metadata of one project and answers it
// as it now stands. The row and the audit row naming it share one transaction,
// so a change the log does not account for never reaches the database.
//
// A member the request leaves out arrives at the query as NULL, which its
// COALESCE reads as leaving the column alone. The contract types neither member
// as nullable, so an explicit null is refused in front of this and never has to
// be told apart from an absent member.
func (s *server) UpdateProject(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	ctx := r.Context()

	var body struct {
		Name     *string         `json:"name"`
		Metadata json.RawMessage `json:"metadata"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeInternal(ctx, w, "decoding an update the contract accepted", err,
			updateProjectDetail)
		return
	}

	var stored sqlcgen.Project
	if err := s.store.WithTx(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		var err error
		if stored, err = q.UpdateProject(ctx, sqlcgen.UpdateProjectParams{
			ID:       id,
			Name:     filterText(body.Name),
			Metadata: body.Metadata,
		}); err != nil {
			return fmt.Errorf("updating %s: %w", id, err)
		}
		return audit.Log(ctx, q, audit.Entry{
			Actor:      queryActor(ctx),
			Action:     actionUpdateProject,
			ObjectType: auditObjectProjects,
			ObjectID:   stored.ID.String(),
		})
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			problem.Write(w, http.StatusNotFound, problem.TypeNotFound,
				"Not found", unknownProjectDetail)
			return
		}
		writeInternal(ctx, w, "updating a project", err, updateProjectDetail)
		return
	}

	item, err := projectOf(stored)
	if err != nil {
		writeInternal(ctx, w, "rendering a project", err, updateProjectDetail)
		return
	}
	writeJSON(w, item)
}

// projectOf renders one registry row as the answer the contract promises. The
// instant comes out in UTC, the zone this API states them in, and the metadata
// is decoded rather than passed through, because the contract types it as an
// object and a row written past this API could hold anything.
//
// A project registered without a name renders as a null member, which is what
// the column holding NULL means; the empty string would claim a name of no
// characters.
func projectOf(row sqlcgen.Project) (Project, error) {
	var metadata map[string]any
	if err := json.Unmarshal(row.Metadata, &metadata); err != nil {
		return Project{}, fmt.Errorf("decoding the metadata of %s: %w", row.ID, err)
	}

	item := Project{
		Id:         row.ID,
		Platform:   row.Platform,
		Cloud:      row.Cloud,
		ExternalId: row.ExternalID,
		Metadata:   metadata,
		CreatedAt:  row.CreatedAt.Time.UTC(),
	}
	if row.Name.Valid {
		name := row.Name.String
		item.Name = &name
	}
	return item, nil
}

// violatesUnique reports whether err is Postgres refusing a write against the
// named unique constraint.
func violatesUnique(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) &&
		pgErr.Code == uniqueViolation && pgErr.ConstraintName == constraint
}
