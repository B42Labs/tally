package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/b42labs/tally/internal/engine/store"
	"github.com/b42labs/tally/internal/engine/store/storetest"
)

// The SQLSTATEs the role's tests expect.
const (
	insufficientPrivilege = "42501"
	invalidPassword       = "28P01"
	undefinedObject       = "42704"
)

// Each top-level test below starts a container of its own: roles live in the
// server rather than in one database, so a role one test creates or drops would
// otherwise reach the next.

// TestEnsureConsoleRole is the deployment's story: the role step runs on every
// deploy, creates the login role the first time and sets its password every
// time, and what connects as the role reads the engine tables and changes
// nothing. The subtests share one server and run in order.
func TestEnsureConsoleRole(t *testing.T) {
	const (
		firstPassword  = "console-first-password"
		secondPassword = "console-second-password"
	)

	db := storetest.NewDB(t)

	t.Run("the empty password is refused before anything is sent", func(t *testing.T) {
		created, err := db.Store.EnsureConsoleRole(t.Context(), "")
		if err == nil {
			t.Fatalf("EnsureConsoleRole() = %t, want an error", created)
		}
		if want := "the password of the role tally_console is empty"; err.Error() != want {
			t.Errorf("EnsureConsoleRole() error = %q, want %q", err, want)
		}
		if consoleRoleExists(t, db.Store) {
			t.Error("pg_roles holds tally_console after the refusal, want nothing created")
		}
	})

	t.Run("a connection that may not create roles is refused", func(t *testing.T) {
		const (
			plainRole     = "console_role_plain"
			plainPassword = "console-plain-password"
		)
		if _, err := db.Store.Pool().Exec(t.Context(),
			"CREATE ROLE "+plainRole+" LOGIN PASSWORD '"+plainPassword+"'"); err != nil {
			t.Fatalf("creating the login role without CREATEROLE: %v", err)
		}
		plain, err := store.New(t.Context(), storetest.URLAs(t, db.URL, plainRole, plainPassword))
		if err != nil {
			t.Fatalf("New() error = %v, want nil", err)
		}
		defer plain.Close()

		_, err = plain.EnsureConsoleRole(t.Context(), firstPassword)
		assertRoleError(t, err, "creating the role tally_console", insufficientPrivilege, firstPassword)
		if consoleRoleExists(t, db.Store) {
			t.Error("pg_roles holds tally_console after the refusal, want nothing created")
		}
	})

	var firstVerifier string

	t.Run("the first call creates the role, which connects and reads", func(t *testing.T) {
		created, err := db.Store.EnsureConsoleRole(t.Context(), firstPassword)
		if err != nil {
			t.Fatalf("EnsureConsoleRole() error = %v, want nil", err)
		}
		if !created {
			t.Error("EnsureConsoleRole() = false, want true for the role it created")
		}

		assertReadsRuns(t, connectAs(t, db.URL, firstPassword))
		firstVerifier = storedPassword(t, db.Store)
	})

	t.Run("a second call with the same password stores a new verifier", func(t *testing.T) {
		created, err := db.Store.EnsureConsoleRole(t.Context(), firstPassword)
		if err != nil {
			t.Fatalf("EnsureConsoleRole() error = %v, want nil", err)
		}
		if created {
			t.Error("EnsureConsoleRole() = true, want false for the role that was there")
		}

		// The cleartext never reaches the server: what it stores is a verifier,
		// and each call draws the salt anew.
		second := storedPassword(t, db.Store)
		for _, verifier := range []string{firstVerifier, second} {
			if !strings.HasPrefix(verifier, "SCRAM-SHA-256$4096:") {
				t.Errorf("rolpassword = %q, want a SCRAM-SHA-256 verifier of 4096 iterations", verifier)
			}
		}
		if second == firstVerifier {
			t.Errorf("rolpassword = %q after both calls, want a fresh salt on the second", second)
		}
		assertReadsRuns(t, connectAs(t, db.URL, firstPassword))
	})

	t.Run("a call with another password replaces the first", func(t *testing.T) {
		created, err := db.Store.EnsureConsoleRole(t.Context(), secondPassword)
		if err != nil {
			t.Fatalf("EnsureConsoleRole() error = %v, want nil", err)
		}
		if created {
			t.Error("EnsureConsoleRole() = true, want false for the role that was there")
		}

		old, err := pgx.Connect(t.Context(), storetest.URLAs(t, db.URL, store.ConsoleRole, firstPassword))
		if err == nil {
			_ = old.Close(context.Background())
			t.Fatal("the first password still connects, want it refused once the second is set")
		}
		if got := sqlState(err); got != invalidPassword {
			t.Errorf("connecting with the first password: %v, want SQLSTATE %s", err, invalidPassword)
		}
		assertReadsRuns(t, connectAs(t, db.URL, secondPassword))
	})

	t.Run("the role reads and writes nothing else", func(t *testing.T) {
		conn := connectAs(t, db.URL, secondPassword)

		for _, stmt := range []string{
			`INSERT INTO runs (period_from, period_to, status) VALUES ('2026-03-01Z', '2026-04-01Z', 'completed')`,
			`UPDATE runs SET status = status`,
			`DELETE FROM runs`,
			`TRUNCATE runs`,
			`SELECT count(*) FROM goose_db_version_engine`,
			`CREATE TABLE console_probe (id int)`,
		} {
			_, err := conn.Exec(t.Context(), stmt)
			if got := sqlState(err); got != insufficientPrivilege {
				t.Errorf("%s as tally_console: %v, want SQLSTATE %s", stmt, err, insufficientPrivilege)
			}
		}
	})
}

