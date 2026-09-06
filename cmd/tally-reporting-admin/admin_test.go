package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/b42labs/tally/internal/refdoc"
	"github.com/b42labs/tally/internal/reporting/auth"
	"github.com/b42labs/tally/internal/reporting/config"
	"github.com/b42labs/tally/internal/reporting/store/sqlcgen"
	"github.com/b42labs/tally/internal/reporting/store/storetest"
	reportingmigrations "github.com/b42labs/tally/migrations/reporting"
)

// The shape of the two token kinds as they reach stdout.
var (
	ingestTokenPattern = regexp.MustCompile(`^tly_i_[0-9a-f]{64}$`)
	apiTokenPattern    = regexp.MustCompile(`^tly_a_[0-9a-f]{64}$`)
)

func TestAdminCLI(t *testing.T) {
	db := storetest.NewDB(t)
	useDatabase(t, db.URL)
	q := sqlcgen.New(db.Store.Pool())

	t.Run("create-ingest-credential prints the token once and stores only its hash", func(t *testing.T) {
		stdout, stderr, err := runCLI(t, "create-ingest-credential",
			"--platform", "openstack", "--cloud", "os-prod-eu1", "--description", "cli integration test")
		if err != nil {
			t.Fatalf("create-ingest-credential error = %v, want nil (stderr %q)", err, stderr)
		}

		token := onlyLine(t, stdout)
		if !ingestTokenPattern.MatchString(token) {
			t.Errorf("stdout = %q, want a token matching %s", token, ingestTokenPattern)
		}

		id := idOfTokenHash(t, db, "ingest_credentials", auth.HashToken(token))
		if !strings.Contains(stderr, id.String()) {
			t.Errorf("stderr = %q, want it to name the new row %s", stderr, id)
		}
		if want := "will not be shown again"; !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want it to contain %q", stderr, want)
		}
		assertTokenIsNotStored(t, db, token)
	})

	t.Run("create-api-token prints the token once and stores only its hash", func(t *testing.T) {
		stdout, stderr, err := runCLI(t, "create-api-token",
			"--role", "admin", "--description", "cli integration test")
		if err != nil {
			t.Fatalf("create-api-token error = %v, want nil (stderr %q)", err, stderr)
		}

		token := onlyLine(t, stdout)
		if !apiTokenPattern.MatchString(token) {
			t.Errorf("stdout = %q, want a token matching %s", token, apiTokenPattern)
		}

		id := idOfTokenHash(t, db, "api_tokens", auth.HashToken(token))
		if !strings.Contains(stderr, id.String()) {
			t.Errorf("stderr = %q, want it to name the new row %s", stderr, id)
		}
		if want := "will not be shown again"; !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want it to contain %q", stderr, want)
		}
		assertTokenIsNotStored(t, db, token)
	})

	t.Run("every mutation leaves one audit row", func(t *testing.T) {
		ingestID, _ := createIngestCredential(t, db, "os-audit")
		tokenID, _ := createAPIToken(t, db, "--role", "read_all")
		revoke(t, "revoke-ingest-credential", ingestID)
		revoke(t, "revoke-api-token", tokenID)

		for _, tc := range []struct {
			action   string
			objectID uuid.UUID
		}{
			{"ingest_credentials.create", ingestID},
			{"ingest_credentials.revoke", ingestID},
			{"api_tokens.create", tokenID},
			{"api_tokens.revoke", tokenID},
		} {
			if got := auditRows(t, db, tc.action, tc.objectID); got != 1 {
				t.Errorf("audit rows for %s on %s = %d, want 1", tc.action, tc.objectID, got)
			}
		}
	})

	t.Run("create-api-token refuses a scope its role cannot carry", func(t *testing.T) {
		for name, tc := range map[string]struct {
			args []string
			want string
		}{
			"a role nothing grants":             {[]string{"--role", "superuser"}, "--role"},
			"a project token without a project": {[]string{"--role", "project"}, "--project-id"},
			"a read_all token naming a project": {[]string{"--role", "read_all", "--project-id", uuid.NewString()}, "--project-id"},
			"a project id that is not a uuid":   {[]string{"--role", "project", "--project-id", "not-a-uuid"}, "--project-id"},
		} {
			t.Run(name, func(t *testing.T) {
				before := countRows(t, db, "api_tokens")

				stdout, _, err := runCLI(t, append([]string{"create-api-token"}, tc.args...)...)
				if err == nil {
					t.Fatalf("create-api-token error = nil, want one mentioning %s", tc.want)
				}
				if !strings.Contains(err.Error(), tc.want) {
					t.Errorf("create-api-token error = %q, want it to mention %s", err, tc.want)
				}
				if stdout != "" {
					t.Errorf("stdout = %q, want nothing printed for a token that was not issued", stdout)
				}
				if after := countRows(t, db, "api_tokens"); after != before {
					t.Errorf("api_tokens rows = %d, want %d unchanged", after, before)
				}
			})
		}
	})

	t.Run("create-meta-project registers the meta row and prints its id", func(t *testing.T) {
		stdout, stderr, err := runCLI(t, "create-meta-project",
			"--external-id", "customer-alpha", "--name", "Customer Alpha")
		if err != nil {
			t.Fatalf("create-meta-project error = %v, want nil (stderr %q)", err, stderr)
		}

		id, err := uuid.Parse(onlyLine(t, stdout))
		if err != nil {
			t.Fatalf("stdout = %q, want the id of the new row alone: %v", stdout, err)
		}
		if want := "registered meta-project customer-alpha"; !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want it to contain %q", stderr, want)
		}

		want := registryRow{
			platform:   "meta",
			cloud:      "meta",
			externalID: "customer-alpha",
			name:       pgtype.Text{String: "Customer Alpha", Valid: true},
			metadata:   "{}",
		}
		if got := projectRow(t, db, id); got != want {
			t.Errorf("projects row %s = %+v, want %+v", id, got, want)
		}
		if got := auditRows(t, db, "projects.create", id); got != 1 {
			t.Errorf("audit rows for projects.create on %s = %d, want 1", id, got)
		}
	})

	t.Run("create-partner registers the partner row without a name", func(t *testing.T) {
		stdout, stderr, err := runCLI(t, "create-partner", "--external-id", "partner-corp")
		if err != nil {
			t.Fatalf("create-partner error = %v, want nil (stderr %q)", err, stderr)
		}

		id, err := uuid.Parse(onlyLine(t, stdout))
		if err != nil {
			t.Fatalf("stdout = %q, want the id of the new row alone: %v", stdout, err)
		}
		if want := "registered partner partner-corp"; !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want it to contain %q", stderr, want)
		}

		// A --name nobody passed leaves the column NULL rather than empty.
		want := registryRow{platform: "partner", cloud: "partner", externalID: "partner-corp", metadata: "{}"}
		if got := projectRow(t, db, id); got != want {
			t.Errorf("projects row %s = %+v, want %+v", id, got, want)
		}
		if got := auditRows(t, db, "projects.create", id); got != 1 {
			t.Errorf("audit rows for projects.create on %s = %d, want 1", id, got)
		}
	})

	t.Run("create-partner refuses a missing or empty external id", func(t *testing.T) {
		for name, tc := range map[string]struct {
			args []string
			want string
		}{
			"the flag left out":   {nil, `"external-id" not set`},
			"the flag left empty": {[]string{"--external-id", ""}, "--external-id: must not be empty"},
		} {
			t.Run(name, func(t *testing.T) {
				before := countRows(t, db, "projects")

				stdout, _, err := runCLI(t, append([]string{"create-partner"}, tc.args...)...)
				if err == nil {
					t.Fatalf("create-partner error = nil, want one containing %q", tc.want)
				}
				if !strings.Contains(err.Error(), tc.want) {
					t.Errorf("create-partner error = %q, want it to contain %q", err, tc.want)
				}
				if stdout != "" {
					t.Errorf("stdout = %q, want nothing printed for a project that was not registered", stdout)
				}
				if after := countRows(t, db, "projects"); after != before {
					t.Errorf("projects rows = %d, want %d unchanged", after, before)
				}
			})
		}
	})

	t.Run("create-partner refuses a second registration of one external id", func(t *testing.T) {
		if _, stderr, err := runCLI(t, "create-partner", "--external-id", "partner-twice"); err != nil {
			t.Fatalf("create-partner error = %v, want nil (stderr %q)", err, stderr)
		}
		before := countRows(t, db, "projects")

		stdout, _, err := runCLI(t, "create-partner", "--external-id", "partner-twice")
		if err == nil {
			t.Fatal("the second create-partner error = nil, want the held external id reported")
		}
		if want := "partner partner-twice: already registered"; !strings.Contains(err.Error(), want) {
			t.Errorf("the second create-partner error = %q, want it to contain %q", err, want)
		}
		if stdout != "" {
			t.Errorf("stdout = %q, want nothing printed for a project that was not registered", stdout)
		}
		if after := countRows(t, db, "projects"); after != before {
			t.Errorf("projects rows = %d, want %d unchanged", after, before)
		}
	})

	t.Run("revoking an ingest credential takes it out of use", func(t *testing.T) {
		id, token := createIngestCredential(t, db, "os-revoke")
		middleware := auth.Ingest(q, auth.ModeEnforced, nil)

		if got := statusFor(t, middleware, token); got != http.StatusNoContent {
			t.Fatalf("status before the revocation = %d, want %d", got, http.StatusNoContent)
		}

		revoke(t, "revoke-ingest-credential", id)

		if !revokedAt(t, db, "ingest_credentials", id).Valid {
			t.Error("revoked_at is NULL after the revocation")
		}
		if got := statusFor(t, middleware, token); got != http.StatusUnauthorized {
			t.Errorf("status after the revocation = %d, want %d", got, http.StatusUnauthorized)
		}

		t.Run("a second revocation changes nothing", func(t *testing.T) {
			stdout, stderr, err := runCLI(t, "revoke-ingest-credential", id.String())
			if err != nil {
				t.Fatalf("revoke-ingest-credential error = %v, want nil (stderr %q)", err, stderr)
			}
			if want := "already revoked"; !strings.Contains(stdout, want) {
				t.Errorf("stdout = %q, want it to contain %q", stdout, want)
			}
			if got := auditRows(t, db, "ingest_credentials.revoke", id); got != 1 {
				t.Errorf("audit rows after the second revocation = %d, want 1", got)
			}
		})

		t.Run("an id no credential holds is an error", func(t *testing.T) {
			_, _, err := runCLI(t, "revoke-ingest-credential", uuid.NewString())
			if err == nil {
				t.Fatal("revoke-ingest-credential error = nil, want one reporting the unknown id")
			}
			if want := "not found"; !strings.Contains(err.Error(), want) {
				t.Errorf("revoke-ingest-credential error = %q, want it to contain %q", err, want)
			}
		})

		t.Run("an argument that is not a uuid is an error", func(t *testing.T) {
			_, _, err := runCLI(t, "revoke-ingest-credential", "not-a-uuid")
			if err == nil {
				t.Fatal("revoke-ingest-credential error = nil, want one reporting the malformed id")
			}
			if want := "not a uuid"; !strings.Contains(err.Error(), want) {
				t.Errorf("revoke-ingest-credential error = %q, want it to contain %q", err, want)
			}
		})
	})

	t.Run("revoking a query token takes it out of use", func(t *testing.T) {
		id, token := createAPIToken(t, db, "--role", "admin")
		middleware := auth.Query(auth.NewStaticTokenAuthenticator(q), auth.RoleAdmin, auth.ModeEnforced, nil)

		if got := statusFor(t, middleware, token); got != http.StatusNoContent {
			t.Fatalf("status before the revocation = %d, want %d", got, http.StatusNoContent)
		}

		revoke(t, "revoke-api-token", id)

		if !revokedAt(t, db, "api_tokens", id).Valid {
			t.Error("revoked_at is NULL after the revocation")
		}
		if got := statusFor(t, middleware, token); got != http.StatusUnauthorized {
			t.Errorf("status after the revocation = %d, want %d", got, http.StatusUnauthorized)
		}

		t.Run("a second revocation changes nothing", func(t *testing.T) {
			stdout, stderr, err := runCLI(t, "revoke-api-token", id.String())
			if err != nil {
				t.Fatalf("revoke-api-token error = %v, want nil (stderr %q)", err, stderr)
			}
			if want := "already revoked"; !strings.Contains(stdout, want) {
				t.Errorf("stdout = %q, want it to contain %q", stdout, want)
			}
			if got := auditRows(t, db, "api_tokens.revoke", id); got != 1 {
				t.Errorf("audit rows after the second revocation = %d, want 1", got)
			}
		})

		t.Run("an id no token holds is an error", func(t *testing.T) {
			_, _, err := runCLI(t, "revoke-api-token", uuid.NewString())
			if err == nil {
				t.Fatal("revoke-api-token error = nil, want one reporting the unknown id")
			}
			if want := "not found"; !strings.Contains(err.Error(), want) {
				t.Errorf("revoke-api-token error = %q, want it to contain %q", err, want)
			}
		})
	})

	t.Run("migrate", func(t *testing.T) {
		t.Run("applies the chain to a database that has none of it", func(t *testing.T) {
			useDatabase(t, db.NewSiblingDB(t, "migrate_cli"))

			stdout, stderr, err := runCLI(t, "migrate")
			if err != nil {
				t.Fatalf("migrate error = %v, want nil (stderr %q)", err, stderr)
			}
			for _, version := range chainVersions() {
				if want := fmt.Sprintf("applied migration %d", version); !strings.Contains(stdout, want) {
					t.Errorf("stdout = %q, want it to contain %q", stdout, want)
				}
			}

			stdout, stderr, err = runCLI(t, "migrate")
			if err != nil {
				t.Fatalf("the second migrate error = %v, want nil (stderr %q)", err, stderr)
			}
			if want := "nothing to apply\n"; stdout != want {
				t.Errorf("stdout of the second run = %q, want %q", stdout, want)
			}
		})

		t.Run("migrate-status names the version of every migration", func(t *testing.T) {
			useDatabase(t, db.NewSiblingDB(t, "status_cli"))

			stdout, stderr, err := runCLI(t, "migrate-status")
			if err != nil {
				t.Fatalf("migrate-status error = %v, want nil (stderr %q)", err, stderr)
			}
			if want := migrationStatusOutput("pending"); stdout != want {
				t.Errorf("stdout = %q, want %q", stdout, want)
			}

			if _, stderr, err = runCLI(t, "migrate"); err != nil {
				t.Fatalf("migrate error = %v, want nil (stderr %q)", err, stderr)
			}

			stdout, stderr, err = runCLI(t, "migrate-status")
			if err != nil {
				t.Fatalf("migrate-status error = %v, want nil (stderr %q)", err, stderr)
			}
			if want := migrationStatusOutput("applied"); stdout != want {
				t.Errorf("stdout after migrating = %q, want %q", stdout, want)
			}
		})

		t.Run("migrate-down-to runs the chain's down migrations", func(t *testing.T) {
			useDatabase(t, db.NewSiblingDB(t, "down_cli"))
			if _, stderr, err := runCLI(t, "migrate"); err != nil {
				t.Fatalf("migrate error = %v, want nil (stderr %q)", err, stderr)
			}

			t.Run("and refuses to without --yes", func(t *testing.T) {
				_, _, err := runCLI(t, "migrate-down-to", "0")
				if err == nil {
					t.Fatal("migrate-down-to error = nil, want the missing confirmation reported")
				}
				if want := "--yes"; !strings.Contains(err.Error(), want) {
					t.Errorf("migrate-down-to error = %q, want it to mention %q", err, want)
				}
			})

			stdout, stderr, err := runCLI(t, "migrate-down-to", "0", "--yes")
			if err != nil {
				t.Fatalf("migrate-down-to error = %v, want nil (stderr %q)", err, stderr)
			}
			for _, version := range chainVersions() {
				if want := fmt.Sprintf("rolled back migration %d", version); !strings.Contains(stdout, want) {
					t.Errorf("stdout = %q, want it to contain %q", stdout, want)
				}
			}

			stdout, stderr, err = runCLI(t, "migrate-status")
			if err != nil {
				t.Fatalf("migrate-status error = %v, want nil (stderr %q)", err, stderr)
			}
			if want := migrationStatusOutput("pending"); stdout != want {
				t.Errorf("stdout after the rollback = %q, want %q", stdout, want)
			}
		})

		t.Run("migrate-down-to refuses a version that is not a number", func(t *testing.T) {
			useDatabase(t, db.URL)

			_, _, err := runCLI(t, "migrate-down-to", "latest", "--yes")
			if err == nil {
				t.Fatal("migrate-down-to error = nil, want the malformed version reported")
			}
			if want := "not a number"; !strings.Contains(err.Error(), want) {
				t.Errorf("migrate-down-to error = %q, want it to contain %q", err, want)
			}
		})

		t.Run("reports a database it cannot reach", func(t *testing.T) {
			useDatabase(t, "postgres://nobody@127.0.0.1:1/none")

			if _, _, err := runCLI(t, "migrate"); err == nil {
				t.Fatal("migrate error = nil, want the connection failure")
			}
		})
	})

	t.Run("create-meta-project reports a database it cannot reach", func(t *testing.T) {
		// The variable goes back to the shared database when the subtest ends,
		// so the ones after it still reach their rows.
		useDatabase(t, "postgres://nobody@127.0.0.1:1/none")

		stdout, _, err := runCLI(t, "create-meta-project", "--external-id", "unreachable")
		if err == nil {
			t.Fatal("create-meta-project error = nil, want the connection failure")
		}
		if stdout != "" {
			t.Errorf("stdout = %q, want nothing printed for a project that was not registered", stdout)
		}
	})

	t.Run("--help lists the two commands", func(t *testing.T) {
		// Building the tree reads no configuration, so the help text comes out
		// of a shell that has none set.
		useDatabase(t, "")

		stdout, stderr, err := runCLI(t, "--help")
		if err != nil {
			t.Fatalf("--help error = %v, want nil (stderr %q)", err, stderr)
		}
		for _, command := range []string{"create-meta-project", "create-partner"} {
			if !strings.Contains(stdout, command) {
				t.Errorf("stdout = %q, want it to list %s", stdout, command)
			}
		}
	})
}

