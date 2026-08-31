package simulator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// testAPIToken is the credential the registrar is built with. It is a literal
// the cases search the errors and the log for, because a message that carried
// the token would put it into whatever an operator pastes it into.
const testAPIToken = "tly_a_secret-of-the-test"

// conflictProblem is what the Reporting API answers a key it already holds, and
// a relation triple that is already active.
const conflictProblem = `{"type":"urn:tally:error:conflict","title":"already registered","status":409}`

// recordedRequest is one request the stand-in registry was sent, as much of it
// as the cases read.
type recordedRequest struct {
	method        string
	path          string
	rawQuery      string
	authorization string
	contentType   string
	body          map[string]any
}

// registryAnswer answers one request and reports whether it did. A case states
// the answers it needs and leaves the rest to the registry that holds nothing.
type registryAnswer func(w http.ResponseWriter, r *http.Request, body map[string]any) bool

// registryServer stands in for the project registry of the Reporting API. It
// records every request it was sent and answers by the case's function, so a
// case states what the API answers and holds the requests against it afterwards.
type registryServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []recordedRequest
	// ids is the id every registered key was created under. A relation names its
	// ends by id, so this is what lets a case tell the id of alpha's tenant from
	// the id of any other row.
	ids map[ProjectKey]uuid.UUID
}

func newRegistryServer(t *testing.T, answer registryAnswer) *registryServer {
	t.Helper()

	server := &registryServer{ids: make(map[ProjectKey]uuid.UUID)}
	server.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading the body of %s %s: %v", r.Method, r.URL.Path, err)
		}
		var body map[string]any
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("%s %s carries the body %q, want a JSON document: %v",
					r.Method, r.URL.Path, raw, err)
			}
		}
		server.mu.Lock()
		server.requests = append(server.requests, recordedRequest{
			method:        r.Method,
			path:          r.URL.Path,
			rawQuery:      r.URL.RawQuery,
			authorization: r.Header.Get("Authorization"),
			contentType:   r.Header.Get("Content-Type"),
			body:          body,
		})
		server.mu.Unlock()

		if answer != nil && answer(w, r, body) {
			return
		}
		server.answerAsEmptyRegistry(w, r, body)
	}))
	t.Cleanup(server.Close)
	return server
}

// answerAsEmptyRegistry is what a registry holding none of the rows answers: a
// project is created under a fresh id, kept by its key, a relation is created,
// and a lookup serves the row its key names.
func (s *registryServer) answerAsEmptyRegistry(w http.ResponseWriter, r *http.Request, body map[string]any) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == projectsRoute:
		cloud, _ := body["cloud"].(string)
		externalID, _ := body["external_id"].(string)
		id := uuid.New()
		s.mu.Lock()
		s.ids[ProjectKey{Cloud: cloud, ExternalID: externalID}] = id
		s.mu.Unlock()
		writeAnswer(w, http.StatusCreated, fmt.Sprintf(`{"id":%q}`, id))
	case r.Method == http.MethodGet && r.URL.Path == projectsRoute:
		query := r.URL.Query()
		id := s.idOf(ProjectKey{Cloud: query.Get("cloud"), ExternalID: query.Get("external_id")})
		writeAnswer(w, http.StatusOK, fmt.Sprintf(`{"items":[{"id":%q}],"next_cursor":null}`, id))
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/relations"):
		// A registry holding no relation of the source, which is what the check in
		// front of every creation reads.
		writeAnswer(w, http.StatusOK, `{"items":[]}`)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/relations"):
		writeAnswer(w, http.StatusCreated, fmt.Sprintf(`{"id":%q}`, uuid.New()))
	default:
		writeAnswer(w, http.StatusNotFound,
			fmt.Sprintf(`{"type":"urn:tally:error:not_found","title":%q}`, r.Method+" "+r.URL.Path))
	}
}

// received is what the server was sent, in order.
func (s *registryServer) received() []recordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]recordedRequest(nil), s.requests...)
}

// idOf is the id the server answered the registration of key with.
func (s *registryServer) idOf(key ProjectKey) uuid.UUID {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.ids[key]
}

