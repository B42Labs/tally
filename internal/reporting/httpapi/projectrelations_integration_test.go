package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/b42labs/tally/internal/reporting/auth"
	"github.com/b42labs/tally/internal/reporting/httpapi/problem"
	"github.com/b42labs/tally/internal/reporting/store/storetest"
)

// attributingType is the relation type the harness configures the cycle guard
// with, which is the one the configuration defaults to. plainType is a type
// outside that list, so a relation of it is created wherever the graph holds
// one at all.
const (
	attributingType = "infrastructure_tenant"
	plainType       = "peer"
)

// The span the temporal subtests keep one relation valid over. The instants are
// fixed rather than measured from now, so what a read answers depends on the
// `at` it carries rather than on when the suite runs.
var (
	relationOpened = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	relationClosed = time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
)

// TestProjectRelationsOverHTTP drives the five relation operations through the
// whole stack: the contract validator, the role a route demands, the
// transactions behind the writes, and the rows they leave. The subtests share
// one database and register their projects under clouds of their own, so a
// relation one of them creates stays out of what another walks.
func TestProjectRelationsOverHTTP(t *testing.T) {
	db := storetest.NewDB(t)
	a := newAPI(t, db.Store)

	adminID, adminToken := seedAPIToken(t, a.queries, auth.RoleAdmin, nil)
	_, readAllToken := seedAPIToken(t, a.queries, auth.RoleReadAll, nil)
	_, projectToken := seedAPIToken(t, a.queries, auth.RoleProject, []uuid.UUID{uuid.New()})

	t.Run("relates two projects and answers the relation under the id that closes it", func(t *testing.T) {
		pair := registerProjects(t, a, adminToken, "os-rel-create", 2)
		metadata := map[string]any{"owner": "team-a"}

		rec := a.call(t, http.MethodPost, relationsPath(pair[0].Id), adminToken,
			projectDocument(t, map[string]any{
				"target_id": pair[1].Id, "relation_type": plainType, "metadata": metadata,
			}))

		created := createdRelation(t, rec)
		if got, want := rec.Header().Get("Location"), relationTarget(pair[0].Id, created.Id); got != want {
			t.Errorf("Location = %q, want %q", got, want)
		}
		if created.SourceId != pair[0].Id || created.TargetId != pair[1].Id ||
			created.RelationType != plainType {
			t.Errorf("relation = %+v, want %s to %s as %s",
				created, pair[0].Id, pair[1].Id, plainType)
		}
		if !reflect.DeepEqual(created.Metadata, metadata) {
			t.Errorf("metadata = %v, want %v", created.Metadata, metadata)
		}
		// A creation that names no start is valid from the instant it was
		// written, which the database clock decides.
		if since := time.Since(created.ValidFrom); since > time.Minute || since < -time.Minute {
			t.Errorf("valid_from = %s, want about now", created.ValidFrom)
		}
		// An open relation renders its end as null rather than dropping the
		// member: a client reads "still valid" off it.
		if got := memberRaw(t, rec, "valid_to"); got != "null" {
			t.Errorf("valid_to = %s, want null while the relation is open", got)
		}
		// The instant is read off the wire rather than off the model: time.Time
		// has parsed the zone away by the time the model carries it.
		if at := memberOnTheWire(t, rec, "valid_from"); !strings.HasSuffix(at, "Z") {
			t.Errorf("valid_from = %q, want it stated in UTC", at)
		}
		// The answer names the row that was written rather than anything
		// derived from the request.
		if got, want := storedRelationIDs(t, a, pair[0].Id, pair[1].Id),
			[]uuid.UUID{created.Id}; !slices.Equal(got, want) {
			t.Errorf("stored relations = %v, want the answered %v", got, want)
		}

		rows := projectAudits(t, a, adminID.String(), actionCreateRelation, created.Id)
		if len(rows) != 1 {
			t.Fatalf("creation audit rows = %v, want exactly one", rows)
		}
		if got := rows[0].objectType; got != auditObjectRelations {
			t.Errorf("audit object type = %q, want %q", got, auditObjectRelations)
		}
	})

	t.Run("holds one open relation per source, target and type", func(t *testing.T) {
		pair := registerProjects(t, a, adminToken, "os-rel-conflict", 2)
		first := relate(t, a, adminToken, pair[0].Id, pair[1].Id, plainType)

		rec := a.call(t, http.MethodPost, relationsPath(pair[0].Id), adminToken,
			relationDocument(t, pair[1].Id, plainType))

		assertProblem(t, rec, http.StatusConflict, problem.TypeConflict)
		if got, want := problemDetail(t, rec),
			"a relation of this type between these projects is already active"; got != want {
			t.Errorf("detail = %q, want %q", got, want)
		}

		// The index covers the open relations alone, so the same triple is
		// related again once the first relation is closed.
		closeRelation(t, a, adminToken, pair[0].Id, first.Id)
		second := relate(t, a, adminToken, pair[0].Id, pair[1].Id, plainType)

		if got, want := storedRelationIDs(t, a, pair[0].Id, pair[1].Id),
			[]uuid.UUID{first.Id, second.Id}; !slices.Equal(got, want) {
			t.Errorf("stored relations = %v, want the closed %s beside the new %s",
				got, first.Id, second.Id)
		}
	})

	t.Run("relates real projects to virtual ones like any other", func(t *testing.T) {
		real := registerProjects(t, a, adminToken, "os-rel-virtual", 1)[0]
		partner := registeredProject(t, a.call(t, http.MethodPost, projectsRoute, adminToken,
			projectDocument(t, map[string]any{
				"platform": "partner", "cloud": "partner", "external_id": "partner-corp-rel",
			})))
		meta := registeredProject(t, a.call(t, http.MethodPost, projectsRoute, adminToken,
			projectDocument(t, map[string]any{
				"platform": "meta", "cloud": "meta", "external_id": "customer-alpha-rel",
			})))

		// The relation to the partner names a start of its own, so the temporal
		// read further down has a span to ask about.
		managed := createdRelation(t, a.call(t, http.MethodPost, relationsPath(real.Id),
			adminToken, projectDocument(t, map[string]any{
				"target_id": partner.Id, "relation_type": "managed_by",
				"valid_from": relationOpened,
			})))
		member := relate(t, a, adminToken, real.Id, meta.Id, "member_of")

		listed := relationListOf(t, a.call(t, http.MethodGet,
			relationsPath(real.Id)+"?direction=outgoing", adminToken, nil))
		if got, want := relationIDs(listed.Items),
			[]uuid.UUID{managed.Id, member.Id}; !slices.Equal(got, want) {
			t.Errorf("served relations = %v, want %v", got, want)
		}

		walk := relatedListOf(t, a.call(t, http.MethodGet,
			relatedPath(real.Id)+"?depth=1", adminToken, nil))
		reached := make(map[uuid.UUID]string, len(walk.Items))
		for _, item := range walk.Items {
			reached[item.Project.Id] = item.RelationType
		}
		wantReached := map[uuid.UUID]string{partner.Id: "managed_by", meta.Id: "member_of"}
		if !reflect.DeepEqual(reached, wantReached) {
			t.Errorf("walked projects = %v, want %v", reached, wantReached)
		}

		// Neither type attributes cost, so no cycle walk runs and the relation
		// back out of the meta-project is created like any other.
		relate(t, a, adminToken, meta.Id, real.Id, "member_of")

		relationFrom(t, a.call(t, http.MethodPatch, relationTarget(real.Id, managed.Id),
			adminToken, projectDocument(t, map[string]any{"valid_to": relationClosed})))

		// The relation to the partner ran from relationOpened to relationClosed,
		// which an instant inside the span finds and one after it does not.
		inside := relationListOf(t, a.call(t, http.MethodGet,
			relationsPath(real.Id)+"?relation_type=managed_by&at="+
				instant(time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)), adminToken, nil))
		if got, want := relationIDs(inside.Items),
			[]uuid.UUID{managed.Id}; !slices.Equal(got, want) {
			t.Errorf("relations inside the span = %v, want %v", got, want)
		}
		after := relationListOf(t, a.call(t, http.MethodGet,
			relationsPath(real.Id)+"?relation_type=managed_by&at="+
				instant(time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)), adminToken, nil))
		if got := relationIDs(after.Items); len(got) != 0 {
			t.Errorf("relations after the span = %v, want none", got)
		}

		closeRelation(t, a, adminToken, real.Id, member.Id)
		remaining := relationListOf(t, a.call(t, http.MethodGet,
			relationsPath(real.Id)+"?direction=outgoing", adminToken, nil))
		if got := relationIDs(remaining.Items); slices.Contains(got, member.Id) {
			t.Errorf("served relations = %v, want the closed %s gone", got, member.Id)
		}

		// A relation reaching a virtual project is refused like any other one
		// whose target the registry does not hold.
		rec := a.call(t, http.MethodPost, relationsPath(real.Id), adminToken,
			relationDocument(t, uuid.New(), "member_of"))

		assertProblem(t, rec, http.StatusUnprocessableEntity, problem.TypeValidation)
		if got, want := problemDetail(t, rec), "this project is not registered"; got != want {
			t.Errorf("detail = %q, want %q", got, want)
		}
	})

	t.Run("closes a relation and answers a repeated close the same way", func(t *testing.T) {
		pair := registerProjects(t, a, adminToken, "os-rel-close", 2)
		relation := relate(t, a, adminToken, pair[0].Id, pair[1].Id, plainType)

		closeRelation(t, a, adminToken, pair[0].Id, relation.Id)
		closedAt := storedValidTo(t, a, relation.Id)
		if closedAt == nil {
			t.Fatal("the relation is still open, want the close to have ended it")
		}

		closeRelation(t, a, adminToken, pair[0].Id, relation.Id)

		// The second call changed nothing: the instant the relation ended at is
		// the one it was closed at, and no audit row claims a second close.
		if got := storedValidTo(t, a, relation.Id); got == nil || !got.Equal(*closedAt) {
			t.Errorf("valid_to = %v, want the %s the first close wrote", got, closedAt)
		}
		if rows := projectAudits(t, a, adminID.String(), actionCloseRelation, relation.Id); len(rows) != 1 {
			t.Errorf("close audit rows = %v, want exactly one", rows)
		}

		// A relation is addressed under the project it leaves, so closing it
		// through the project it reaches finds nothing.
		rec := a.call(t, http.MethodDelete, relationTarget(pair[1].Id, relation.Id), adminToken, nil)

		assertProblem(t, rec, http.StatusNotFound, problem.TypeNotFound)
		if got, want := problemDetail(t, rec),
			"this relation is not stored for this project"; got != want {
			t.Errorf("detail = %q, want %q", got, want)
		}
	})

	t.Run("refuses a relation it cannot store", func(t *testing.T) {
		pair := registerProjects(t, a, adminToken, "os-rel-refuse", 2)

		t.Run("one that leaves and reaches the same project", func(t *testing.T) {
			rec := a.call(t, http.MethodPost, relationsPath(pair[0].Id), adminToken,
				relationDocument(t, pair[0].Id, plainType))

			assertProblem(t, rec, http.StatusUnprocessableEntity, problem.TypeValidation)
			if got, want := problemDetail(t, rec),
				"a relation cannot leave and reach the same project"; got != want {
				t.Errorf("detail = %q, want %q", got, want)
			}
			if got, want := fieldErrorLocations(t, rec), []string{"body.target_id"}; !slices.Equal(got, want) {
				t.Errorf("field errors = %v, want %v", got, want)
			}
		})

		t.Run("one reaching a project the registry does not hold", func(t *testing.T) {
			rec := a.call(t, http.MethodPost, relationsPath(pair[0].Id), adminToken,
				relationDocument(t, uuid.New(), plainType))

			assertProblem(t, rec, http.StatusUnprocessableEntity, problem.TypeValidation)
			if got, want := problemDetail(t, rec), "this project is not registered"; got != want {
				t.Errorf("detail = %q, want %q", got, want)
			}
		})

		t.Run("one leaving a project the registry does not hold", func(t *testing.T) {
			rec := a.call(t, http.MethodPost, relationsPath(uuid.New()), adminToken,
				relationDocument(t, pair[1].Id, plainType))

			assertProblem(t, rec, http.StatusNotFound, problem.TypeNotFound)
			if got, want := problemDetail(t, rec), "this project is not registered"; got != want {
				t.Errorf("detail = %q, want %q", got, want)
			}
		})
	})

	t.Run("tells an empty neighborhood from a project it does not hold", func(t *testing.T) {
		alone := registerProjects(t, a, adminToken, "os-rel-empty", 1)[0]
		unknown := uuid.New()

		for _, tc := range []struct {
			name  string
			route func(uuid.UUID) string
		}{
			{name: "the relation list", route: relationsPath},
			{name: "the traversal", route: relatedPath},
		} {
			t.Run(tc.name+" of a project with nothing to answer", func(t *testing.T) {
				rec := a.call(t, http.MethodGet, tc.route(alone.Id), adminToken, nil)

				if got := rec.Code; got != http.StatusOK {
					t.Fatalf("status = %d, want %d (body %q)", got, http.StatusOK, rec.Body)
				}
				// The raw body rather than the decoded one: a client iterates
				// items without a nil check, which null would break and [] does
				// not.
				if got, want := strings.TrimSpace(rec.Body.String()), `{"items":[]}`; got != want {
					t.Errorf("body = %s, want %s", got, want)
				}
			})

			t.Run(tc.name+" of a project the registry does not hold", func(t *testing.T) {
				rec := a.call(t, http.MethodGet, tc.route(unknown), adminToken, nil)

				assertProblem(t, rec, http.StatusNotFound, problem.TypeNotFound)
				if got, want := problemDetail(t, rec), "this project is not registered"; got != want {
					t.Errorf("detail = %q, want %q", got, want)
				}
			})
		}
	})

	t.Run("serves the relations of one project by direction", func(t *testing.T) {
		trio := registerProjects(t, a, adminToken, "os-rel-direction", 3)
		named := namesOf(trio...)
		// Created in this order, which is the order the answers hold them in.
		relate(t, a, adminToken, trio[0].Id, trio[1].Id, plainType)
		relate(t, a, adminToken, trio[2].Id, trio[0].Id, plainType)
		relate(t, a, adminToken, trio[0].Id, trio[2].Id, attributingType)

		for _, tc := range []struct {
			name  string
			query string
			want  []string
		}{
			{name: "the ones it leaves", query: "?direction=outgoing", want: []string{"p1->p2", "p1->p3"}},
			{name: "the ones it reaches", query: "?direction=incoming", want: []string{"p3->p1"}},
			{
				name:  "either",
				query: "?direction=both",
				want:  []string{"p1->p2", "p3->p1", "p1->p3"},
			},
			{name: "either by default", want: []string{"p1->p2", "p3->p1", "p1->p3"}},
			{
				name:  "narrowed to one type",
				query: "?relation_type=" + plainType,
				want:  []string{"p1->p2", "p3->p1"},
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				list := relationListOf(t, a.call(t, http.MethodGet,
					relationsPath(trio[0].Id)+tc.query, adminToken, nil))

				if got := relationEdges(list.Items, named); !slices.Equal(got, tc.want) {
					t.Errorf("served relations = %v, want %v", got, tc.want)
				}
			})
		}
	})

	t.Run("answers the graph as it stood at one instant", func(t *testing.T) {
		pair := registerProjects(t, a, adminToken, "os-rel-temporal", 2)
		relation := createdRelation(t, a.call(t, http.MethodPost, relationsPath(pair[0].Id), adminToken,
			projectDocument(t, map[string]any{
				"target_id": pair[1].Id, "relation_type": plainType, "valid_from": relationOpened,
			})))
		relationFrom(t, a.call(t, http.MethodPatch, relationTarget(pair[0].Id, relation.Id), adminToken,
			projectDocument(t, map[string]any{"valid_to": relationClosed})))

		for _, tc := range []struct {
			name  string
			query string
			want  int
		}{
			{name: "the instant it started", query: "?at=" + instant(relationOpened), want: 1},
			{name: "inside the span", query: "?at=" + instant(relationOpened.AddDate(0, 0, 9)), want: 1},
			// The end is the exclusive bound, so the relation is gone at the
			// instant it was closed at rather than one moment later.
			{name: "the instant it was closed at", query: "?at=" + instant(relationClosed), want: 0},
			{name: "after the span", query: "?at=" + instant(relationClosed.AddDate(0, 0, 17)), want: 0},
			{name: "now, which is what a request naming no instant asks for", want: 0},
		} {
			t.Run(tc.name, func(t *testing.T) {
				list := relationListOf(t, a.call(t, http.MethodGet,
					relationsPath(pair[0].Id)+tc.query, adminToken, nil))
				walk := relatedListOf(t, a.call(t, http.MethodGet,
					relatedPath(pair[0].Id)+tc.query, adminToken, nil))

				if got := len(list.Items); got != tc.want {
					t.Errorf("listed relations = %d, want %d", got, tc.want)
				}
				if got := len(walk.Items); got != tc.want {
					t.Errorf("walked projects = %d, want %d", got, tc.want)
				}
			})
		}
	})

	t.Run("updates a relation", func(t *testing.T) {
		t.Run("ends it at the instant the request names", func(t *testing.T) {
			pair := registerProjects(t, a, adminToken, "os-rel-update", 2)
			relation := createdRelation(t, a.call(t, http.MethodPost, relationsPath(pair[0].Id),
				adminToken, projectDocument(t, map[string]any{
					"target_id": pair[1].Id, "relation_type": plainType,
					"valid_from": relationOpened,
				})))

			closed := relationFrom(t, a.call(t, http.MethodPatch,
				relationTarget(pair[0].Id, relation.Id), adminToken,
				projectDocument(t, map[string]any{"valid_to": relationClosed})))
			if closed.ValidTo == nil || !closed.ValidTo.Equal(relationClosed) {
				t.Errorf("valid_to = %v, want %s", closed.ValidTo, relationClosed)
			}

			// A relation that is closed already has the instant it ended at
			// corrected rather than being refused.
			corrected := relationClosed.AddDate(0, 0, 5)
			again := relationFrom(t, a.call(t, http.MethodPatch,
				relationTarget(pair[0].Id, relation.Id), adminToken,
				projectDocument(t, map[string]any{"valid_to": corrected})))
			if again.ValidTo == nil || !again.ValidTo.Equal(corrected) {
				t.Errorf("valid_to = %v, want the corrected %s", again.ValidTo, corrected)
			}

			// A request that names the metadata alone leaves the end as it is.
			metadata := map[string]any{"owner": "team-b"}
			kept := relationFrom(t, a.call(t, http.MethodPatch,
				relationTarget(pair[0].Id, relation.Id), adminToken,
				projectDocument(t, map[string]any{"metadata": metadata})))
			if !reflect.DeepEqual(kept.Metadata, metadata) {
				t.Errorf("metadata = %v, want %v", kept.Metadata, metadata)
			}
			if kept.ValidTo == nil || !kept.ValidTo.Equal(corrected) {
				t.Errorf("valid_to = %v, want the untouched %s", kept.ValidTo, corrected)
			}

			if rows := projectAudits(t, a, adminID.String(), actionUpdateRelation, relation.Id); len(rows) != 3 {
				t.Errorf("update audit rows = %v, want one per update", rows)
			}
		})

		t.Run("refuses an end that is not after the start", func(t *testing.T) {
			pair := registerProjects(t, a, adminToken, "os-rel-update-span", 2)
			relation := createdRelation(t, a.call(t, http.MethodPost, relationsPath(pair[0].Id),
				adminToken, projectDocument(t, map[string]any{
					"target_id": pair[1].Id, "relation_type": plainType,
					"valid_from": relationOpened,
				})))

			for _, tc := range []struct {
				name    string
				validTo time.Time
			}{
				{name: "before the start", validTo: relationOpened.AddDate(0, -1, 0)},
				{
					name:    "at the start, which is a relation that was never valid",
					validTo: relationOpened,
				},
			} {
				t.Run(tc.name, func(t *testing.T) {
					rec := a.call(t, http.MethodPatch, relationTarget(pair[0].Id, relation.Id),
						adminToken, projectDocument(t, map[string]any{"valid_to": tc.validTo}))

					assertProblem(t, rec, http.StatusUnprocessableEntity, problem.TypeValidation)
					if got, want := problemDetail(t, rec),
						"valid_to has to be after valid_from"; got != want {
						t.Errorf("detail = %q, want %q", got, want)
					}
					if got := storedValidTo(t, a, relation.Id); got != nil {
						t.Errorf("valid_to = %s, want a refused update to leave the relation open", got)
					}
				})
			}
		})

		t.Run("refuses a relation that does not leave the project of the path", func(t *testing.T) {
			pair := registerProjects(t, a, adminToken, "os-rel-update-source", 2)
			relation := relate(t, a, adminToken, pair[0].Id, pair[1].Id, plainType)

			rec := a.call(t, http.MethodPatch, relationTarget(pair[1].Id, relation.Id), adminToken,
				projectDocument(t, map[string]any{"metadata": map[string]any{"owner": "team-c"}}))

			assertProblem(t, rec, http.StatusNotFound, problem.TypeNotFound)
			if got, want := problemDetail(t, rec),
				"this relation is not stored for this project"; got != want {
				t.Errorf("detail = %q, want %q", got, want)
			}
		})

		t.Run("refuses a body that changes nothing", func(t *testing.T) {
			pair := registerProjects(t, a, adminToken, "os-rel-update-empty", 2)
			relation := relate(t, a, adminToken, pair[0].Id, pair[1].Id, plainType)

			// The contract asks an update for at least one member, so the empty
			// object is refused rather than served as an update of nothing.
			rec := a.call(t, http.MethodPatch, relationTarget(pair[0].Id, relation.Id),
				adminToken, []byte(`{}`))

			assertProblem(t, rec, http.StatusBadRequest, problem.TypeValidation)
			if rows := projectAudits(t, a, adminID.String(), actionUpdateRelation, relation.Id); len(rows) != 0 {
				t.Errorf("update audit rows = %v, want none for a refused update", rows)
			}
		})
	})

	t.Run("keeps the attributing relations a forest", func(t *testing.T) {
		t.Run("refuses the relation that would close a cycle", func(t *testing.T) {
			pair := registerProjects(t, a, adminToken, "os-rel-cycle", 2)
			relate(t, a, adminToken, pair[0].Id, pair[1].Id, attributingType)

			rec := a.call(t, http.MethodPost, relationsPath(pair[1].Id), adminToken,
				relationDocument(t, pair[0].Id, attributingType))

			assertProblem(t, rec, http.StatusUnprocessableEntity, problem.TypeRelationCycle)
			if got, want := problemDetail(t, rec),
				"this relation would close a cycle over the relation types that attribute cost"; got != want {
				t.Errorf("detail = %q, want %q", got, want)
			}
			if got := storedRelationIDs(t, a, pair[1].Id, pair[0].Id); len(got) != 0 {
				t.Errorf("stored relations = %v, want a refused creation to write none", got)
			}

			// The walk covers the attributing types alone, so the same pair is
			// related the other way round under a type that carries no cost.
			back := relate(t, a, adminToken, pair[1].Id, pair[0].Id, plainType)
			if got, want := storedRelationIDs(t, a, pair[1].Id, pair[0].Id),
				[]uuid.UUID{back.Id}; !slices.Equal(got, want) {
				t.Errorf("stored relations = %v, want %v", got, want)
			}
		})

		t.Run("walks past the projects between the two ends", func(t *testing.T) {
			chain := registerProjects(t, a, adminToken, "os-rel-cycle-deep", 3)
			relate(t, a, adminToken, chain[0].Id, chain[1].Id, attributingType)
			relate(t, a, adminToken, chain[1].Id, chain[2].Id, attributingType)

			rec := a.call(t, http.MethodPost, relationsPath(chain[2].Id), adminToken,
				relationDocument(t, chain[0].Id, attributingType))

			assertProblem(t, rec, http.StatusUnprocessableEntity, problem.TypeRelationCycle)
			if got := storedRelationIDs(t, a, chain[2].Id, chain[0].Id); len(got) != 0 {
				t.Errorf("stored relations = %v, want a refused creation to write none", got)
			}
		})

		t.Run("lets one of two racing creations through", func(t *testing.T) {
			pair := registerProjects(t, a, adminToken, "os-rel-cycle-race", 2)
			// Each creation is fine on its own and the two are a cycle
			// together. They are driven as real requests, so each runs on a
			// connection of its own and the advisory lock is the only thing
			// that can order them. One pair is enough: the lock makes the
			// outcome the same whichever of the two gets there first.
			targets := [2]string{relationsPath(pair[0].Id), relationsPath(pair[1].Id)}
			bodies := [2][]byte{
				relationDocument(t, pair[1].Id, attributingType),
				relationDocument(t, pair[0].Id, attributingType),
			}

			var answers [2]*httptest.ResponseRecorder
			var racing sync.WaitGroup
			for i := range targets {
				racing.Add(1)
				go func() {
					defer racing.Done()
					answers[i] = a.call(t, http.MethodPost, targets[i], adminToken, bodies[i])
				}()
			}
			racing.Wait()

			created, refused := 0, 0
			for _, rec := range answers {
				switch rec.Code {
				case http.StatusCreated:
					created++
				case http.StatusUnprocessableEntity:
					refused++
					assertProblem(t, rec, http.StatusUnprocessableEntity, problem.TypeRelationCycle)
				default:
					t.Errorf("status = %d, want a creation or a refused cycle (body %q)",
						rec.Code, rec.Body)
				}
			}
			if created != 1 || refused != 1 {
				t.Errorf("%d creations and %d refusals, want one of each", created, refused)
			}

			stored := append(storedRelationIDs(t, a, pair[0].Id, pair[1].Id),
				storedRelationIDs(t, a, pair[1].Id, pair[0].Id)...)
			if len(stored) != 1 {
				t.Errorf("stored relations = %v, want the one creation that got through", stored)
			}
		})
	})

	t.Run("walks the graph out of one project", func(t *testing.T) {
		chain := registerProjects(t, a, adminToken, "os-rel-walk", 3)
		first := relate(t, a, adminToken, chain[0].Id, chain[1].Id, plainType)
		second := relate(t, a, adminToken, chain[1].Id, chain[2].Id, plainType)

		t.Run("one relation out when the request names no depth", func(t *testing.T) {
			list := relatedListOf(t, a.call(t, http.MethodGet, relatedPath(chain[0].Id), adminToken, nil))

			if got, want := walkSteps(list.Items), []string{"p2@1/1"}; !slices.Equal(got, want) {
				t.Fatalf("walked projects = %v, want %v", got, want)
			}
			if got, want := list.Items[0].Path, []uuid.UUID{first.Id}; !slices.Equal(got, want) {
				t.Errorf("path = %v, want the relation %v it was reached over", got, want)
			}
			if got := list.Items[0].RelationType; got != plainType {
				t.Errorf("relation type = %q, want %q", got, plainType)
			}
		})

		t.Run("as far out as the request asks", func(t *testing.T) {
			list := relatedListOf(t, a.call(t, http.MethodGet,
				relatedPath(chain[0].Id)+"?depth=2", adminToken, nil))

			if got, want := walkSteps(list.Items), []string{"p2@1/1", "p3@2/2"}; !slices.Equal(got, want) {
				t.Fatalf("walked projects = %v, want %v", got, want)
			}
			if got, want := list.Items[1].Path,
				[]uuid.UUID{first.Id, second.Id}; !slices.Equal(got, want) {
				t.Errorf("path = %v, want the relations %v in walk order", got, want)
			}
		})

		t.Run("visiting a project once, so a cycle terminates", func(t *testing.T) {
			cycle := registerProjects(t, a, adminToken, "os-rel-walk-cycle", 2)
			relate(t, a, adminToken, cycle[0].Id, cycle[1].Id, plainType)
			relate(t, a, adminToken, cycle[1].Id, cycle[0].Id, plainType)

			list := relatedListOf(t, a.call(t, http.MethodGet,
				relatedPath(cycle[0].Id)+"?depth=10", adminToken, nil))

			// The relation back is walked and reaches the project the walk
			// started from, which is where it stops rather than going round.
			if got, want := walkSteps(list.Items), []string{"p2@1/1"}; !slices.Equal(got, want) {
				t.Errorf("walked projects = %v, want %v", got, want)
			}
		})
	})

	t.Run("refuses a request the contract does not describe", func(t *testing.T) {
		project := registerProjects(t, a, adminToken, "os-rel-contract", 1)[0]

		for _, tc := range []struct {
			name   string
			method string
			target string
			body   []byte
			want   []string
		}{
			{
				name: "a walk of no relation at all", method: http.MethodGet,
				target: relatedPath(project.Id) + "?depth=0", want: []string{"query.depth"},
			},
			{
				name: "a walk past the bound", method: http.MethodGet,
				target: relatedPath(project.Id) + "?depth=11", want: []string{"query.depth"},
			},
			{
				name: "a direction the contract does not name", method: http.MethodGet,
				target: relationsPath(project.Id) + "?direction=banana",
				want:   []string{"query.direction"},
			},
			{
				name: "an instant that is not one", method: http.MethodGet,
				target: relationsPath(project.Id) + "?at=yesterday", want: []string{"query.at"},
			},
			{
				// The contract carries the RFC 4122 pattern next to the format,
				// so the validator refuses the id before the route is matched.
				name: "a relation id that is not a UUID", method: http.MethodDelete,
				target: relationsPath(project.Id) + "/not-a-uuid",
				want:   []string{"path.relation_id"},
			},
			{
				name: "a target that is not a UUID", method: http.MethodPost,
				target: relationsPath(project.Id), want: []string{"body.target_id"},
				body: projectDocument(t, map[string]any{
					"target_id": "not-a-uuid", "relation_type": plainType,
				}),
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				rec := a.call(t, tc.method, tc.target, adminToken, tc.body)

				assertProblem(t, rec, http.StatusBadRequest, problem.TypeValidation)
				if got := fieldErrorLocations(t, rec); !slices.Equal(got, tc.want) {
					t.Errorf("field errors = %v, want %v", got, tc.want)
				}
			})
		}
	})

	t.Run("puts every operation behind the role it needs", func(t *testing.T) {
		pair := registerProjects(t, a, adminToken, "os-rel-roles", 2)
		existing := relate(t, a, adminToken, pair[0].Id, pair[1].Id, "roles-existing")

		t.Run("no token at all", func(t *testing.T) {
			for _, op := range relationOperations(t, pair[0], pair[1], existing, "roles-anonymous") {
				t.Run(op.name, func(t *testing.T) {
					assertChallenged(t, a.call(t, op.method, op.target, "", op.body))
				})
			}
		})

		for _, tc := range []struct {
			name string
			// relationType is what this case's creation asks for, so the one
			// case whose creation goes through collides with nothing.
			relationType string
			token        string
			// reads says whether the token reaches the two reads, admin whether
			// it reaches the three writes.
			reads bool
			admin bool
		}{
			{name: "a project token", relationType: "roles-project", token: projectToken},
			{
				name: "a read_all token", relationType: "roles-read-all",
				token: readAllToken, reads: true,
			},
			{
				name: "an admin token", relationType: "roles-admin",
				token: adminToken, reads: true, admin: true,
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				for _, op := range relationOperations(t, pair[0], pair[1], existing, tc.relationType) {
					t.Run(op.name, func(t *testing.T) {
						allowed := tc.reads
						if op.admin {
							allowed = tc.admin
						}
						want := http.StatusForbidden
						if allowed {
							want = op.served
						}

						rec := a.call(t, op.method, op.target, tc.token, op.body)

						if got := rec.Code; got != want {
							t.Errorf("status = %d, want %d (body %q)", got, want, rec.Body)
						}
						if want == http.StatusForbidden {
							assertProblem(t, rec, http.StatusForbidden, problem.TypeForbidden)
						}
					})
				}
			})
		}

		// The creations the matrix refused wrote nothing, so the pair holds the
		// relation the setup created and the one the admin case created.
		if got := storedRelationIDs(t, a, pair[0].Id, pair[1].Id); len(got) != 2 {
			t.Errorf("stored relations = %v, want the seeded one beside the admin case's", got)
		}
	})
}