// TestCreateRollsBackWhenTheAuditInsertFails runs against a container of its
// own: it drops the audit log, which every other test needs. The credential and
// its audit row share one transaction, so neither may survive alone.
func TestCreateRollsBackWhenTheAuditInsertFails(t *testing.T) {
	db := storetest.NewDB(t)
	useDatabase(t, db.URL)

	if _, err := db.Store.Pool().Exec(t.Context(), "DROP TABLE audit_log"); err != nil {
		t.Fatalf("dropping audit_log: %v", err)
	}

	stdout, _, err := runCLI(t, "create-ingest-credential", "--platform", "openstack", "--cloud", "os-rollback")
	if err == nil {
		t.Fatal("create-ingest-credential error = nil, want the failed audit insert")
	}
	if want := "audit log row"; !strings.Contains(err.Error(), want) {
		t.Errorf("create-ingest-credential error = %q, want the one from the audit insert, mentioning %q", err, want)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want no token printed for a credential that was rolled back", stdout)
	}
	if got := countRows(t, db, "ingest_credentials"); got != 0 {
		t.Errorf("ingest_credentials rows = %d, want 0: the credential outlived its audit row", got)
	}

	stdout, _, err = runCLI(t, "create-partner", "--external-id", "rollback")
	if err == nil {
		t.Fatal("create-partner error = nil, want the failed audit insert")
	}
	if want := "audit log row"; !strings.Contains(err.Error(), want) {
		t.Errorf("create-partner error = %q, want the one from the audit insert, mentioning %q", err, want)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want no id printed for a project that was rolled back", stdout)
	}
	if got := countRows(t, db, "projects"); got != 0 {
		t.Errorf("projects rows = %d, want 0: the project outlived its audit row", got)
	}
}