func writeAnswer(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// answersEverythingWith replies to every request the same way, which is what a
// destination that refuses the first project does to the whole registration.
func answersEverythingWith(status int, body string) registryAnswer {
	return func(w http.ResponseWriter, _ *http.Request, _ map[string]any) bool {
		writeAnswer(w, status, body)
		return true
	}
}

// holdsEveryProject refuses every project as already registered and serves list
// as the lookup that follows.
func holdsEveryProject(list string) registryAnswer {
	return func(w http.ResponseWriter, r *http.Request, _ map[string]any) bool {
		if r.Method == http.MethodGet && r.URL.Path == projectsRoute {
			writeAnswer(w, http.StatusOK, list)
			return true
		}
		writeAnswer(w, http.StatusConflict, conflictProblem)
		return true
	}
}

// heldRelation is a relation the stand-in registry holds, as much of it as the
// filter below needs. An empty validTo is a relation that was never closed.
type heldRelation struct {
	id, target         uuid.UUID
	validFrom, validTo string
}

// holdsRelations answers every read of a project's relations with the ones of
// stored that were valid at the instant the read names, which is now when it
// names none. That filter is the registry's own
// (valid_from <= at AND (valid_to IS NULL OR valid_to > at)), and the guard in
// front of a creation depends on it: a stand-in that served every relation it
// holds would let a case pass that says nothing about what the API answers.
//
// The bounds are parsed here rather than in the handler, because the handler
// runs on the server's goroutine, where a t.Fatalf would stop that goroutine
// instead of the case.
func holdsRelations(t *testing.T, stored ...heldRelation) registryAnswer {
	t.Helper()

	type bounded struct {
		item               string
		validFrom, validTo time.Time
	}
	relations := make([]bounded, len(stored))
	for i, relation := range stored {
		relations[i] = bounded{
			item: fmt.Sprintf(`{"id":%q,"target_id":%q,"valid_from":%q}`,
				relation.id, relation.target, relation.validFrom),
			validFrom: relationInstant(t, relation.validFrom),
		}
		if relation.validTo != "" {
			relations[i].validTo = relationInstant(t, relation.validTo)
		}
	}

	return func(w http.ResponseWriter, r *http.Request, _ map[string]any) bool {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/relations") {
			return false
		}
		at := time.Now()
		if raw := r.URL.Query().Get("at"); raw != "" {
			parsed, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				t.Errorf("the read asks for the relations at %q, want an RFC 3339 instant: %v", raw, err)
			}
			at = parsed
		}
		items := make([]string, 0, len(relations))
		for _, relation := range relations {
			if !relation.validFrom.After(at) && (relation.validTo.IsZero() || relation.validTo.After(at)) {
				items = append(items, relation.item)
			}
		}
		writeAnswer(w, http.StatusOK, fmt.Sprintf(`{"items":[%s]}`, strings.Join(items, ",")))
		return true
	}
}

// relationInstant parses one of the instants a held relation is bounded by.
func relationInstant(t *testing.T, value string) time.Time {
	t.Helper()

	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parsing the instant %q of a held relation: %v", value, err)
	}
	return parsed
}

// monthRegistrations is what the seeded month registers: a row per tenant, a row
// per Gardener project, and the relation between each pair of them.
func monthRegistrations(t *testing.T, month Month) Registrations {
	t.Helper()

	registrations, err := RegistrationsOf(month, gardenCloud)
	if err != nil {
		t.Fatalf("RegistrationsOf(month, %q) error = %v, want nil", gardenCloud, err)
	}
	return registrations
}