// TestProjectRelationsReportAFailedQuery drives the failure paths of the five
// relation operations. With authentication disabled nothing reads the database
// before the handler does, so a database that is gone stops the request inside
// the handler rather than at the credential lookup in front of it.
func TestProjectRelationsReportAFailedQuery(t *testing.T) {
	db := storetest.NewDB(t)
	a := newAPIInMode(t, db.Store, auth.ModeDisabled)

	// The bodies and the paths are built while the database is still there, so
	// nothing but the request itself runs after it is gone.
	project, relation := uuid.New(), uuid.New()
	creation := relationDocument(t, uuid.New(), plainType)
	update := projectDocument(t, map[string]any{"metadata": map[string]any{"owner": "team-a"}})

	// How long the database gets to shut down cleanly.
	stopTimeout := 10 * time.Second
	if err := db.Container.Stop(t.Context(), &stopTimeout); err != nil {
		t.Fatalf("stopping the database container: %v", err)
	}

	for _, tc := range []struct {
		name   string
		method string
		target string
		body   []byte
		detail string
	}{
		{
			// A project that cannot be read is not a project that is not there,
			// so the answer is a 500 rather than the 404 of an unknown id.
			name: "the relation list", method: http.MethodGet,
			target: relationsPath(project), detail: "the relations could not be read",
		},
		{
			name: "the traversal", method: http.MethodGet,
			target: relatedPath(project), detail: "the related projects could not be read",
		},
		{
			name: "the creation", method: http.MethodPost, target: relationsPath(project),
			body: creation, detail: "the relation could not be created",
		},
		{
			name: "the update", method: http.MethodPatch,
			target: relationTarget(project, relation), body: update,
			detail: "the relation could not be updated",
		},
		{
			name: "the close", method: http.MethodDelete,
			target: relationTarget(project, relation), detail: "the relation could not be closed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := a.call(t, tc.method, tc.target, "", tc.body)

			assertProblem(t, rec, http.StatusInternalServerError, problem.TypeInternal)
			if got := problemDetail(t, rec); got != tc.detail {
				t.Errorf("detail = %q, want %q", got, tc.detail)
			}
		})
	}
}

