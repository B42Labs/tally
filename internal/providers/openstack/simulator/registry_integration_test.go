// This file holds the registrar against the Reporting API itself: the wire
// documents registry.go mirrors are posted to the contract's request validator
// and through the role guard every route carries, into the registry tables of a
// real database. Neither is something the httptest stand-in of registry_test.go
// can state. That server answers whatever a case tells it to, so a document the
// contract refuses and a role a route would not grant pass it alike.
package simulator

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/b42labs/tally/internal/reporting/auth"
	"github.com/b42labs/tally/internal/reporting/httpapi"
	"github.com/b42labs/tally/internal/reporting/ingest"
	"github.com/b42labs/tally/internal/reporting/reconciliation"
	"github.com/b42labs/tally/internal/reporting/registry"
	"github.com/b42labs/tally/internal/reporting/store/sqlcgen"
	"github.com/b42labs/tally/internal/reporting/store/storetest"
)

// testRouterInternalToken guards the /internal routes of the API this test
// starts. A registration calls none of them, and the router takes the secret
// either way.
const testRouterInternalToken = "internal-token-of-the-registry-integration-test"

// The clouds the refused registration is read under. They differ from the pair
// in registrations_test.go because both cases run against one registry: rows
// counted under the clouds the first case registered would be its rows rather
// than none.
const (
	readOnlyTenantsCloud = "os-readonly"
	readOnlyGardenCloud  = "garden-readonly"
)

// The clouds the second month is read under, for the same reason: the case that
// registers two months has to count its own relations and nobody else's.
const (
	secondMonthTenantsCloud = "os-second-month"
	secondMonthGardenCloud  = "garden-second-month"
)

// The clouds the month behind a closed relation is registered under, for the
// same reason again.
const (
	closedTenantsCloud = "os-closed-relation"
	closedGardenCloud  = "garden-closed-relation"
)