// useDatabase points the CLI at dbURL and blanks every other variable it reads.
// A variable set to the empty string falls back to its default exactly as an
// unset one does.
func useDatabase(t *testing.T, dbURL string) {
	t.Helper()

	for _, name := range config.EnvNames {
		t.Setenv(name, "")
	}
	t.Setenv("TALLY_REPORTING_DB_URL", dbURL)
}

// runCLI executes one command in process and returns its stdout, its stderr,
// and the error it ended with. It is the same command tree main runs, built
// over buffers instead of the process's streams.
func runCLI(t *testing.T, args ...string) (string, string, error) {
	t.Helper()

	var stdout, stderr bytes.Buffer
	root := newRootCmd()
	root.SetArgs(args)
	root.SetOut(&stdout)
	root.SetErr(&stderr)

	err := root.ExecuteContext(t.Context())
	return stdout.String(), stderr.String(), err
}

// createIngestCredential issues a credential for cloud through the CLI and
// returns the new row's id together with the token that was printed.
func createIngestCredential(t *testing.T, db storetest.DB, cloud string) (uuid.UUID, string) {
	t.Helper()

	stdout, stderr, err := runCLI(t, "create-ingest-credential", "--platform", "openstack", "--cloud", cloud)
	if err != nil {
		t.Fatalf("create-ingest-credential error = %v, want nil (stderr %q)", err, stderr)
	}
	token := onlyLine(t, stdout)
	return idOfTokenHash(t, db, "ingest_credentials", auth.HashToken(token)), token
}