// relationOperation is one of the five relation operations as the role matrix
// exercises it: which status a call that gets through answers, and whether the
// route takes the admin role rather than read_all.
type relationOperation struct {
	name   string
	method string
	target string
	body   []byte
	admin  bool
	served int
}

// relationOperations builds the five operations against one related pair. The
// creation asks for relationType, so two cases of the matrix never ask for the
// same triple.
func relationOperations(t *testing.T, source, target Project, existing Relation,
	relationType string,
) []relationOperation {
	t.Helper()

	return []relationOperation{
		{
			name: "the relation list", method: http.MethodGet,
			target: relationsPath(source.Id), served: http.StatusOK,
		},
		{
			name: "the traversal", method: http.MethodGet,
			target: relatedPath(source.Id), served: http.StatusOK,
		},
		{
			name: "the creation", method: http.MethodPost, target: relationsPath(source.Id),
			admin: true, served: http.StatusCreated,
			body: relationDocument(t, target.Id, relationType),
		},
		{
			name: "the update", method: http.MethodPatch,
			target: relationTarget(source.Id, existing.Id), admin: true, served: http.StatusOK,
			body: projectDocument(t, map[string]any{"metadata": map[string]any{"by": relationType}}),
		},
		{
			name: "the close", method: http.MethodDelete,
			target: relationTarget(source.Id, existing.Id), admin: true,
			served: http.StatusNoContent,
		},
	}
}