// TestRegisterAgainstTheReportingAPI registers a generated month with the real
// project registry: the router cmd/tally-reporting builds, over a migrated
// TimescaleDB in a container, with authentication enforced.
//
// What it covers is the seam between the two. Whether the documents this
// package mirrors are the documents the contract takes, whether the rows and
// relations they create are the ones a tenant's cost is attributed by, and
// whether a run started with a token that may read the registry stops before it
// writes into it.
func TestRegisterAgainstTheReportingAPI(t *testing.T) {
	db := storetest.NewDB(t)
	queries := sqlcgen.New(db.Store.Pool())
	server := startReportingAPI(t, db, queries)
	adminToken := seedAPIToken(t, queries, auth.RoleAdmin)
	readAllToken := seedAPIToken(t, queries, auth.RoleReadAll)

	t.Run("a registry that holds nothing, then the same month again", func(t *testing.T) {
		month := namedMonth(t, 1, july2026, tenantsCloud)
		registrations, err := RegistrationsOf(month, gardenCloud)
		if err != nil {
			t.Fatalf("RegistrationsOf(month, %q) error = %v, want nil", gardenCloud, err)
		}

		report, err := NewRegistrar(server.URL, adminToken, testLogger(t)).
			Register(t.Context(), registrations)
		if err != nil {
			t.Fatalf("Register() error = %v, want nil", err)
		}
		want := RegistrationReport{ProjectsCreated: 8, RelationsCreated: 2}
		if report != want {
			t.Errorf("Register() = %+v, want %+v: the six tenants and the two Gardener projects are "+
				"registered, and each Gardener project is related to the tenant its shoots run on",
				report, want)
		}

		// Rerunning the same seed, period and cloud is what an operator does after a
		// run that failed halfway, so the second registration has to find every row
		// and every relation in place rather than end on one of them.
		again, err := NewRegistrar(server.URL, adminToken, testLogger(t)).
			Register(t.Context(), registrations)
		if err != nil {
			t.Fatalf("the second Register() error = %v, want nil: a row the registry holds is not a "+
				"failed registration", err)
		}
		wantAgain := RegistrationReport{ProjectsExisting: 8, RelationsExisting: 2}
		if again != wantAgain {
			t.Errorf("the second Register() = %+v, want %+v: the month is registered, so the second "+
				"run creates nothing", again, wantAgain)
		}

		// What the two runs left in the registry. A row written twice would be
		// counted here, and a platform the API stored otherwise would put a Gardener
		// project into the OpenStack installation.
		wantRows := map[projectGroup]int{
			{cloud: tenantsCloud, platform: "openstack"}: 6,
			{cloud: gardenCloud, platform: "gardener"}:   2,
		}
		if got := projectCounts(t, db, tenantsCloud, gardenCloud); !maps.Equal(got, wantRows) {
			t.Errorf("the registry holds %v, want %v: a tenant is a row of the installation it ran "+
				"in, a Gardener project a row of the garden", got, wantRows)
		}

		// Nothing else in this database writes a relation, so the whole table is what
		// the registrations put there.
		stored := storedRelations(t, db)
		if len(stored) != 2 {
			t.Fatalf("the registry holds %d relations, want 2: one per Gardener project, and the "+
				"second run reuses the ones the first created", len(stored))
		}
		for i, relation := range stored {
			if relation.relationType != relationInfrastructureTenant {
				t.Errorf("relation %d is of type %q, want %q: this is the type the cost of a tenant "+
					"is attributed by", i, relation.relationType, relationInfrastructureTenant)
			}
			if !relation.validFrom.Equal(july2026) {
				t.Errorf("relation %d is valid from %s, want %s: the attribution covers the whole "+
					"month the usage falls in", i, relation.validFrom.Format(time.RFC3339),
					july2026.Format(time.RFC3339))
			}
			if relation.validTo != nil {
				t.Errorf("relation %d is closed at %s, want an open relation: a closed one attributes "+
					"nothing after its end", i, relation.validTo.Format(time.RFC3339))
			}
		}

		// The registry answers the walk the attribution is read by, so alpha reaching
		// its tenant over infrastructure_tenant is what the registration was for. It
		// is read over the route rather than out of the table, because the route is
		// what a consumer of the registry sees.
		var alpha uuid.UUID
		if err := db.Store.Pool().QueryRow(t.Context(),
			`SELECT id FROM projects WHERE cloud = $1 AND external_id = $2`,
			gardenCloud, "alpha").Scan(&alpha); err != nil {
			t.Fatalf("reading the id of the Gardener project alpha: %v", err)
		}
		wantReached := []reachedProject{{
			cloud:        tenantsCloud,
			externalID:   month.GardenerProjects[0].TenantID,
			relationType: relationInfrastructureTenant,
		}}
		got := relatedProjects(t, server.URL, adminToken, alpha)
		if !slices.Equal(got, wantReached) {
			t.Errorf("alpha reaches %+v, want %+v: the shoots of alpha run on that tenant, and its "+
				"cost is what the relation attributes", got, wantReached)
		}
	})

	t.Run("a second month against the same garden cloud", func(t *testing.T) {
		// The Gardener rows are keyed by their names, so the second month finds
		// them in place; its tenants are keyed by identifiers the month is salted
		// into, so the relation it would create points somewhere else. The
		// registry keys an open relation by (source_id, target_id, relation_type),
		// which makes that a second open relation rather than a 409, and a
		// statement of alpha would then carry the cost of both months' tenants.
		first, err := RegistrationsOf(
			namedMonth(t, 1, july2026, secondMonthTenantsCloud), secondMonthGardenCloud)
		if err != nil {
			t.Fatalf("RegistrationsOf(month, %q) error = %v, want nil", secondMonthGardenCloud, err)
		}
		if _, err := NewRegistrar(server.URL, adminToken, testLogger(t)).
			Register(t.Context(), first); err != nil {
			t.Fatalf("Register() error = %v, want nil", err)
		}

		second, err := RegistrationsOf(
			namedMonth(t, 1, june2026, secondMonthTenantsCloud), secondMonthGardenCloud)
		if err != nil {
			t.Fatalf("RegistrationsOf(month, %q) error = %v, want nil", secondMonthGardenCloud, err)
		}
		report, err := NewRegistrar(server.URL, adminToken, testLogger(t)).
			Register(t.Context(), second)
		if err == nil {
			t.Fatalf("the second Register() error = nil, want the run refused: two open relations " +
				"out of one Gardener project attribute two tenants to it")
		}
		if want := "is already related to"; !strings.Contains(err.Error(), want) {
			t.Errorf("the second Register() error = %q, want it to contain %q", err, want)
		}
		// The earlier relation starts in July and this run registers June, so the
		// close the guard names elsewhere would set a valid_to before that start,
		// which the API answers 422 for. What is left is a garden cloud of its own.
		if want := "register this month under another garden cloud"; !strings.Contains(err.Error(), want) {
			t.Errorf("the second Register() error = %q, want it to contain %q: the earlier relation "+
				"starts after this month, so no close ends it before this month begins", err, want)
		}
		if unwanted := "PATCH "; strings.Contains(err.Error(), unwanted) {
			t.Errorf("the second Register() error = %q, want it to name no %q: the close it would "+
				"stand for is refused", err, unwanted)
		}
		assertCloseRefused(t, db, server.URL, adminToken, secondMonthGardenCloud, june2026)
		if report.RelationsCreated != 0 || report.RelationsExisting != 0 {
			t.Errorf("the second Register() = %+v, want no relation touched", report)
		}

		if got := relationsOutOf(t, db, secondMonthGardenCloud); got != 2 {
			t.Errorf("the two runs left %d relations out of %s, want 2: one per Gardener project, "+
				"and the second run created none beside them", got, secondMonthGardenCloud)
		}
	})

	t.Run("a second month behind a relation closed with DELETE", func(t *testing.T) {
		// DELETE closes a relation at now, and every simulated period is in the
		// past, so a relation closed that way still covers the month a second run
		// registers: the engine bills a period by the relations that overlap it, not
		// by the ones open when it runs. Registering beside such a relation is
		// therefore the same double attribution as registering beside an open one,
		// and the guard has to refuse it against the API's own point-in-time reads
		// rather than against the valid_to it reads off a row.
		first, err := RegistrationsOf(
			namedMonth(t, 1, july2026, closedTenantsCloud), closedGardenCloud)
		if err != nil {
			t.Fatalf("RegistrationsOf(month, %q) error = %v, want nil", closedGardenCloud, err)
		}
		if _, err := NewRegistrar(server.URL, adminToken, testLogger(t)).
			Register(t.Context(), first); err != nil {
			t.Fatalf("Register() error = %v, want nil", err)
		}
		if closed := closeRelations(t, db, server.URL, adminToken, closedGardenCloud); closed != 2 {
			t.Fatalf("the case closed %d relations, want 2: one per Gardener project", closed)
		}

		// Another seed over the same period: the Gardener rows stay, and the tenants
		// the relations would point at are new ones.
		second, err := RegistrationsOf(
			namedMonth(t, 2, july2026, closedTenantsCloud), closedGardenCloud)
		if err != nil {
			t.Fatalf("RegistrationsOf(month, %q) error = %v, want nil", closedGardenCloud, err)
		}
		report, err := NewRegistrar(server.URL, adminToken, testLogger(t)).
			Register(t.Context(), second)
		if err == nil {
			t.Fatalf("the second Register() error = nil, want the run refused: a relation closed at " +
				"now still attributes the month this run registers")
		}
		// The earlier relation starts where this month starts, so there is no
		// valid_to that is both after its start and no later than the month it
		// attributes: PATCH answers 422 for one, and the message says so instead of
		// naming the route.
		if want := "register this month under another garden cloud"; !strings.Contains(err.Error(), want) {
			t.Errorf("the second Register() error = %q, want it to contain %q: the earlier relation "+
				"starts where this month does and cannot be ended before it", err, want)
		}
		if unwanted := "PATCH "; strings.Contains(err.Error(), unwanted) {
			t.Errorf("the second Register() error = %q, want it to name no %q: the close it would "+
				"stand for is refused", err, unwanted)
		}
		assertCloseRefused(t, db, server.URL, adminToken, closedGardenCloud, july2026)
		if report.RelationsCreated != 0 || report.RelationsExisting != 0 {
			t.Errorf("the second Register() = %+v, want no relation touched", report)
		}
		if got := relationsOutOf(t, db, closedGardenCloud); got != 2 {
			t.Errorf("the two runs left %d relations out of %s, want 2: the closed ones, and none "+
				"created beside them", got, closedGardenCloud)
		}
	})

	t.Run("a token without the admin role", func(t *testing.T) {
		// A month under clouds of its own, because the registry is the one the case
		// above wrote into: rows counted under its clouds would be its rows.
		month := namedMonth(t, 1, july2026, readOnlyTenantsCloud)
		registrations, err := RegistrationsOf(month, readOnlyGardenCloud)
		if err != nil {
			t.Fatalf("RegistrationsOf(month, %q) error = %v, want nil", readOnlyGardenCloud, err)
		}

		_, err = NewRegistrar(server.URL, readAllToken, testLogger(t)).
			Register(t.Context(), registrations)
		if err == nil {
			t.Fatalf("Register() error = nil, want the registration refused: reading the registry is " +
				"not writing into it")
		}
		const refusal = "the Reporting API answered 403 for POST /api/v1/projects"
		if !strings.Contains(err.Error(), refusal) {
			t.Errorf("Register() error = %q, want it to contain %q: the route answers the role it "+
				"demands", err, refusal)
		}
		const hint = "TALLY_SIM_API_TOKEN has to be an api token of role admin"
		if !strings.HasSuffix(err.Error(), hint) {
			t.Errorf("Register() error = %q, want it to end in %q: the fix for a refused token is not "+
				"in the answer", err, hint)
		}

		if got := projectCounts(t, db, readOnlyTenantsCloud, readOnlyGardenCloud); len(got) != 0 {
			t.Errorf("the registry holds %v under the two clouds, want nothing: the first project is "+
				"refused, so no row is written", got)
		}
	})
}