// createAPIToken issues a query token through the CLI and returns the new row's
// id together with the token that was printed.
func createAPIToken(t *testing.T, db storetest.DB, args ...string) (uuid.UUID, string) {
	t.Helper()

	stdout, stderr, err := runCLI(t, append([]string{"create-api-token"}, args...)...)
	if err != nil {
		t.Fatalf("create-api-token error = %v, want nil (stderr %q)", err, stderr)
	}
	token := onlyLine(t, stdout)
	return idOfTokenHash(t, db, "api_tokens", auth.HashToken(token)), token
}

// revoke runs one of the revoke subcommands against id and fails the test when
// it reports an error.
func revoke(t *testing.T, command string, id uuid.UUID) {
	t.Helper()

	if _, stderr, err := runCLI(t, command, id.String()); err != nil {
		t.Fatalf("%s error = %v, want nil (stderr %q)", command, err, stderr)
	}
}

// chainVersions is every version of the embedded migration chain, oldest
// first. The chain is numbered sequentially from 1, so its highest version
// names the whole of it; a gap in the numbering fails the tests below.
func chainVersions() []int64 {
	versions := make([]int64, 0, reportingmigrations.Version)
	for version := int64(1); version <= reportingmigrations.Version; version++ {
		versions = append(versions, version)
	}
	return versions
}

