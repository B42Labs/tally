// Package storetest hands integration tests a TimescaleDB container with the
// reporting migration chain applied. Every package that talks to the reporting
// database tests against a real database rather than a mock, so they all start
// from this helper instead of repeating the container setup.
//
// The normative specification is roadmap/00-conventions.md section 9.
package storetest

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"testing"

	"github.com/moby/moby/api/types/network"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/b42labs/tally/internal/reporting/store"
)

// The container the tests run against. It is the image the dev stack uses, so
// tests meet the same TimescaleDB version as a developer's cluster.
const (
	image = "timescale/timescaledb:latest-pg16"
	// ContainerPort is where Postgres listens inside the container. It is
	// exported because a test that follows the container across a restart has to
	// ask for the host port it is published on.
	ContainerPort = "5432/tcp"
)

// The credentials of the throwaway database. They are hard-coded because the
// container is reachable only through its ephemeral host port and lives for the
// duration of one test.
const (
	dbUser     = "tally"
	dbPassword = "tally-test-password"
	dbName     = "tally_reporting"
)

// poolMaxConns bounds the pool a test works through. A handful is plenty for
// one test binary and keeps the container's default max_connections free for
// the pools the tests open themselves.
const poolMaxConns = 4

// DB is a migrated database in its own container.
type DB struct {
	// Store is a pool connected to the database.
	Store *store.Store
	// URL is the connection string the pool was opened with. Restarting the
	// container remaps the host port, so a test that does must build a fresh URL
	// from Container rather than reuse this one.
	URL string
	// Container runs the database. Tests that need the database to disappear,
	// such as the readiness tests, stop and start it.
	Container testcontainers.Container
}

// NewDB starts a TimescaleDB container, waits until it accepts connections,
// applies the migration chain, and opens a pool on the result. The pool and the
// container are torn down when the test ends.
func NewDB(t *testing.T) DB {
	t.Helper()

	ctx := context.Background()
	container, err := testcontainers.Run(ctx, image,
		testcontainers.WithEnv(map[string]string{
			"POSTGRES_USER":     dbUser,
			"POSTGRES_PASSWORD": dbPassword,
			"POSTGRES_DB":       dbName,
		}),
		testcontainers.WithExposedPorts(ContainerPort),
		// Waiting for a query to succeed rather than for a log line covers the
		// initdb phase, during which the server is up but reachable only over its
		// unix socket.
		testcontainers.WithWaitStrategy(wait.ForSQL(ContainerPort, "pgx", func(host string, port network.Port) string {
			return dbURL(host, port.Port())
		})),
	)
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Errorf("terminating the database container: %v", err)
		}
	})
	if err != nil {
		t.Fatalf("starting the database container: %v", err)
	}

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("reading the container host: %v", err)
	}
	port, err := container.MappedPort(ctx, ContainerPort)
	if err != nil {
		t.Fatalf("reading the mapped database port: %v", err)
	}
	url := dbURL(host, port.Port())

	if _, err := store.Migrate(ctx, url); err != nil {
		t.Fatalf("migrating the test database: %v", err)
	}

	s, err := store.New(ctx, url, poolMaxConns)
	if err != nil {
		t.Fatalf("opening the test database pool: %v", err)
	}
	t.Cleanup(s.Close)

	return DB{Store: s, URL: url, Container: container}
}

// NewSiblingDB creates an empty database next to the migrated one and returns
// its URL. It inherits the timescaledb extension from template1, where the
// image installs it, so the hypertable statements of the chain work there too.
//
// It is what a test that needs an unmigrated database starts from: restarting
// the container would move the published port, this only adds a database.
func (d DB) NewSiblingDB(t *testing.T, name string) string {
	t.Helper()

	if _, err := d.Store.Pool().Exec(t.Context(), "CREATE DATABASE "+name); err != nil {
		t.Fatalf("creating database %s: %v", name, err)
	}
	parsed, err := url.Parse(d.URL)
	if err != nil {
		t.Fatalf("parsing the database url: %v", err)
	}
	parsed.Path = "/" + name
	return parsed.String()
}

// dbURL is the connection string of the containerized database at host:port.
func dbURL(host, port string) string {
	return fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable",
		dbUser, dbPassword, net.JoinHostPort(host, port), dbName)
}