// relationsPath is the route the relations of one project are listed and
// created under.
func relationsPath(id uuid.UUID) string {
	return projectPath(id) + "/relations"
}

// relationTarget is the route one relation is updated and closed under.
func relationTarget(project, relation uuid.UUID) string {
	return relationsPath(project) + "/" + relation.String()
}

// relatedPath is the route the traversal out of one project answers under.
func relatedPath(id uuid.UUID) string {
	return projectPath(id) + "/related"
}

// registerProjects registers count projects under a cloud of the subtest's own,
// named p1 to pN, so that a walk of one subtest never reaches the graph of
// another.
func registerProjects(t *testing.T, a api, token, cloud string, count int) []Project {
	t.Helper()

	registered := make([]Project, count)
	for i := range registered {
		registered[i] = registerProject(t, a, token, fixturePlatform, cloud, fmt.Sprintf("p%d", i+1))
	}
	return registered
}

// relate creates one relation over POST and returns it. The setup goes through
// the API so that the rows the reads walk are the ones the write path wrote.
func relate(t *testing.T, a api, token string, source, target uuid.UUID, relationType string) Relation {
	t.Helper()

	return createdRelation(t, a.call(t, http.MethodPost, relationsPath(source), token,
		relationDocument(t, target, relationType)))
}