// migrationStatusOutput is what migrate-status prints for a chain whose
// migrations are all in the given state. The command reports every migration,
// so the whole chain has to be spelled out to compare stdout exactly.
func migrationStatusOutput(state string) string {
	var out strings.Builder
	for _, version := range chainVersions() {
		fmt.Fprintf(&out, "migration %d %s\n", version, state)
	}
	return out.String()
}

// onlyLine returns the single line out holds. A token has to stand alone on
// stdout, because that is what lets a caller redirect it into a file.
func onlyLine(t *testing.T, out string) string {
	t.Helper()

	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("stdout = %q, want exactly one line", out)
	}
	return lines[0]
}

// idOfTokenHash returns the id of the row in table whose token_hash is hash,
// which is how a test finds the row a printed token belongs to. No row means
// the CLI stored something other than the token's digest.
func idOfTokenHash(t *testing.T, db storetest.DB, table, hash string) uuid.UUID {
	t.Helper()

	var id uuid.UUID
	query := fmt.Sprintf("SELECT id FROM %s WHERE token_hash = $1", table)
	if err := db.Store.Pool().QueryRow(t.Context(), query, hash).Scan(&id); err != nil {
		t.Fatalf("reading the %s row holding the hash of the printed token: %v", table, err)
	}
	return id
}