// TestRegistrarRegistersEveryProjectBeforeTheFirstRelation covers a run against
// a registry that holds nothing yet. The order is what makes the registration
// work at all: a relation names its two ends by the ids the registry answered
// the rows with, so a relation posted before its ends would be refused for a
// target the registry does not hold.
func TestRegistrarRegistersEveryProjectBeforeTheFirstRelation(t *testing.T) {
	month := namedMonth(t, 1, july2026, tenantsCloud)
	registrations := monthRegistrations(t, month)
	server := newRegistryServer(t, nil)

	report, err := NewRegistrar(server.URL, testAPIToken, testLogger(t)).
		Register(t.Context(), registrations)
	if err != nil {
		t.Fatalf("Register() error = %v, want nil", err)
	}
	// Six tenants and two Gardener projects are eight rows, and the two Gardener
	// projects are two relations. A registry that held nothing created all of them.
	want := RegistrationReport{ProjectsCreated: 8, RelationsCreated: 2}
	if report != want {
		t.Errorf("Register() = %+v, want %+v: an empty registry creates every row and every relation",
			report, want)
	}

	requests := server.received()
	if len(requests) != 14 {
		t.Fatalf("the registration sent %d requests, want 14: one per project, and per relation the "+
			"two reads of what the source is already related to and the creation", len(requests))
	}
	for i, request := range requests {
		if want := "Bearer " + testAPIToken; request.authorization != want {
			t.Errorf("request %d (%s %s) carries the authorization %q, want %q: every route of the "+
				"Reporting API is authenticated", i, request.method, request.path,
				request.authorization, want)
		}
		if request.method == http.MethodPost && request.contentType != "application/json" {
			t.Errorf("request %d posts %s as %q, want application/json: the API refuses another type",
				i, request.path, request.contentType)
		}
	}
	for i, request := range requests[:8] {
		if request.method != http.MethodPost || request.path != projectsRoute {
			t.Errorf("request %d is %s %s, want POST %s: the eight rows go first",
				i, request.method, request.path, projectsRoute)
		}
	}

	alpha := month.GardenerProjects[0]
	alphaRelation := requests[10]
	wantRoute := projectsRoute + "/" + server.idOf(ProjectKey{Cloud: gardenCloud, ExternalID: alpha.Name}).String() +
		"/relations"
	// The two reads in front of the creation are what keep a second month from
	// being related beside the first: they ask for the relations of this type
	// leaving this project, one as they stood when the month starts and one as
	// they stand now. Neither instant alone is the set that would attribute
	// beside the relation this run creates.
	wantChecks := []string{
		"at=2026-07-01T00%3A00%3A00Z&direction=outgoing&relation_type=" + relationInfrastructureTenant,
		"direction=outgoing&relation_type=" + relationInfrastructureTenant,
	}
	for i, check := range requests[8:10] {
		if check.method != http.MethodGet || check.path != wantRoute {
			t.Errorf("request %d is %s %s, want GET %s: what the source is already related to is read "+
				"before the relation is created", 8+i, check.method, check.path, wantRoute)
		}
		if got := check.rawQuery; got != wantChecks[i] {
			t.Errorf("check %d asks %q, want %q", i, got, wantChecks[i])
		}
	}
	if alphaRelation.method != http.MethodPost || alphaRelation.path != wantRoute {
		t.Errorf("relation 0 is %s %s, want POST %s: a relation leaves the project the route names",
			alphaRelation.method, alphaRelation.path, wantRoute)
	}
	wantTarget := server.idOf(ProjectKey{Cloud: tenantsCloud, ExternalID: alpha.TenantID}).String()
	if got := alphaRelation.body["target_id"]; got != wantTarget {
		t.Errorf("relation 0 points at %v, want %s: the target is the id the tenant of %s was registered "+
			"under", got, wantTarget, alpha.Name)
	}
	// The relation is valid from the first instant of the month, so the usage of
	// the first day is attributed too.
	if got := alphaRelation.body["valid_from"]; got != "2026-07-01T00:00:00Z" {
		t.Errorf("relation 0 is valid from %v, want %q", got, "2026-07-01T00:00:00Z")
	}
	if got := requests[13]; got.method != http.MethodPost || !strings.HasSuffix(got.path, "/relations") {
		t.Errorf("request 13 is %s %s, want the second relation posted below a project",
			got.method, got.path)
	}

	first := requests[0].body
	wantMembers := map[string]any{
		"platform":    "openstack",
		"cloud":       tenantsCloud,
		"external_id": month.Tenants[0].ID,
		"name":        month.Tenants[0].Name,
	}
	for member, want := range wantMembers {
		if got := first[member]; got != want {
			t.Errorf("project 0 states %s = %v, want %v: the row is keyed by its cloud and external id",
				member, got, want)
		}
	}
	metadata, ok := first["metadata"].(map[string]any)
	if !ok || metadata["created_by"] != "tally-openstack-simulator" {
		t.Errorf("project 0 states the metadata %v, want created_by = %q: an operator reading the "+
			"registry sees what wrote the row", first["metadata"], "tally-openstack-simulator")
	}
}

// TestRegistrarLooksUpAProjectTheRegistryAlreadyHolds covers the rerun. A run
// that failed halfway left rows behind, and registering them again is answered
// 409 rather than with the id the relations need. The id is read back by the key
// the registry refused, so the second run ends in the registry the first one
// meant to leave behind instead of relating nothing to nothing.
func TestRegistrarLooksUpAProjectTheRegistryAlreadyHolds(t *testing.T) {
	month := namedMonth(t, 1, july2026, tenantsCloud)
	registrations := monthRegistrations(t, month)
	alpha := month.GardenerProjects[0]
	held := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	server := newRegistryServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) bool {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == projectsRoute &&
			body["external_id"] == alpha.TenantID:
			writeAnswer(w, http.StatusConflict, conflictProblem)
		case r.Method == http.MethodGet && r.URL.Path == projectsRoute:
			writeAnswer(w, http.StatusOK, fmt.Sprintf(`{"items":[{"id":%q}],"next_cursor":null}`, held))
		default:
			return false
		}
		return true
	})

	report, err := NewRegistrar(server.URL, testAPIToken, testLogger(t)).
		Register(t.Context(), registrations)
	if err != nil {
		t.Fatalf("Register() error = %v, want nil: a row the registry holds is not a failed registration",
			err)
	}
	want := RegistrationReport{ProjectsCreated: 7, ProjectsExisting: 1, RelationsCreated: 2}
	if report != want {
		t.Errorf("Register() = %+v, want %+v: the row the registry held is reported as found, not created",
			report, want)
	}

	var lookup recordedRequest
	var relation recordedRequest
	for _, request := range server.received() {
		switch {
		case request.method == http.MethodGet && request.path == projectsRoute:
			lookup = request
		case request.method == http.MethodPost && strings.HasSuffix(request.path, "/relations") &&
			relation.method == "":
			relation = request
		}
	}
	if got := relation.body["target_id"]; got != held.String() {
		t.Errorf("relation 0 points at %v, want %s: the target is the id the lookup read back",
			got, held)
	}
	query, err := url.ParseQuery(lookup.rawQuery)
	if err != nil {
		t.Fatalf("the lookup query %q is unreadable: %v", lookup.rawQuery, err)
	}
	if query.Get("cloud") != tenantsCloud || query.Get("external_id") != alpha.TenantID {
		t.Errorf("the lookup asks for %v, want cloud=%s and external_id=%s: the pair is the key the "+
			"registry refused the row under", query, tenantsCloud, alpha.TenantID)
	}
}

