package store

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

const (
	// ConsoleRole is the login role the console reads the engine database as.
	ConsoleRole = "tally_console"
	// ConsoleReaderRole is the group role engine migration 3 creates and grants
	// SELECT on the engine tables. ConsoleRole reads through its membership.
	ConsoleReaderRole = "tally_console_reader"
)

// The SQLSTATEs CREATE ROLE reports for a role that is already there: the
// catalog lookup finds it, or a concurrent creation reaches the unique index
// over pg_authid first.
const (
	duplicateObject = "42710"
	uniqueViolation = "23505"
)

// foreignConsoleRoleQuery reports whether the role $1 holds more than
// membership in $2, whether $2 holds an attribute, a login or a membership,
// whether a role other than the connection's own is a member of $1 or $2, $1's
// own membership in $2 without ADMIN OPTION aside, or whether $1 or $2 owns an
// object in any database on the server. A grant with ADMIN OPTION is a
// membership as well, so the membership clause covers the roles that
// administer either.
const foreignConsoleRoleQuery = `
SELECT r.rolsuper OR r.rolcreaterole OR r.rolcreatedb OR r.rolreplication OR r.rolbypassrls
    OR EXISTS (SELECT 1 FROM pg_auth_members m JOIN pg_roles g ON g.oid = m.roleid
               WHERE m.member = r.oid AND g.rolname <> $2)
    OR EXISTS (SELECT 1 FROM pg_roles g WHERE g.rolname = $2
               AND (g.rolsuper OR g.rolcreaterole OR g.rolcreatedb OR g.rolreplication
                    OR g.rolbypassrls OR g.rolcanlogin
                    OR EXISTS (SELECT 1 FROM pg_auth_members m WHERE m.member = g.oid)))
    OR EXISTS (SELECT 1 FROM pg_auth_members m
               JOIN pg_roles g ON g.oid = m.roleid
               JOIN pg_roles a ON a.oid = m.member
               WHERE g.rolname IN ($1, $2) AND a.rolname <> current_user
                 AND (m.member <> r.oid OR m.admin_option))
    OR EXISTS (SELECT 1 FROM pg_shdepend d JOIN pg_roles o ON o.oid = d.refobjid
               WHERE d.refclassid = 'pg_authid'::regclass AND d.deptype = 'o'
                 AND o.rolname IN ($1, $2))
FROM pg_roles r WHERE r.rolname = $1`

// The parameters of the verifier: PostgreSQL's own default iteration count, and
// the salt length the server draws when it computes a verifier itself.
const (
	scramIterations = 4096
	scramSaltLen    = 16
)