// closeRelation closes one relation over DELETE, which the contract promises is
// a 204 carrying nothing.
func closeRelation(t *testing.T, a api, token string, source, relation uuid.UUID) {
	t.Helper()

	rec := a.call(t, http.MethodDelete, relationTarget(source, relation), token, nil)
	if got := rec.Code; got != http.StatusNoContent {
		t.Fatalf("status = %d, want %d (body %q)", got, http.StatusNoContent, rec.Body)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want a 204 to carry none", rec.Body)
	}
}

// relationDocument renders the body a creation carries.
func relationDocument(t *testing.T, target uuid.UUID, relationType string) []byte {
	t.Helper()

	return projectDocument(t, map[string]any{
		"target_id": target, "relation_type": relationType,
	})
}

// createdRelation decodes the answer of a creation, which the contract promises
// is a 201 carrying the relation.
func createdRelation(t *testing.T, rec *httptest.ResponseRecorder) Relation {
	t.Helper()

	if got := rec.Code; got != http.StatusCreated {
		t.Fatalf("status = %d, want %d (body %q)", got, http.StatusCreated, rec.Body)
	}
	return decodeRelation(t, rec)
}

// relationFrom decodes the answer of an update, which the contract promises is
// a 200 carrying the relation as it now stands.
func relationFrom(t *testing.T, rec *httptest.ResponseRecorder) Relation {
	t.Helper()

	if got := rec.Code; got != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %q)", got, http.StatusOK, rec.Body)
	}
	return decodeRelation(t, rec)
}

