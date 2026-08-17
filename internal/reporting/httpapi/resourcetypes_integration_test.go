package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/b42labs/tally/internal/reporting/auth"
	"github.com/b42labs/tally/internal/reporting/httpapi/problem"
	"github.com/b42labs/tally/internal/reporting/store/sqlcgen"
	"github.com/b42labs/tally/internal/reporting/store/storetest"
)

// instanceRoute is the registry route of the resource type the subtests
// register against, spelled once because half of them address it.
const instanceRoute = "/api/v1/resource-types/openstack/instance"

// TestResourceTypesOverHTTP drives the registry endpoints through the whole
// stack: the contract validator, the role a route demands, the schema compiler,
// and the rows behind them. The subtests share one database and run in order,
// so the ones reading the seeded registry come before the ones registering a
// pair of their own.
func TestResourceTypesOverHTTP(t *testing.T) {
	db := storetest.NewDB(t)
	a := newAPI(t, db.Store)

	adminID, adminToken := seedAPIToken(t, a.queries, auth.RoleAdmin, nil)
	_, readAllToken := seedAPIToken(t, a.queries, auth.RoleReadAll, nil)
	_, projectToken := seedAPIToken(t, a.queries, auth.RoleProject, []uuid.UUID{uuid.New()})

	t.Run("registers a size schema and replaces it on the next put", func(t *testing.T) {
		first := map[string]any{
			"type":       "object",
			"required":   []any{"vcpus", "flavor"},
			"properties": map[string]any{"vcpus": map[string]any{"type": "integer", "minimum": 1}},
		}

		got := resourceTypeOf(t, a.call(t, http.MethodPut, instanceRoute, adminToken, registration(t, first)))

		if got.Platform != "openstack" || got.ResourceType != "instance" {
			t.Errorf("answer names (%s, %s), want (openstack, instance)", got.Platform, got.ResourceType)
		}
		if want := asDocument(t, first); !reflect.DeepEqual(got.SizeSchema, want) {
			t.Errorf("answered size schema = %v, want %v", got.SizeSchema, want)
		}
		stored, storedAt := storedResourceType(t, a, "openstack", "instance")
		if want := asDocument(t, first); !reflect.DeepEqual(stored, want) {
			t.Errorf("stored size schema = %v, want %v", stored, want)
		}

		// The same pair again, with a document that shares nothing with the first.
		second := map[string]any{
			"type":       "object",
			"required":   []any{"flavor"},
			"properties": map[string]any{"flavor": map[string]any{"type": "string"}},
		}

		replaced := resourceTypeOf(t, a.call(t, http.MethodPut, instanceRoute, adminToken, registration(t, second)))

		if want := asDocument(t, second); !reflect.DeepEqual(replaced.SizeSchema, want) {
			t.Errorf("answered size schema = %v, want %v", replaced.SizeSchema, want)
		}
		replacedDoc, replacedAt := storedResourceType(t, a, "openstack", "instance")
		if want := asDocument(t, second); !reflect.DeepEqual(replacedDoc, want) {
			t.Errorf("stored size schema = %v, want %v", replacedDoc, want)
		}
		if !replacedAt.After(storedAt) {
			t.Errorf("updated_at = %v, want it after the %v of the registration it replaced", replacedAt, storedAt)
		}
	})

	t.Run("refuses a size schema that does not compile and stores nothing", func(t *testing.T) {
		const route = "/api/v1/resource-types/openstack/volume"
		before, beforeAt := storedResourceType(t, a, "openstack", "volume")

		// A document the contract takes, because size_schema is an object, and the
		// compiler does not, because type takes a string.
		rec := a.call(t, http.MethodPut, route, adminToken, registration(t, map[string]any{"type": 42}))

		assertProblem(t, rec, http.StatusBadRequest, problem.TypeValidation)
		detail := problemDetail(t, rec)
		if !strings.Contains(detail, "compiling the size schema") || !strings.Contains(detail, "/type") {
			t.Errorf("detail = %q, want the compiler's message naming the keyword at fault", detail)
		}

		after, afterAt := storedResourceType(t, a, "openstack", "volume")
		if !reflect.DeepEqual(after, before) || !afterAt.Equal(beforeAt) {
			t.Errorf("stored row = %v at %v, want it unchanged at %v, %v", after, afterAt, before, beforeAt)
		}
	})

	t.Run("refuses a registration without a size schema", func(t *testing.T) {
		rec := a.call(t, http.MethodPut, instanceRoute, adminToken, []byte(`{}`))

		assertProblem(t, rec, http.StatusBadRequest, problem.TypeValidation)
	})

	t.Run("refuses a registration the credential may not make", func(t *testing.T) {
		// The pair is registered nowhere, so a refusal that wrote anyway is
		// visible afterwards.
		const route = "/api/v1/resource-types/openstack/refused"
		body := registration(t, map[string]any{"type": "object"})

		t.Run("a read_all token", func(t *testing.T) {
			rec := a.call(t, http.MethodPut, route, readAllToken, body)

			assertProblem(t, rec, http.StatusForbidden, problem.TypeForbidden)
		})

		t.Run("a project token", func(t *testing.T) {
			rec := a.call(t, http.MethodPut, route, projectToken, body)

			assertProblem(t, rec, http.StatusForbidden, problem.TypeForbidden)
		})

		t.Run("no token", func(t *testing.T) {
			rec := a.call(t, http.MethodPut, route, "", body)

			assertChallenged(t, rec)
		})

		if registered(t, a, "openstack", "refused") {
			t.Error("(openstack, refused) is registered, want a refused registration to write nothing")
		}
	})

	t.Run("lists the resource types the migration chain seeded", func(t *testing.T) {
		rec := a.call(t, http.MethodGet, "/api/v1/resource-types", projectToken, nil)

		if got := rec.Code; got != http.StatusOK {
			t.Fatalf("status = %d, want %d (body %q)", got, http.StatusOK, rec.Body)
		}

		// The cursor member is sent as null rather than left out: a generated
		// client carries it, and a page that ends is what null says.
		var envelope map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("decoding the body %q: %v", rec.Body.String(), err)
		}
		cursor, ok := envelope["next_cursor"]
		if !ok {
			t.Error("the answer carries no next_cursor, want it sent as null")
		}
		if cursor != nil {
			t.Errorf("next_cursor = %v, want null", cursor)
		}

		var list ResourceTypeList
		if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
			t.Fatalf("decoding the body %q: %v", rec.Body.String(), err)
		}
		// The seeds are the openstack pairs, and the pairs a subtest registers
		// carry a platform of their own, so this stays an exact comparison
		// without depending on which subtests have run before it.
		var pairs []string
		for _, entry := range list.Items {
			if entry.Platform != "openstack" {
				continue
			}
			pairs = append(pairs, entry.Platform+":"+entry.ResourceType)
		}
		want := []string{
			"openstack:floating_ip", "openstack:image", "openstack:instance",
			"openstack:loadbalancer", "openstack:volume",
		}
		if !slices.Equal(pairs, want) {
			t.Errorf("listed openstack pairs = %v, want the seeded %v", pairs, want)
		}
	})

	t.Run("reads one registered resource type", func(t *testing.T) {
		got := resourceTypeOf(t, a.call(t, http.MethodGet, instanceRoute, projectToken, nil))

		stored, storedAt := storedResourceType(t, a, "openstack", "instance")
		if got.Platform != "openstack" || got.ResourceType != "instance" {
			t.Errorf("answer names (%s, %s), want (openstack, instance)", got.Platform, got.ResourceType)
		}
		if !reflect.DeepEqual(got.SizeSchema, stored) {
			t.Errorf("answered size schema = %v, want the stored %v", got.SizeSchema, stored)
		}
		if !got.UpdatedAt.Equal(storedAt) {
			t.Errorf("answered updated_at = %v, want the stored %v", got.UpdatedAt, storedAt)
		}
	})

	t.Run("answers a pair nothing is registered for with a not_found problem", func(t *testing.T) {
		rec := a.call(t, http.MethodGet, "/api/v1/resource-types/openstack/black_hole", projectToken, nil)

		assertProblem(t, rec, http.StatusNotFound, problem.TypeNotFound)
	})

	t.Run("records a registration against the token that made it", func(t *testing.T) {
		// A platform of the test's own, so registering a pair here does not
		// change what the subtest above asserts the seeded registry holds.
		const route = "/api/v1/resource-types/tallytest/share"

		resourceTypeOf(t, a.call(t, http.MethodPut, route, adminToken,
			registration(t, map[string]any{"type": "object"})))

		actors := registrationAudits(t, a, "tallytest", "share")
		if len(actors) != 1 {
			t.Fatalf("registration audit rows = %v, want exactly one", actors)
		}
		if actors[0] != adminID.String() {
			t.Errorf("audit actor = %q, want the token id %q", actors[0], adminID)
		}
	})

	t.Run("applies a newly registered schema without a restart", func(t *testing.T) {
		const (
			cloud = "os-http-registry"
			route = "/api/v1/resource-types/openstack/image"
		)
		_, ingestToken := seedIngestCredential(t, a.queries, fixturePlatform, cloud)
		res := fixture{cloud: cloud, resourceType: "image", id: "img-registry"}

		// The seeded (openstack, image) schema takes any size_gb from zero up.
		accepted := res.event("img-registry-create", "image.create", createTime,
			payloadOf("active", map[string]any{"size_gb": 10}))
		first := ingestResultOf(t, a.call(t, http.MethodPost, "/api/v1/events", ingestToken, item(t, accepted)))
		if first.Accepted != 1 {
			t.Fatalf("result = %+v, want the event accepted under the seeded schema", first)
		}

		stricter := map[string]any{
			"type":     "object",
			"required": []any{"size_gb"},
			"properties": map[string]any{
				"size_gb": map[string]any{"type": "number", "minimum": 100},
			},
		}
		resourceTypeOf(t, a.call(t, http.MethodPut, route, adminToken, registration(t, stricter)))

		// The same router, the same pipeline, the same compiled-schema cache: what
		// makes the next event meet the new schema is the updated_at of the row it
		// was registered with.
		refused := res.event("img-registry-update", "image.update", updateTime,
			payloadOf("active", map[string]any{"size_gb": 20}))

		second := ingestResultOf(t, a.call(t, http.MethodPost, "/api/v1/events", ingestToken, item(t, refused)))

		if second.Accepted != 0 {
			t.Errorf("result = %+v, want the event refused under the registered schema", second)
		}
		if len(second.Rejected) != 1 {
			t.Fatalf("rejected = %+v, want the event the new schema refuses", second.Rejected)
		}
		assertRejected(t, second.Rejected[0], 0, refused.EventID, "size_schema: ")
	})
}