// EnsureConsoleRole creates ConsoleRole as a member of ConsoleReaderRole, or
// sets its password when it is there already, and reports whether it created
// the role. The connection has to be a superuser, or hold CREATEROLE and ADMIN
// OPTION on both roles: PostgreSQL 16 lets a role alter, and grant membership
// in, only the roles it administers. A role with CREATEROLE administers the
// roles it creates itself, so it needs the option granted only on a role
// someone else created.
//
// A role that is there already gets a login only when it is what this function
// creates: no attribute such as SUPERUSER or CREATEDB, no membership but the
// one in ConsoleReaderRole, and no member but the connection's own role.
// ConsoleReaderRole has to be what migration 3 creates: no attribute, no login
// and no membership, since ConsoleRole inherits its memberships and takes on
// its attributes through SET ROLE, and no member but the connection's own role
// and ConsoleRole, the latter without ADMIN OPTION. A login as the reader role
// or another member of either reads what the console reads, and one holding
// ADMIN OPTION can make any role such a member. Neither role may own an object
// in any database on the server: ConsoleRole holds the owner's rights on what
// either owns, writing and dropping included. Anything else is refused before
// the password is set. The call itself sets LOGIN, VALID UNTIL 'infinity' and
// a membership that inherits, so a role made NOLOGIN, NOINHERIT or with an
// expiry works afterwards.
//
// Every call sets the password, so the deployment's secret is what decides it,
// and a password someone set by hand is overwritten by the next call. The
// password reaches the server as a SCRAM-SHA-256 verifier with a fresh salt, so
// a server that logs DDL logs the verifier and never the cleartext. The caller
// holds the password to printable ASCII without spaces, which is what lets the
// verifier skip SASLprep.
//
// The statements run one by one and outside a transaction. A call that stops
// after the CREATE ROLE, a refusal included, leaves a role without login and
// password, one that stops after the ALTER ROLE a role that connects and reads
// nothing, and the next call completes either. Two calls at the same moment
// can fail the ALTER ROLE with "tuple concurrently updated", and calling again
// succeeds.
//
// No error carries the password or the verifier: a *pgconn.PgError holds no
// statement text, and this function adds none.
func (s *Store) EnsureConsoleRole(ctx context.Context, password string) (created bool, err error) {
	if password == "" {
		return false, errors.New("the password of the role " + ConsoleRole + " is empty")
	}

	salt := make([]byte, scramSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return false, fmt.Errorf("drawing the salt of the password: %w", err)
	}
	verifier, err := scramVerifier(password, salt)
	if err != nil {
		return false, err
	}

	created = true
	if _, err := s.pool.Exec(ctx, "CREATE ROLE "+ConsoleRole); err != nil {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || (pgErr.Code != duplicateObject && pgErr.Code != uniqueViolation) {
			return false, fmt.Errorf("creating the role %s: %w", ConsoleRole, err)
		}
		created = false
	}
	var foreign bool
	if err := s.pool.QueryRow(ctx, foreignConsoleRoleQuery, ConsoleRole, ConsoleReaderRole).Scan(&foreign); err != nil {
		return false, fmt.Errorf("checking the role %s: %w", ConsoleRole, err)
	}
	if foreign {
		return false, fmt.Errorf("the role %s holds an attribute or a membership beyond %[2]s, "+
			"%[2]s holds an attribute, a login or a membership, "+
			"another role is a member of either, or either owns an object: "+
			"revoke or reassign that and run again", ConsoleRole, ConsoleReaderRole)
	}
	// Role DDL takes no bind parameter, so the verifier is written in as a
	// literal. Its alphabet is base64's, which holds no quote and no backslash.
	if _, err := s.pool.Exec(ctx, "ALTER ROLE "+ConsoleRole+
		" WITH LOGIN VALID UNTIL 'infinity' PASSWORD '"+verifier+"'"); err != nil {
		return false, fmt.Errorf("setting the password of the role %s: %w", ConsoleRole, err)
	}
	// On PostgreSQL 16 a membership inherits by its own option, which an
	// existing grant keeps, rather than by the member's INHERIT attribute.
	if _, err := s.pool.Exec(ctx, "GRANT "+ConsoleReaderRole+" TO "+ConsoleRole+" WITH INHERIT TRUE"); err != nil {
		return false, fmt.Errorf("granting %s to %s: %w", ConsoleReaderRole, ConsoleRole, err)
	}
	return created, nil
}

// scramVerifier is the SCRAM-SHA-256 verifier of password under salt, in the
// form PostgreSQL stores in pg_authid.rolpassword and accepts as a PASSWORD
// value as it stands (RFC 5802 section 3, RFC 7677).
func scramVerifier(password string, salt []byte) (string, error) {
	salted, err := pbkdf2.Key(sha256.New, password, salt, scramIterations, sha256.Size)
	if err != nil {
		return "", fmt.Errorf("deriving the salted password: %w", err)
	}
	clientKey := hmacSHA256(salted, "Client Key")
	storedKey := sha256.Sum256(clientKey)
	serverKey := hmacSHA256(salted, "Server Key")

	b64 := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s",
		scramIterations, b64(salt), b64(storedKey[:]), b64(serverKey)), nil
}

// hmacSHA256 is HMAC-SHA-256 of message under key.
func hmacSHA256(key []byte, message string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(message))
	return mac.Sum(nil)
}