// TestRegistrarLeavesAnActiveRelationAsItStands covers the relation half of a
// rerun. One triple of source, target and type carries a single open relation,
// so the 409 says the attribution is already in place.
func TestRegistrarLeavesAnActiveRelationAsItStands(t *testing.T) {
	registrations := monthRegistrations(t, namedMonth(t, 1, july2026, tenantsCloud))
	// The count is guarded because the server answers each request in a goroutine
	// of its own, and only the first relation is the one that is already active.
	var (
		mu        sync.Mutex
		relations int
	)
	server := newRegistryServer(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) bool {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/relations") {
			return false
		}
		mu.Lock()
		relations++
		first := relations == 1
		mu.Unlock()
		if !first {
			return false
		}
		writeAnswer(w, http.StatusConflict, conflictProblem)
		return true
	})

	report, err := NewRegistrar(server.URL, testAPIToken, testLogger(t)).
		Register(t.Context(), registrations)
	if err != nil {
		t.Fatalf("Register() error = %v, want nil", err)
	}
	want := RegistrationReport{ProjectsCreated: 8, RelationsCreated: 1, RelationsExisting: 1}
	if report != want {
		t.Errorf("Register() = %+v, want %+v: the active relation is reported as found and the run "+
			"carries on with the next one", report, want)
	}
}

// TestRegistrarRefusesASecondMonthBesideAnOpenRelation covers the rerun under
// another period, seed or cloud. The Gardener row is keyed by its name and is
// the one an earlier run registered, while the tenant it points at is keyed by
// an identifier the month is salted into, so the relation of the second run
// names a target the first one never had. The registry keys an open relation by
// (source, target, type), so it would take that as a new relation and hold both
// open: the export then walks both and sums two months into one statement.
func TestRegistrarRefusesASecondMonthBesideAnOpenRelation(t *testing.T) {
	registrations := monthRegistrations(t, namedMonth(t, 1, july2026, tenantsCloud))
	// What the earlier run left behind: an open relation of the same type out of
	// the same Gardener project, pointing at the tenant of that other month.
	earlier := uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	earlierTenant := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	server := newRegistryServer(t, holdsRelations(t, heldRelation{
		id: earlier, target: earlierTenant, validFrom: "2026-06-01T00:00:00Z",
	}))

	report, err := NewRegistrar(server.URL, testAPIToken, testLogger(t)).
		Register(t.Context(), registrations)

	if err == nil {
		t.Fatalf("Register() error = nil, want the registration to end: a second open relation out of " +
			"one project attributes two tenants to it")
	}
	for _, want := range []string{
		"is already related to " + earlierTenant.String(),
		"by " + relationInfrastructureTenant,
		// Ending the earlier relation is what makes room for this one, and the
		// instant it has to end at is the start of this month: a relation closed
		// later than that still attributes the month the engine is about to bill,
		// so DELETE, which ends one at now, is no way out of this registry.
		"no later than 2026-07-01T00:00:00Z",
		"PATCH " + projectsRoute + "/",
		"/relations/" + earlier.String(),
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Register() error = %q, want it to contain %q: an operator ends the earlier "+
				"relation off this message", err, want)
		}
	}
	if want := (RegistrationReport{ProjectsCreated: 8}); report != want {
		t.Errorf("Register() = %+v, want %+v: the rows got through, the relation did not",
			report, want)
	}
	for _, request := range server.received() {
		if request.method == http.MethodPost && strings.HasSuffix(request.path, "/relations") {
			t.Errorf("the registration posted %s %s, want no relation created beside the open one",
				request.method, request.path)
		}
	}
}

// TestRegistrarRefusesAMonthBesideARelationThatOutlivesIt covers the two shapes
// a read at one instant alone would miss, and both of them end in the same
// registry: two relations of one Gardener project attributing at once, and a
// statement carrying the cost of two tenants.
//
// A relation closed with DELETE is closed at now, which is after every instant
// of a simulated month, so it goes on attributing the month this run registers:
// following the message of the guard is what would otherwise get a registration
// through that the guard exists to refuse. A relation that starts after this
// month attributes nothing of it and everything from its own start on, which is
// where the one this run creates would stand beside it.
func TestRegistrarRefusesAMonthBesideARelationThatOutlivesIt(t *testing.T) {
	registrations := monthRegistrations(t, namedMonth(t, 1, july2026, tenantsCloud))
	earlierTenant := uuid.MustParse("11111111-1111-1111-1111-111111111111")

	tests := []struct {
		name     string
		relation heldRelation
	}{
		{
			name: "a relation closed after the month it attributed",
			relation: heldRelation{
				validFrom: "2026-06-01T00:00:00Z",
				validTo:   time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
			},
		},
		{
			name:     "a relation that starts after this month",
			relation: heldRelation{validFrom: "2026-08-01T00:00:00Z"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			relation := tt.relation
			relation.id, relation.target = uuid.New(), earlierTenant
			server := newRegistryServer(t, holdsRelations(t, relation))

			_, err := NewRegistrar(server.URL, testAPIToken, testLogger(t)).
				Register(t.Context(), registrations)

			if err == nil {
				t.Fatalf("Register() error = nil, want the registration to end: the earlier relation " +
					"attributes beside the one this run creates")
			}
			if want := "is already related to " + earlierTenant.String(); !strings.Contains(err.Error(), want) {
				t.Errorf("Register() error = %q, want it to contain %q", err, want)
			}
			for _, request := range server.received() {
				if request.method == http.MethodPost && strings.HasSuffix(request.path, "/relations") {
					t.Errorf("the registration posted %s %s, want no relation created beside the "+
						"earlier one", request.method, request.path)
				}
			}
		})
	}
}