// seedAPIToken issues a query token for the given role and scope, stores its
// hash, and returns the row's id together with the token itself.
func seedAPIToken(t *testing.T, q *sqlcgen.Queries, role auth.Role, projectIDs []uuid.UUID) (uuid.UUID, string) {
	t.Helper()

	token, err := auth.GenerateAPIToken()
	if err != nil {
		t.Fatalf("generating the api token: %v", err)
	}
	// project_ids is NOT NULL, so a token that names no project carries an empty
	// array rather than a nil slice, which pgx would send as NULL.
	if projectIDs == nil {
		projectIDs = []uuid.UUID{}
	}
	id, err := q.CreateAPIToken(t.Context(), sqlcgen.CreateAPITokenParams{
		TokenHash:   auth.HashToken(token),
		Role:        string(role),
		ProjectIds:  projectIDs,
		Description: pgtype.Text{String: "httpapi integration test", Valid: true},
	})
	if err != nil {
		t.Fatalf("CreateAPIToken() error = %v, want nil", err)
	}
	return id, token
}

// registration renders the body a PUT carries.
func registration(t *testing.T, schema map[string]any) []byte {
	t.Helper()

	raw, err := json.Marshal(map[string]any{"size_schema": schema})
	if err != nil {
		t.Fatalf("marshaling the registration: %v", err)
	}
	return raw
}