// startReportingAPI serves the Reporting API over db for the length of the test
// and returns the server it listens on. The options are the ones the API's own
// integration tests build the router with
// (internal/reporting/httpapi/events_integration_test.go): the request
// validator in front of every handler, authentication enforced so each route
// asks for the role its guard names, and infrastructure_tenant as the
// attributing type, which is what the registrar creates.
func startReportingAPI(t *testing.T, db storetest.DB, q *sqlcgen.Queries) *httptest.Server {
	t.Helper()

	handler, err := httpapi.NewRouter(httpapi.Options{
		Logger:                   slog.New(slog.NewJSONHandler(io.Discard, nil)),
		DB:                       db.Store,
		UnhealthyThreshold:       time.Minute,
		Queries:                  q,
		Store:                    db.Store,
		AuthMode:                 auth.ModeEnforced,
		InternalToken:            testRouterInternalToken,
		Authenticator:            auth.NewStaticTokenAuthenticator(q),
		Pipeline:                 ingest.New(registry.New(), false, nil, nil),
		AttributingRelationTypes: []string{relationInfrastructureTenant},
		Syncer: reconciliation.New(db.Store, ingest.New(registry.New(), false, nil, nil),
			reconciliation.Config{}, map[string]reconciliation.Adapter{}, time.Now, nil),
	})
	if err != nil {
		t.Fatalf("NewRouter() error = %v, want nil", err)
	}

	// Plain HTTP: what a simulated run needs from the API is its rules, and a
	// server whose certificate the registrar had to trust would add nothing to
	// them.
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

// seedAPIToken issues an API token of the given role and stores its hash, the
// way the API's own integration tests seed one
// (internal/reporting/httpapi/resourcetypes_integration_test.go).
func seedAPIToken(t *testing.T, q *sqlcgen.Queries, role auth.Role) string {
	t.Helper()

	token, err := auth.GenerateAPIToken()
	if err != nil {
		t.Fatalf("generating the %s token: %v", role, err)
	}
	// project_ids is NOT NULL, so a token that names no project carries an empty
	// array rather than a nil slice, which pgx would send as NULL.
	if _, err := q.CreateAPIToken(t.Context(), sqlcgen.CreateAPITokenParams{
		TokenHash:   auth.HashToken(token),
		Role:        string(role),
		ProjectIds:  []uuid.UUID{},
		Description: pgtype.Text{String: "simulator registry integration test", Valid: true},
	}); err != nil {
		t.Fatalf("CreateAPIToken() error = %v, want nil", err)
	}
	return token
}

// projectGroup is what the registered rows are counted by: the installation a
// row belongs to, and the platform that installation runs.
type projectGroup struct{ cloud, platform string }

// projectCounts reads how many projects the registry holds under each of the
// clouds, by platform. A cloud without a row is absent from the result rather
// than counted as zero.
func projectCounts(t *testing.T, db storetest.DB, clouds ...string) map[projectGroup]int {
	t.Helper()

	rows, err := db.Store.Pool().Query(t.Context(),
		`SELECT cloud, platform, count(*) FROM projects WHERE cloud = ANY($1) GROUP BY cloud, platform`,
		clouds)
	if err != nil {
		t.Fatalf("querying the registered projects: %v", err)
	}
	defer rows.Close()

	counted := map[projectGroup]int{}
	for rows.Next() {
		var group projectGroup
		var count int
		if err := rows.Scan(&group.cloud, &group.platform, &count); err != nil {
			t.Fatalf("scanning a count of registered projects: %v", err)
		}
		counted[group] = count
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the registered projects: %v", err)
	}
	return counted
}

// storedRelation is one row of project_relations, as much of it as this test
// reads. A nil validTo is an active relation.
type storedRelation struct {
	relationType string
	validFrom    time.Time
	validTo      *time.Time
}

// storedRelations reads every relation the registry holds.
func storedRelations(t *testing.T, db storetest.DB) []storedRelation {
	t.Helper()

	rows, err := db.Store.Pool().Query(t.Context(),
		`SELECT relation_type, valid_from, valid_to FROM project_relations`)
	if err != nil {
		t.Fatalf("querying the stored relations: %v", err)
	}
	defer rows.Close()

	var stored []storedRelation
	for rows.Next() {
		var relation storedRelation
		if err := rows.Scan(&relation.relationType, &relation.validFrom, &relation.validTo); err != nil {
			t.Fatalf("scanning a stored relation: %v", err)
		}
		stored = append(stored, relation)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the stored relations: %v", err)
	}
	return stored
}

// relationsOutOf counts the relations leaving the projects of one cloud, which
// is how many attributions a run left behind there.
func relationsOutOf(t *testing.T, db storetest.DB, cloud string) int {
	t.Helper()

	var count int
	if err := db.Store.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM project_relations r JOIN projects p ON p.id = r.source_id
		 WHERE p.cloud = $1`, cloud).Scan(&count); err != nil {
		t.Fatalf("counting the relations out of %s: %v", cloud, err)
	}
	return count
}

// closeRelations closes every open relation leaving the projects of one cloud
// over DELETE /api/v1/projects/{id}/relations/{relation_id}, and answers how
// many it closed. That route is what an operator reaches for to make room for
// another month, so a case about what such a registry answers has to leave the
// relations behind the way the route does.
func closeRelations(t *testing.T, db storetest.DB, serverURL, token, cloud string) int {
	t.Helper()

	rows, err := db.Store.Pool().Query(t.Context(),
		`SELECT r.id, r.source_id FROM project_relations r JOIN projects p ON p.id = r.source_id
		 WHERE p.cloud = $1 AND r.valid_to IS NULL`, cloud)
	if err != nil {
		t.Fatalf("querying the open relations out of %s: %v", cloud, err)
	}
	defer rows.Close()

	var routes []string
	for rows.Next() {
		var relation, source uuid.UUID
		if err := rows.Scan(&relation, &source); err != nil {
			t.Fatalf("scanning an open relation out of %s: %v", cloud, err)
		}
		routes = append(routes, projectsRoute+"/"+source.String()+"/relations/"+relation.String())
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the open relations out of %s: %v", cloud, err)
	}

	for _, route := range routes {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodDelete, serverURL+route, nil)
		if err != nil {
			t.Fatalf("building the request that closes %s: %v", route, err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("closing %s: %v", route, err)
		}
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatalf("reading the answer of DELETE %s: %v", route, err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			t.Fatalf("DELETE %s is answered %d %s, want 204", route, response.StatusCode, body)
		}
	}
	return len(routes)
}

// assertCloseRefused ends the first relation leaving the projects of one cloud
// at instant, over PATCH /api/v1/projects/{id}/relations/{relation_id}, and
// demands the 422 the API answers a valid_to that is not after the stored
// valid_from with. That close is what the guard names when the earlier relation
// began in an earlier month, so a case about a refusal that names none has to
// state why: it would send an operator into this answer.
func assertCloseRefused(t *testing.T, db storetest.DB, serverURL, token, cloud string,
	instant time.Time,
) {
	t.Helper()

	var relation, source uuid.UUID
	if err := db.Store.Pool().QueryRow(t.Context(),
		`SELECT r.id, r.source_id FROM project_relations r JOIN projects p ON p.id = r.source_id
		 WHERE p.cloud = $1 ORDER BY r.id LIMIT 1`, cloud).Scan(&relation, &source); err != nil {
		t.Fatalf("reading a relation out of %s: %v", cloud, err)
	}

	route := projectsRoute + "/" + source.String() + "/relations/" + relation.String()
	end := instant.UTC().Format(time.RFC3339)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPatch, serverURL+route,
		strings.NewReader(fmt.Sprintf(`{"valid_to":%q}`, end)))
	if err != nil {
		t.Fatalf("building the request that ends %s: %v", route, err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("ending %s at %s: %v", route, end, err)
	}
	defer func() { _ = response.Body.Close() }()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading the answer of PATCH %s: %v", route, err)
	}
	if response.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("PATCH %s with valid_to %s is answered %d %s, want 422: the earlier relation does "+
			"not start before that instant, so ending it there is no way out of this registry",
			route, end, response.StatusCode, answer)
	}
}

// reachedProject is one project a walk over the relations arrived at, flattened
// out of the RelatedProject of api/reporting/openapi.yaml.
type reachedProject struct{ cloud, externalID, relationType string }

// relatedProjects walks the infrastructure_tenant relations out of project over
// GET /api/v1/projects/{id}/related and returns what the walk reached, in the
// order the answer lists it.
func relatedProjects(t *testing.T, serverURL, token string, project uuid.UUID) []reachedProject {
	t.Helper()

	target := serverURL + "/api/v1/projects/" + project.String() +
		"/related?relation_type=" + relationInfrastructureTenant
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("building the request for the related projects: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("reading the related projects: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading the answer of the related projects: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("the related projects of %s are answered %d %s, want 200",
			project, response.StatusCode, body)
	}

	// The answer is the RelatedProjectList of api/reporting/openapi.yaml: an item
	// per project the walk reached, carrying the registry row and the type of the
	// relation the walk arrived on.
	var answer struct {
		Items []struct {
			Project struct {
				Cloud      string `json:"cloud"`
				ExternalID string `json:"external_id"`
			} `json:"project"`
			RelationType string `json:"relation_type"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("decoding the related projects: %v", err)
	}

	reached := make([]reachedProject, 0, len(answer.Items))
	for _, item := range answer.Items {
		reached = append(reached, reachedProject{
			cloud:        item.Project.Cloud,
			externalID:   item.Project.ExternalID,
			relationType: item.RelationType,
		})
	}
	return reached
}