// TestRegistrarNamesNoCloseThatCannotBeMade covers the conflicts the close the
// message otherwise names cannot resolve. PATCH refuses a valid_to that is not
// after the stored valid_from with 422, so ending the earlier relation where
// this month begins is a way out only while that relation began earlier. A
// relation of the month being registered and one of a later month both start at
// or after the instant the close would name, so an operator who followed the
// route would read that refusal and stand where they started.
func TestRegistrarNamesNoCloseThatCannotBeMade(t *testing.T) {
	registrations := monthRegistrations(t, namedMonth(t, 1, july2026, tenantsCloud))
	earlierTenant := uuid.MustParse("11111111-1111-1111-1111-111111111111")

	tests := []struct {
		name     string
		relation heldRelation
	}{
		{
			name: "a relation of this month, closed with DELETE",
			relation: heldRelation{
				validFrom: "2026-07-01T00:00:00Z",
				validTo:   time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
			},
		},
		{
			name:     "a relation that starts after this month",
			relation: heldRelation{validFrom: "2026-08-01T00:00:00Z"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			relation := tt.relation
			relation.id, relation.target = uuid.New(), earlierTenant
			server := newRegistryServer(t, holdsRelations(t, relation))

			_, err := NewRegistrar(server.URL, testAPIToken, testLogger(t)).
				Register(t.Context(), registrations)

			if err == nil {
				t.Fatalf("Register() error = nil, want the registration to end: the earlier relation " +
					"attributes beside the one this run creates")
			}
			for _, want := range []string{
				"is already related to " + earlierTenant.String(),
				// The instant the earlier relation starts at is what says why no close
				// is named, so the message carries it.
				"from " + relation.validFrom + " on",
				"register this month under another garden cloud",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Register() error = %q, want it to contain %q", err, want)
				}
			}
			for _, unwanted := range []string{"PATCH", "no later than"} {
				if strings.Contains(err.Error(), unwanted) {
					t.Errorf("Register() error = %q, want it to name no %q: the Reporting API answers "+
						"422 for a valid_to that is not after the stored valid_from, so that close is "+
						"no way out of this registry", err, unwanted)
				}
			}
		})
	}
}

// TestRegistrarRelatesBesideAClosedRelationAndAnotherType covers the two
// relations a rerun leaves alone: one that was ended before this month began,
// which attributes nothing of it, and one of another type, which attributes
// nothing at all. Refusing either would leave an operator with a registry they
// cannot register into again.
func TestRegistrarRelatesBesideAClosedRelationAndAnotherType(t *testing.T) {
	registrations := monthRegistrations(t, namedMonth(t, 1, july2026, tenantsCloud))
	other := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	// The relation ends where the month begins, so neither read finds it: the
	// registry serves the relations valid at an instant, and this one is valid at
	// none from the first instant of July on.
	server := newRegistryServer(t, holdsRelations(t, heldRelation{
		id: uuid.New(), target: other,
		validFrom: "2026-06-01T00:00:00Z", validTo: "2026-07-01T00:00:00Z",
	}))

	report, err := NewRegistrar(server.URL, testAPIToken, testLogger(t)).
		Register(t.Context(), registrations)
	if err != nil {
		t.Fatalf("Register() error = %v, want nil: a closed relation attributes nothing", err)
	}
	if want := (RegistrationReport{ProjectsCreated: 8, RelationsCreated: 2}); report != want {
		t.Errorf("Register() = %+v, want %+v", report, want)
	}
	for _, request := range server.received() {
		if request.method != http.MethodGet || !strings.HasSuffix(request.path, "/relations") {
			continue
		}
		// The read asks for one type, so a relation of another one is never in the
		// answer and never has to be told from a relation of this one.
		if got := request.rawQuery; !strings.Contains(got, "relation_type="+relationInfrastructureTenant) {
			t.Errorf("the check asks %q, want it to name the relation type: another type attributes "+
				"nothing and is no reason to refuse", got)
		}
	}
}