// TestEnsureConsoleRoleOverARoleCreatedBeforehand is the role someone created
// by hand, or a call that stopped after its first statement. Whatever it lacks
// of a login, an unexpired password and a membership that inherits, one call
// completes. The subtests share one server and drop the role after each.
func TestEnsureConsoleRoleOverARoleCreatedBeforehand(t *testing.T) {
	const password = "console-beforehand-password"

	db := storetest.NewDB(t)

	for _, tc := range []struct {
		name  string
		setup []string
	}{
		{"a role without login", []string{"CREATE ROLE tally_console NOLOGIN"}},
		{"a role that does not inherit", []string{"CREATE ROLE tally_console NOLOGIN NOINHERIT"}},
		{"a role whose password expired", []string{"CREATE ROLE tally_console LOGIN VALID UNTIL '2000-01-01Z'"}},
		{"a membership granted without inheritance", []string{
			"CREATE ROLE tally_console NOLOGIN",
			"GRANT tally_console_reader TO tally_console WITH INHERIT FALSE",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dropAfterwards(t, db.Store, store.ConsoleRole)
			for _, stmt := range tc.setup {
				if _, err := db.Store.Pool().Exec(t.Context(), stmt); err != nil {
					t.Fatalf("%s: %v", stmt, err)
				}
			}

			created, err := db.Store.EnsureConsoleRole(t.Context(), password)
			if err != nil {
				t.Fatalf("EnsureConsoleRole() error = %v, want nil", err)
			}
			if created {
				t.Error("EnsureConsoleRole() = true, want false for the role that was there")
			}
			assertReadsRuns(t, connectAs(t, db.URL, password))
		})
	}
}