// assertTokenIsNotStored checks that the raw token reached no column of the
// tables the CLI writes. Casting a row to text covers every column of it,
// including the audit log's JSONB details.
func assertTokenIsNotStored(t *testing.T, db storetest.DB, token string) {
	t.Helper()

	for _, table := range []string{"ingest_credentials", "api_tokens", "audit_log"} {
		var rows int
		query := fmt.Sprintf("SELECT count(*) FROM %s t WHERE t::text LIKE '%%' || $1 || '%%'", table)
		if err := db.Store.Pool().QueryRow(t.Context(), query, token).Scan(&rows); err != nil {
			t.Fatalf("searching %s for the raw token: %v", table, err)
		}
		if rows != 0 {
			t.Errorf("%s holds %d row(s) carrying the raw token", table, rows)
		}
	}
}

// registryRow is the part of a projects row the registration subtests check.
type registryRow struct {
	platform, cloud, externalID string
	name                        pgtype.Text
	metadata                    string
}

// projectRow reads the registry row a create subcommand wrote. The metadata
// comes back as text, so a row that carries the empty document compares equal
// to one written from any other spelling of it.
func projectRow(t *testing.T, db storetest.DB, id uuid.UUID) registryRow {
	t.Helper()

	var row registryRow
	if err := db.Store.Pool().QueryRow(t.Context(),
		"SELECT platform, cloud, external_id, name, metadata::text FROM projects WHERE id = $1", id).
		Scan(&row.platform, &row.cloud, &row.externalID, &row.name, &row.metadata); err != nil {
		t.Fatalf("reading the projects row %s: %v", id, err)
	}
	return row
}