// TestRegistrarStopsOnARefusedRelation covers a registry that took every row and
// then refused the relation between two of them, which is what a contract the
// relation document does not satisfy answers. The projects are in place, so the
// report says how far the run came, and the message names the route an operator
// has to look at.
func TestRegistrarStopsOnARefusedRelation(t *testing.T) {
	registrations := monthRegistrations(t, namedMonth(t, 1, july2026, tenantsCloud))
	server := newRegistryServer(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) bool {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/relations") {
			return false
		}
		writeAnswer(w, http.StatusBadRequest, `{"type":"urn:tally:error:validation","status":400}`)
		return true
	})

	report, err := NewRegistrar(server.URL, testAPIToken, testLogger(t)).
		Register(t.Context(), registrations)

	if err == nil {
		t.Fatalf("Register() error = nil, want a refused relation to end the registration")
	}
	if want := "the Reporting API answered 400 for POST " + projectsRoute + "/"; !strings.Contains(err.Error(), want) {
		t.Errorf("Register() error = %q, want it to contain %q: the refused route is what an operator "+
			"looks at", err, want)
	}
	if want := `{"type":"urn:tally:error:validation","status":400}`; !strings.HasSuffix(err.Error(), want) {
		t.Errorf("Register() error = %q, want it to end in %q: what the API said is the reason",
			err, want)
	}
	if want := (RegistrationReport{ProjectsCreated: 8}); report != want {
		t.Errorf("Register() = %+v, want %+v: the report counts what got through before the refusal",
			report, want)
	}
}

// TestRegistrarRegistersAnEmptySetWithoutARequest covers the set that names
// nothing. It is not a failure, because a month naming no tenant registers
// nothing, and a run against an unreachable API would otherwise fail for a
// registration that has nothing to say.
func TestRegistrarRegistersAnEmptySetWithoutARequest(t *testing.T) {
	server := newRegistryServer(t, nil)

	report, err := NewRegistrar(server.URL, testAPIToken, testLogger(t)).
		Register(t.Context(), Registrations{})
	if err != nil {
		t.Fatalf("Register(ctx, Registrations{}) error = %v, want nil", err)
	}
	if want := (RegistrationReport{}); report != want {
		t.Errorf("Register(ctx, Registrations{}) = %+v, want %+v", report, want)
	}
	if got := len(server.received()); got != 0 {
		t.Errorf("a set naming nothing sent %d requests, want none", got)
	}
}

// TestRegistrarRefusesARelationNamingAnUnregisteredProject covers a set whose
// relation names a key the set does not register. It is refused before the first
// request, because the rows of such a set would be registered and then related
// to a project id that is the zero uuid.
func TestRegistrarRefusesARelationNamingAnUnregisteredProject(t *testing.T) {
	server := newRegistryServer(t, nil)
	registrations := Registrations{
		Projects: []ProjectRegistration{{
			Platform: "openstack",
			Key:      ProjectKey{Cloud: tenantsCloud, ExternalID: "p1"},
			Name:     "p1",
		}},
		Relations: []RelationRegistration{{
			Source:       ProjectKey{Cloud: gardenCloud, ExternalID: "ghost"},
			Target:       ProjectKey{Cloud: tenantsCloud, ExternalID: "p1"},
			RelationType: relationInfrastructureTenant,
			ValidFrom:    july2026,
		}},
	}

	report, err := NewRegistrar(server.URL, testAPIToken, testLogger(t)).
		Register(t.Context(), registrations)

	want := "the relation from (garden-sim, ghost) to (os-sim, p1) names a project the registrations do " +
		"not hold"
	if err == nil || err.Error() != want {
		t.Fatalf("Register() error = %v, want %q", err, want)
	}
	if got := len(server.received()); got != 0 {
		t.Errorf("a set with a relation to an unregistered key sent %d requests, want none: nothing is "+
			"registered that cannot be related", got)
	}
	if want := (RegistrationReport{}); report != want {
		t.Errorf("Register() = %+v, want %+v", report, want)
	}
}