// TestEnsureConsoleRoleRefusesARoleThatHoldsMore is a role made beforehand
// with more than the console needs, one another role is a member of or
// administers, or a reader role that holds more than migration 3 gives it. A
// login on it would read or change more than the engine tables, or let that
// other role read them, so it is refused before it gets a login or a
// password. The subtests share one server, drop the roles after each and undo
// what they changed beyond that: the reader role, which nothing drops, and the
// objects a role owns, which would keep it from being dropped.
func TestEnsureConsoleRoleRefusesARoleThatHoldsMore(t *testing.T) {
	const (
		password = "console-refused-password"
		other    = "console_role_other"
	)

	db := storetest.NewDB(t)

	for _, tc := range []struct {
		name  string
		setup []string
		undo  []string
	}{
		{"a superuser", []string{"CREATE ROLE tally_console SUPERUSER"}, nil},
		{"a role that may create roles", []string{"CREATE ROLE tally_console CREATEROLE"}, nil},
		{"a role that may create databases", []string{"CREATE ROLE tally_console CREATEDB"}, nil},
		{"a role that may replicate", []string{"CREATE ROLE tally_console REPLICATION"}, nil},
		{"a role that bypasses row security", []string{"CREATE ROLE tally_console BYPASSRLS"}, nil},
		{"a member of a role that writes", []string{
			"CREATE ROLE tally_console",
			"GRANT pg_write_all_data TO tally_console",
		}, nil},
		{"a role another role is a member of", []string{
			"CREATE ROLE tally_console",
			"CREATE ROLE " + other,
			"GRANT tally_console TO " + other,
		}, nil},
		{"a role another role administers", []string{
			"CREATE ROLE tally_console",
			"CREATE ROLE " + other,
			"GRANT tally_console TO " + other + " WITH ADMIN OPTION",
		}, nil},
		{"a reader role another role is a member of", []string{
			"CREATE ROLE " + other,
			"GRANT tally_console_reader TO " + other,
		}, nil},
		{"a reader role another role administers", []string{
			"CREATE ROLE " + other,
			"GRANT tally_console_reader TO " + other + " WITH ADMIN OPTION",
		}, nil},
		{"a role that administers the reader role", []string{
			"CREATE ROLE tally_console",
			"GRANT tally_console_reader TO tally_console WITH ADMIN OPTION",
		}, nil},
		{"a role that owns a table", []string{
			"CREATE ROLE tally_console",
			"CREATE TABLE console_owned (id int)",
			"ALTER TABLE console_owned OWNER TO tally_console",
		}, []string{
			"DROP TABLE IF EXISTS console_owned",
		}},
		{"a reader role that is a superuser", []string{
			"ALTER ROLE tally_console_reader SUPERUSER",
		}, []string{
			"ALTER ROLE tally_console_reader NOSUPERUSER",
		}},
		{"a reader role that logs in", []string{
			"ALTER ROLE tally_console_reader LOGIN",
		}, []string{
			"ALTER ROLE tally_console_reader NOLOGIN",
		}},
		{"a reader role that is a member of a role that writes", []string{
			"GRANT pg_write_all_data TO tally_console_reader",
		}, []string{
			"REVOKE pg_write_all_data FROM tally_console_reader",
		}},
		{"a reader role that owns a schema", []string{
			"CREATE SCHEMA tally_console AUTHORIZATION tally_console_reader",
		}, []string{
			"DROP SCHEMA IF EXISTS tally_console",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dropAfterwards(t, db.Store, store.ConsoleRole, other)
			t.Cleanup(func() {
				for _, stmt := range tc.undo {
					if _, err := db.Store.Pool().Exec(context.Background(), stmt); err != nil {
						t.Errorf("%s: %v", stmt, err)
					}
				}
			})
			for _, stmt := range tc.setup {
				if _, err := db.Store.Pool().Exec(t.Context(), stmt); err != nil {
					t.Fatalf("%s: %v", stmt, err)
				}
			}

			_, err := db.Store.EnsureConsoleRole(t.Context(), password)
			if err == nil {
				t.Fatal("EnsureConsoleRole() error = nil, want the role refused")
			}
			if want := "the role tally_console holds an attribute or a membership beyond tally_console_reader"; !strings.Contains(err.Error(), want) {
				t.Errorf("EnsureConsoleRole() error = %q, want it to name %q", err, want)
			}
			assertNoSecret(t, err, password)

			var canLogin, hasPassword bool
			if err := db.Store.Pool().QueryRow(t.Context(),
				`SELECT rolcanlogin, rolpassword IS NOT NULL FROM pg_authid WHERE rolname = $1`,
				store.ConsoleRole).Scan(&canLogin, &hasPassword); err != nil {
				t.Fatalf("reading pg_authid: %v", err)
			}
			if canLogin || hasPassword {
				t.Errorf("tally_console has login %t and a password %t after the refusal, want neither", canLogin, hasPassword)
			}
		})
	}
}

// TestEnsureConsoleRoleAgainstAConcurrentCreation is two role steps reaching
// the CREATE ROLE together: two Jobs, or a Job and a hand-run make migrate.
// The other session is held uncommitted rather than raced, so the outcome is
// the same on every run: the call under test walks past the catalog lookup,
// which cannot see an uncommitted role, waits on the unique index over
// pg_authid.rolname and fails on it with unique_violation once the other
// session commits. It has to take that role as its own.
func TestEnsureConsoleRoleAgainstAConcurrentCreation(t *testing.T) {
	const password = "console-concurrent-password"

	db := storetest.NewDB(t)

	other, err := pgx.Connect(t.Context(), db.URL)
	if err != nil {
		t.Fatalf("connecting the other session: %v", err)
	}
	defer func() { _ = other.Close(context.Background()) }()

	tx, err := other.Begin(t.Context())
	if err != nil {
		t.Fatalf("opening the other session's transaction: %v", err)
	}
	if _, err := tx.Exec(t.Context(), "CREATE ROLE tally_console NOLOGIN"); err != nil {
		t.Fatalf("creating the role in the other session: %v", err)
	}

	var created bool
	ensured := make(chan error, 1)
	go func() {
		var err error
		created, err = db.Store.EnsureConsoleRole(t.Context(), password)
		ensured <- err
	}()

	waitForTheBlockedRoleCreation(t, db.Store.Pool(), ensured)

	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("committing the other session: %v", err)
	}
	if err := <-ensured; err != nil {
		t.Fatalf("EnsureConsoleRole() error = %v, want the role the other session created taken as its own", err)
	}
	if created {
		t.Error("EnsureConsoleRole() = true, want false for the role the other session created")
	}
	assertReadsRuns(t, connectAs(t, db.URL, password))
}