// auditRows counts the rows the CLI's audit entries left for one change.
func auditRows(t *testing.T, db storetest.DB, action string, objectID uuid.UUID) int {
	t.Helper()

	// The object part of an action is the table the change landed in, which is
	// what object_type holds as well.
	objectType, _, _ := strings.Cut(action, ".")

	var rows int
	if err := db.Store.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM audit_log
		 WHERE actor = 'admin-cli' AND action = $1 AND object_type = $2 AND object_id = $3`,
		action, objectType, objectID.String()).Scan(&rows); err != nil {
		t.Fatalf("counting the audit rows for %s: %v", action, err)
	}
	return rows
}

// countRows is how many rows table holds.
func countRows(t *testing.T, db storetest.DB, table string) int {
	t.Helper()

	var rows int
	if err := db.Store.Pool().QueryRow(t.Context(), "SELECT count(*) FROM "+table).Scan(&rows); err != nil {
		t.Fatalf("counting the rows of %s: %v", table, err)
	}
	return rows
}

// revokedAt reads when the row was revoked, which is NULL while it is in use.
func revokedAt(t *testing.T, db storetest.DB, table string, id uuid.UUID) pgtype.Timestamptz {
	t.Helper()

	var at pgtype.Timestamptz
	query := fmt.Sprintf("SELECT revoked_at FROM %s WHERE id = $1", table)
	if err := db.Store.Pool().QueryRow(t.Context(), query, id).Scan(&at); err != nil {
		t.Fatalf("reading revoked_at of %s %s: %v", table, id, err)
	}
	return at
}

// statusFor drives an authentication middleware with a bearer token and returns
// the status the request ends with. The wrapped handler answers 204, so
// anything else came from the middleware.
func statusFor(t *testing.T, middleware func(http.Handler) http.Handler, token string) int {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()

	middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rec, req)

	return rec.Code
}

// TestReferencePageIsCurrent holds the command line reference page of this
// binary to the tree it documents. A subcommand or a flag added here without
// the page being regenerated fails this test rather than leaving a reader with
// a tree the binary no longer has.
func TestReferencePageIsCurrent(t *testing.T) {
	text, err := refdoc.Commands(newRootCmd())
	if err != nil {
		t.Fatalf("rendering the command tree: %v", err)
	}

	refdoc.Verify(t, "../../docs/reference/command-line/tally-reporting-admin.md",
		map[string]string{"commands": text})
}
