// The Reporting API the auditability drill walks. It is built in process over
// the case's reporting database, through the real router, in enforced auth mode
// with a seeded read_all token. No socket is opened: the requests go through
// handler.ServeHTTP on a recorder, so what the drill reads has passed the
// contract validator, the auth guard and the handlers, and the answer is the
// one an operator asking the same question would get.
package engine_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/b42labs/tally/internal/reporting/auth"
	"github.com/b42labs/tally/internal/reporting/httpapi"
	"github.com/b42labs/tally/internal/reporting/ingest"
	"github.com/b42labs/tally/internal/reporting/reconciliation"
	"github.com/b42labs/tally/internal/reporting/registry"
	reportingsqlcgen "github.com/b42labs/tally/internal/reporting/store/sqlcgen"
)

// registryAPI is the API one drill reads the registry through: the router it
// serves with, and the token it presents.
type registryAPI struct {
	handler http.Handler
	token   string
}

// newRegistryAPI assembles the router over the case's reporting database and
// issues the query token the drill reads with.
func newRegistryAPI(t *testing.T, dbs caseDBs) registryAPI {
	t.Helper()

	q := reportingsqlcgen.New(dbs.reporting)

	handler, err := httpapi.NewRouter(httpapi.Options{
		Logger:                   slog.New(slog.NewJSONHandler(io.Discard, nil)),
		DB:                       dbs.reportingStore,
		UnhealthyThreshold:       time.Minute,
		Queries:                  q,
		Store:                    dbs.reportingStore,
		AuthMode:                 auth.ModeEnforced,
		InternalToken:            "golden-internal-token",
		Authenticator:            auth.NewStaticTokenAuthenticator(q),
		Pipeline:                 ingest.New(registry.New(), false, nil, nil),
		AttributingRelationTypes: attributingRelationTypes,
		Syncer: reconciliation.New(dbs.reportingStore, ingest.New(registry.New(), false, nil, nil),
			reconciliation.Config{}, map[string]reconciliation.Adapter{}, time.Now, nil),
	})
	if err != nil {
		t.Fatalf("httpapi.NewRouter: %v", err)
	}

	token, err := auth.GenerateAPIToken()
	if err != nil {
		t.Fatalf("generating the api token: %v", err)
	}
	// project_ids is NOT NULL, so a token that names no project carries an empty
	// array rather than a nil slice, which pgx would send as NULL.
	if _, err := q.CreateAPIToken(t.Context(), reportingsqlcgen.CreateAPITokenParams{
		TokenHash:   auth.HashToken(token),
		Role:        string(auth.RoleReadAll),
		ProjectIds:  []uuid.UUID{},
		Description: pgtype.Text{String: "golden auditability drill", Valid: true},
	}); err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	return registryAPI{handler: handler, token: token}
}

// get reads one endpoint into v. A path-only target routes because the contract
// declares no servers, so the drill names the paths the way the contract does.
// A status other than 200 is a finding about the walk rather than a decoding
// problem, so it fails naming the target, the status and the body the API
// answered with.
func (api registryAPI) get(t *testing.T, target string, into any) {
	t.Helper()

	r := httptest.NewRequest(http.MethodGet, target, nil)
	r.Header.Set("Authorization", "Bearer "+api.token)
	rec := httptest.NewRecorder()
	api.handler.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", target, rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), into); err != nil {
		t.Fatalf("decoding the answer of GET %s: %v", target, err)
	}
}