// TestEnsureConsoleRoleWithoutTheReaderRole is a server where someone dropped
// the group role by hand: the login role is created and given its password,
// and the membership that carries every read fails naming the step.
func TestEnsureConsoleRoleWithoutTheReaderRole(t *testing.T) {
	const password = "console-no-reader-password"

	db := storetest.NewDB(t)
	for _, stmt := range []string{
		"DROP OWNED BY tally_console_reader",
		"DROP ROLE tally_console_reader",
	} {
		if _, err := db.Store.Pool().Exec(t.Context(), stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	_, err := db.Store.EnsureConsoleRole(t.Context(), password)
	assertRoleError(t, err, "granting tally_console_reader to tally_console", undefinedObject, password)
}

// TestEnsureConsoleRoleOnAStoppedDatabase is a database that does not answer:
// the first statement fails on the dial, under its own step.
func TestEnsureConsoleRoleOnAStoppedDatabase(t *testing.T) {
	const password = "console-stopped-password"

	db := storetest.NewDB(t)
	// How long the database gets to shut down cleanly.
	stopTimeout := 10 * time.Second
	if err := db.Container.Stop(t.Context(), &stopTimeout); err != nil {
		t.Fatalf("stopping the database container: %v", err)
	}
	stopped, err := store.New(t.Context(), db.URL)
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	defer stopped.Close()

	_, err = stopped.EnsureConsoleRole(t.Context(), password)
	if err == nil {
		t.Fatal("EnsureConsoleRole() error = nil, want the dial error")
	}
	if prefix := "creating the role tally_console: "; !strings.HasPrefix(err.Error(), prefix) {
		t.Errorf("EnsureConsoleRole() error = %q, want it to start with %q", err, prefix)
	}
	assertNoSecret(t, err, password)
}

// assertRoleError checks that err names the step that failed, carries the
// server's SQLSTATE, and gives nothing of the password away.
func assertRoleError(t *testing.T, err error, step, code, password string) {
	t.Helper()

	if err == nil {
		t.Fatalf("EnsureConsoleRole() error = nil, want it to fail %s", step)
	}
	if !strings.Contains(err.Error(), step) {
		t.Errorf("EnsureConsoleRole() error = %q, want it to name %q", err, step)
	}
	if got := sqlState(err); got != code {
		t.Errorf("EnsureConsoleRole() error = %v, want SQLSTATE %s", err, code)
	}
	assertNoSecret(t, err, password)
}

// assertNoSecret fails when err carries the password or the verifier, which
// would put either in whatever log the error is written to.
func assertNoSecret(t *testing.T, err error, password string) {
	t.Helper()

	for _, secret := range []string{password, "SCRAM-SHA-256"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("EnsureConsoleRole() error = %q, want it free of %q", err, secret)
		}
	}
}

// assertReadsRuns checks that conn may read the runs table.
func assertReadsRuns(t *testing.T, conn *pgx.Conn) {
	t.Helper()

	var runs int
	if err := conn.QueryRow(t.Context(), "SELECT count(*) FROM runs").Scan(&runs); err != nil {
		t.Errorf("reading runs as tally_console: %v, want the count", err)
	}
}

// connectAs opens a connection as tally_console with password, through the
// container's mapped port, and closes it when the test ends.
func connectAs(t *testing.T, dbURL, password string) *pgx.Conn {
	t.Helper()

	conn, err := pgx.Connect(t.Context(), storetest.URLAs(t, dbURL, store.ConsoleRole, password))
	if err != nil {
		t.Fatalf("connecting as tally_console: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// dropAfterwards drops roles when the test ends, so the next subtest on the
// same server starts without them. It is registered before the connections
// the test opens as one of them, which are therefore closed first.
func dropAfterwards(t *testing.T, s *store.Store, roles ...string) {
	t.Helper()

	t.Cleanup(func() {
		for _, role := range roles {
			if _, err := s.Pool().Exec(context.Background(), "DROP ROLE IF EXISTS "+role); err != nil {
				t.Errorf("dropping the role %s: %v", role, err)
			}
		}
	})
}

// consoleRoleExists reports whether pg_roles holds tally_console.
func consoleRoleExists(t *testing.T, s *store.Store) bool {
	t.Helper()

	var exists bool
	if err := s.Pool().QueryRow(t.Context(),
		`SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, store.ConsoleRole).Scan(&exists); err != nil {
		t.Fatalf("querying pg_roles: %v", err)
	}
	return exists
}

// storedPassword is what the server keeps as tally_console's password.
func storedPassword(t *testing.T, s *store.Store) string {
	t.Helper()

	var verifier string
	if err := s.Pool().QueryRow(t.Context(),
		`SELECT rolpassword FROM pg_authid WHERE rolname = $1`, store.ConsoleRole).Scan(&verifier); err != nil {
		t.Fatalf("reading pg_authid: %v", err)
	}
	return verifier
}

// sqlState is the SQLSTATE err carries, or the empty string when it is not a
// Postgres error.
func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}
