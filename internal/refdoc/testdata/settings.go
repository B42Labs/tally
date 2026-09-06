// Package testdata holds the sources the renderers are exercised against. The
// go tool ignores this directory, so the files here are read as bytes and
// parsed rather than built.
package testdata

// Config is the fixture configuration. It carries one field per shape a
// settings table renders.
type Config struct {
	// LogLevel is the threshold, one of DEBUG | INFO.
	LogLevel string `env:"TALLY_TEST_LOG_LEVEL" envDefault:"INFO"`
	// DBURL is the connection string. It has no default because a guessed
	// database is worse than none. Supports the *_FILE convention.
	DBURL string `env:"TALLY_TEST_DB_URL"`
	// HTTPPort is the port the server listens on.
	HTTPPort int `env:"TALLY_TEST_HTTP_PORT" envDefault:"8080"`
	// DBMaxConns bounds the connection pool.
	DBMaxConns int32 `env:"TALLY_TEST_DB_MAX_CONNS" envDefault:"10"`
	// BufferMaxEvents bounds what the buffer holds before it refuses writes.
	BufferMaxEvents int64 `env:"TALLY_TEST_BUFFER_MAX_EVENTS" envDefault:"1000000"`
	// MetricsEnabled exposes the instrumentation.
	MetricsEnabled bool `env:"TALLY_TEST_METRICS_ENABLED" envDefault:"true"`
	// Clouds are the clouds the process reconciles.
	Clouds []string `env:"TALLY_TEST_CLOUDS" envSeparator:"," envDefault:"one,two"`
	// resolved is filled after the environment is read, so no variable names it.
	resolved string
}

// EnvNames is every variable the fixture configuration reads, including the
// *_FILE companion of its one secret.
var EnvNames = []string{
	"TALLY_TEST_LOG_LEVEL",
	"TALLY_TEST_DB_URL",
	"TALLY_TEST_DB_URL_FILE",
	"TALLY_TEST_HTTP_PORT",
	"TALLY_TEST_DB_MAX_CONNS",
	"TALLY_TEST_BUFFER_MAX_EVENTS",
	"TALLY_TEST_METRICS_ENABLED",
	"TALLY_TEST_CLOUDS",
}