// decodeRelation reads one relation out of an answer whose status a caller has
// already checked.
func decodeRelation(t *testing.T, rec *httptest.ResponseRecorder) Relation {
	t.Helper()

	if got, want := rec.Header().Get("Content-Type"), "application/json"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}

	var got Relation
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding the body %q: %v", rec.Body.String(), err)
	}
	return got
}

// relationListOf decodes the answer of a relation list, which the contract
// promises is a 200 carrying the relations valid at one instant.
func relationListOf(t *testing.T, rec *httptest.ResponseRecorder) RelationList {
	t.Helper()

	if got := rec.Code; got != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %q)", got, http.StatusOK, rec.Body)
	}

	var list RelationList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decoding the body %q: %v", rec.Body.String(), err)
	}
	return list
}

// relatedListOf decodes the answer of a traversal, which the contract promises
// is a 200 carrying the projects the walk reached.
func relatedListOf(t *testing.T, rec *httptest.ResponseRecorder) RelatedProjectList {
	t.Helper()

	if got := rec.Code; got != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %q)", got, http.StatusOK, rec.Body)
	}

	var list RelatedProjectList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decoding the body %q: %v", rec.Body.String(), err)
	}
	return list
}

// namesOf indexes projects by id under the external id they are registered
// with, so that a failure reads as p1 rather than as a UUID.
func namesOf(registered ...Project) map[uuid.UUID]string {
	named := make(map[uuid.UUID]string, len(registered))
	for _, project := range registered {
		named[project.Id] = project.ExternalId
	}
	return named
}