// resourceTypeOf decodes the answer of a registration or a read, which the
// contract promises is a 200 carrying one resource type.
func resourceTypeOf(t *testing.T, rec *httptest.ResponseRecorder) ResourceType {
	t.Helper()

	if got := rec.Code; got != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %q)", got, http.StatusOK, rec.Body)
	}

	var got ResourceType
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding the body %q: %v", rec.Body.String(), err)
	}
	return got
}

// problemDetail is the detail member of an error response.
func problemDetail(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()

	var body struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the body %q: %v", rec.Body.String(), err)
	}
	return body.Detail
}

// asDocument is what a Go value looks like once it has been through JSON, which
// is the form both the answer and the stored row come back in.
func asDocument(t *testing.T, value any) map[string]any {
	t.Helper()

	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshaling %v: %v", value, err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("decoding %s: %v", raw, err)
	}
	return document
}

// storedResourceType reads the registered row as the database holds it.
func storedResourceType(t *testing.T, a api, platform, resourceType string) (map[string]any, time.Time) {
	t.Helper()

	var schema map[string]any
	var updatedAt time.Time
	if err := a.store.Pool().QueryRow(t.Context(),
		`SELECT size_schema, updated_at FROM resource_types WHERE platform = $1 AND resource_type = $2`,
		platform, resourceType).Scan(&schema, &updatedAt); err != nil {
		t.Fatalf("reading the registered (%s, %s): %v", platform, resourceType, err)
	}
	return schema, updatedAt
}

// registered reports whether the pair holds a row at all.
func registered(t *testing.T, a api, platform, resourceType string) bool {
	t.Helper()

	var found int
	if err := a.store.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM resource_types WHERE platform = $1 AND resource_type = $2`,
		platform, resourceType).Scan(&found); err != nil {
		t.Fatalf("counting the rows of (%s, %s): %v", platform, resourceType, err)
	}
	return found > 0
}

// registrationAudits returns the actor of every resource_types.register row
// naming the pair, oldest first.
func registrationAudits(t *testing.T, a api, platform, resourceType string) []string {
	t.Helper()

	rows, err := a.store.Pool().Query(t.Context(),
		`SELECT actor FROM audit_log
		 WHERE action = 'resource_types.register' AND object_id = $1 ORDER BY id`,
		platform+":"+resourceType)
	if err != nil {
		t.Fatalf("reading the registration audit rows of (%s, %s): %v", platform, resourceType, err)
	}
	defer rows.Close()

	var actors []string
	for rows.Next() {
		var actor string
		if err := rows.Scan(&actor); err != nil {
			t.Fatalf("scanning an audit row: %v", err)
		}
		actors = append(actors, actor)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the registration audit rows of (%s, %s): %v", platform, resourceType, err)
	}
	return actors
}