// TestRegistrarFailsOnAnAnswerItCannotUse covers every answer that ends the
// registration. What each of them costs an operator is the same: the run stops
// and says which route answered what, so the fix is read off the message rather
// than out of the API's log.
func TestRegistrarFailsOnAnAnswerItCannotUse(t *testing.T) {
	registrations := monthRegistrations(t, namedMonth(t, 1, july2026, tenantsCloud))
	tenant := registrations.Projects[0].Key.ExternalID

	tests := []struct {
		name     string
		answer   registryAnswer
		contains string
		exact    string
		suffix   string
	}{
		{
			name:     "a token the API does not know",
			answer:   answersEverythingWith(http.StatusUnauthorized, `{"status":401}`),
			contains: "the Reporting API answered 401 for POST /api/v1/projects",
			suffix:   "TALLY_SIM_API_TOKEN has to be an api token of role admin",
		},
		{
			name:     "a token without the admin role",
			answer:   answersEverythingWith(http.StatusForbidden, `{"status":403}`),
			contains: "the Reporting API answered 403 for POST /api/v1/projects",
			suffix:   "TALLY_SIM_API_TOKEN has to be an api token of role admin",
		},
		{
			name:     "an API that failed",
			answer:   answersEverythingWith(http.StatusInternalServerError, `{"type":"urn:tally:error:internal"}`),
			contains: `the Reporting API answered 500 for POST /api/v1/projects: {"type":"urn:tally:error:internal"}`,
		},
		{
			name:   "a registration without an id",
			answer: answersEverythingWith(http.StatusCreated, `{}`),
			exact:  "the Reporting API answered 201 without a project id",
		},
		{
			name:     "a registration that is no document",
			answer:   answersEverythingWith(http.StatusCreated, `not json`),
			contains: "decoding the registered project:",
		},
		{
			name:     "a refusal the lookup finds nothing for",
			answer:   holdsEveryProject(`{"items":[],"next_cursor":null}`),
			contains: fmt.Sprintf("refused (%s, %s) as registered and lists no such project", tenantsCloud, tenant),
		},
		{
			name: "a lookup that finds the key twice",
			answer: holdsEveryProject(fmt.Sprintf(`{"items":[{"id":%q},{"id":%q}],"next_cursor":null}`,
				uuid.New(), uuid.New())),
			contains: fmt.Sprintf("lists 2 projects for (%s, %s), want one", tenantsCloud, tenant),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newRegistryServer(t, tt.answer)

			_, err := NewRegistrar(server.URL, testAPIToken, testLogger(t)).
				Register(t.Context(), registrations)

			if err == nil {
				t.Fatalf("Register() error = nil, want the registration to end")
			}
			if tt.exact != "" && err.Error() != tt.exact {
				t.Errorf("Register() error = %q, want %q", err, tt.exact)
			}
			if tt.contains != "" && !strings.Contains(err.Error(), tt.contains) {
				t.Errorf("Register() error = %q, want it to contain %q", err, tt.contains)
			}
			if tt.suffix != "" && !strings.HasSuffix(err.Error(), tt.suffix) {
				t.Errorf("Register() error = %q, want it to end in %q: the answer says the token is "+
					"refused and the message says which token it is", err, tt.suffix)
			}
			if strings.Contains(err.Error(), testAPIToken) {
				t.Errorf("Register() error = %q, want it to name no token", err)
			}
		})
	}
}

// TestRegistrarEncodesTheLookupKeyInTheQuery covers a key carrying the two
// characters a query is built with. An unencoded ampersand would make the lookup
// ask for another external id, and the answer would be a row this run never
// registered.
func TestRegistrarEncodesTheLookupKeyInTheQuery(t *testing.T) {
	server := newRegistryServer(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) bool {
		if r.Method == http.MethodGet {
			writeAnswer(w, http.StatusInternalServerError, `{"type":"urn:tally:error:internal"}`)
			return true
		}
		writeAnswer(w, http.StatusConflict, conflictProblem)
		return true
	})
	registrations := Registrations{Projects: []ProjectRegistration{{
		Platform: "openstack",
		Key:      ProjectKey{Cloud: tenantsCloud, ExternalID: "id with space&x"},
		Name:     "odd",
	}}}

	_, err := NewRegistrar(server.URL, testAPIToken, testLogger(t)).
		Register(t.Context(), registrations)

	want := "the Reporting API answered 500 for GET /api/v1/projects"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Register() error = %v, want it to contain %q", err, want)
	}
	requests := server.received()
	if len(requests) != 2 {
		t.Fatalf("the registration sent %d requests, want 2: the refused POST and the lookup",
			len(requests))
	}
	if got, want := requests[1].rawQuery, "cloud=os-sim&external_id=id+with+space%26x"; got != want {
		t.Errorf("the lookup asks %q, want %q: the key goes onto the URL encoded", got, want)
	}
}

// TestRegistrarQuotesOnlyTheFirstBytesOfARefusedAnswer covers a destination that
// answers at length. What answers may be a proxy serving an HTML error page, and
// the whole of it in one error line is not what an operator reads.
func TestRegistrarQuotesOnlyTheFirstBytesOfARefusedAnswer(t *testing.T) {
	server := newRegistryServer(t,
		answersEverythingWith(http.StatusInternalServerError, strings.Repeat("x", 1000)))

	_, err := NewRegistrar(server.URL, testAPIToken, testLogger(t)).
		Register(t.Context(), monthRegistrations(t, namedMonth(t, 1, july2026, tenantsCloud)))

	if err == nil {
		t.Fatalf("Register() error = nil, want the registration to end")
	}
	if !strings.Contains(err.Error(), strings.Repeat("x", refusalBodyMax)) {
		t.Errorf("Register() error = %q, want it to quote the first %d bytes of the answer",
			err, refusalBodyMax)
	}
	if strings.Contains(err.Error(), strings.Repeat("x", refusalBodyMax+1)) {
		t.Errorf("Register() error quotes more than %d bytes of the answer, want the rest cut off",
			refusalBodyMax)
	}
}