// relationEdges names the relations of an answer as source to target, in the
// order they were served.
func relationEdges(items []Relation, named map[uuid.UUID]string) []string {
	edges := make([]string, len(items))
	for i, item := range items {
		edges[i] = named[item.SourceId] + "->" + named[item.TargetId]
	}
	return edges
}

// relationIDs names the relations of an answer in the order they were served.
func relationIDs(items []Relation) []uuid.UUID {
	ids := make([]uuid.UUID, len(items))
	for i, item := range items {
		ids[i] = item.Id
	}
	return ids
}

// walkSteps names what a traversal answered: the project it reached, the depth
// it sits at, and how many relations the walk took to get there.
func walkSteps(items []RelatedProject) []string {
	steps := make([]string, len(items))
	for i, item := range items {
		steps[i] = fmt.Sprintf("%s@%d/%d", item.Project.ExternalId, item.Depth, len(item.Path))
	}
	return steps
}

// memberRaw is one member of an answer as the answer spells it, which is what
// tells a member that is null from one the answer does not carry at all.
func memberRaw(t *testing.T, rec *httptest.ResponseRecorder, name string) string {
	t.Helper()

	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the body %q: %v", rec.Body.String(), err)
	}
	raw, held := body[name]
	if !held {
		t.Fatalf("the body %q carries no %s", rec.Body.String(), name)
	}
	return string(raw)
}

// storedValidTo is the end the relation row carries, nil while it is open.
func storedValidTo(t *testing.T, a api, relation uuid.UUID) *time.Time {
	t.Helper()

	var validTo *time.Time
	if err := a.store.Pool().QueryRow(t.Context(),
		`SELECT valid_to FROM project_relations WHERE id = $1`, relation).Scan(&validTo); err != nil {
		t.Fatalf("reading the stored valid_to of %s: %v", relation, err)
	}
	return validTo
}

// storedRelationIDs names every relation row between two projects, oldest
// first, whatever its type and whether it is still open. It is what says a
// refused write stored nothing, which no read of this API can answer.
func storedRelationIDs(t *testing.T, a api, source, target uuid.UUID) []uuid.UUID {
	t.Helper()

	rows, err := a.store.Pool().Query(t.Context(),
		`SELECT id FROM project_relations
		 WHERE source_id = $1 AND target_id = $2 ORDER BY created_at, id`,
		source, target)
	if err != nil {
		t.Fatalf("reading the relations from %s to %s: %v", source, target, err)
	}
	defer rows.Close()

	var found []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scanning a relation row: %v", err)
		}
		found = append(found, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the relations from %s to %s: %v", source, target, err)
	}
	return found
}