// TestRegistrarNamesTheRouteAndNotTheTokenOnATransportFailure covers a Reporting
// API that is not there. The message names the route so an operator sees how far
// the registration came, and it carries no credential, because a failed run is
// what gets pasted into a ticket.
func TestRegistrarNamesTheRouteAndNotTheTokenOnATransportFailure(t *testing.T) {
	registrations := monthRegistrations(t, namedMonth(t, 1, july2026, tenantsCloud))

	_, err := NewRegistrar("http://127.0.0.1:1", testAPIToken, testLogger(t)).
		Register(t.Context(), registrations)

	if err == nil {
		t.Fatalf("Register() error = nil, want a registration against a closed port to fail")
	}
	if want := "POST " + projectsRoute + ":"; !strings.Contains(err.Error(), want) {
		t.Errorf("Register() error = %q, want it to contain %q", err, want)
	}
	if strings.Contains(err.Error(), testAPIToken) {
		t.Errorf("Register() error = %q, want it to name no token", err)
	}
}

// TestRegistrarDoesNotFollowARedirect covers a destination that answers a
// redirect rather than the registry, which an ingress or anything terminating
// TLS in front of the API can. Go carries the Authorization header across a
// redirect within one host, so a 308 to http:// on that host would put the admin
// token on the wire in the clear, which is exactly what the https check on the
// configured URL exists to rule out; a redirect that kept the token would be no
// better on 301, 302 and 303, where Go turns the POST into a GET and the
// registration registers nothing. The answer is therefore taken as it stands.
func TestRegistrarDoesNotFollowARedirect(t *testing.T) {
	registrations := monthRegistrations(t, namedMonth(t, 1, july2026, tenantsCloud))
	server := newRegistryServer(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) bool {
		if r.URL.Path != projectsRoute {
			return false
		}
		w.Header().Set("Location", "/moved"+projectsRoute)
		writeAnswer(w, http.StatusPermanentRedirect, `{"status":308}`)
		return true
	})

	_, err := NewRegistrar(server.URL, testAPIToken, testLogger(t)).
		Register(t.Context(), registrations)

	if err == nil {
		t.Fatalf("Register() error = nil, want the redirect to end the registration")
	}
	if want := "the Reporting API answered 308 for POST " + projectsRoute; !strings.Contains(err.Error(), want) {
		t.Errorf("Register() error = %q, want it to contain %q: a redirect is an answer no path here "+
			"can use", err, want)
	}
	for _, request := range server.received() {
		if request.path != projectsRoute {
			t.Errorf("the registration sent %s %s, want nothing sent to what the Location names: a "+
				"followed redirect carries the admin token wherever it points",
				request.method, request.path)
		}
	}
}

// TestRegistrarStopsOnACancelledContext covers the SIGINT that arrives while a
// month is being registered. The cancellation reaches the caller as itself, so a
// run that was stopped is told apart from one the API refused.
func TestRegistrarStopsOnACancelledContext(t *testing.T) {
	server := newRegistryServer(t, nil)
	registrations := monthRegistrations(t, namedMonth(t, 1, july2026, tenantsCloud))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := NewRegistrar(server.URL, testAPIToken, testLogger(t)).Register(ctx, registrations)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Register() error = %v, want it to carry %v", err, context.Canceled)
	}
	if got := len(server.received()); got != 0 {
		t.Errorf("a cancelled registration sent %d requests, want none", got)
	}
}

// TestRegistrarTrimsATrailingSlashAndLogsThroughTheDefaultLogger covers the base
// URL as an operator writes it. A trailing slash would build //api/v1/projects,
// which is another route, and a nil logger is what a caller passes that has none
// of its own.
func TestRegistrarTrimsATrailingSlashAndLogsThroughTheDefaultLogger(t *testing.T) {
	server := newRegistryServer(t, nil)
	registrations := Registrations{Projects: []ProjectRegistration{{
		Platform: "openstack",
		Key:      ProjectKey{Cloud: tenantsCloud, ExternalID: "p1"},
		Name:     "p1",
	}}}
	var log bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&log, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	report, err := NewRegistrar(server.URL+"/", testAPIToken, nil).Register(t.Context(), registrations)
	if err != nil {
		t.Fatalf("Register() error = %v, want nil", err)
	}
	if want := (RegistrationReport{ProjectsCreated: 1}); report != want {
		t.Errorf("Register() = %+v, want %+v", report, want)
	}
	requests := server.received()
	if len(requests) != 1 {
		t.Fatalf("the registration sent %d requests, want 1", len(requests))
	}
	if requests[0].path != projectsRoute {
		t.Errorf("the project is posted to %q, want %q: the slash of the base URL is trimmed",
			requests[0].path, projectsRoute)
	}
	if got := log.String(); !strings.Contains(got, "registered project") {
		t.Errorf("the default logger holds %q, want a line about the registered project", got)
	}
	if got := log.String(); strings.Contains(got, testAPIToken) {
		t.Errorf("the default logger holds %q, want it to name no token", got)
	}
}
