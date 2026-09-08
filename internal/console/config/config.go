// Package config parses the demo console's environment configuration. The
// console reads the Reporting API over HTTP and the engine database directly,
// so it needs an address and a credential for each.
//
// Configuration comes from the environment alone, prefixed TALLY_. Secrets also
// accept the *_FILE convention, which is how a Kubernetes Secret volume reaches
// the process without the value appearing in a pod spec.
//
// The normative specification is roadmap/00-conventions.md section 8.
package config

import (
	"fmt"
	"log/slog"
	"net"
	"net/url"

	"github.com/caarlos0/env/v11"

	"github.com/b42labs/tally/internal/core/envsecret"
)

// The variables this package reads. They are named here because the errors
// quote them: an operator gets the variable to fix, not a Go field name.
const (
	envLogLevel     = "TALLY_LOG_LEVEL"
	envHTTPPort     = "TALLY_CONSOLE_HTTP_PORT"
	envReportingURL = "TALLY_CONSOLE_REPORTING_URL"
	envAPIToken     = "TALLY_CONSOLE_API_TOKEN"
	envCAFile       = "TALLY_CONSOLE_CA_FILE"
	envEngineDBURL  = "TALLY_CONSOLE_ENGINE_DB_URL"
)

// EnvNames is every variable this package reads, including the *_FILE
// companions of the secrets. Tests blank all of them, so a value in the
// developer's shell never reaches the code under test.
var EnvNames = []string{
	envLogLevel,
	envHTTPPort,
	envReportingURL,
	envAPIToken,
	envAPIToken + envsecret.Suffix,
	envCAFile,
	envEngineDBURL,
	envEngineDBURL + envsecret.Suffix,
}

// logLevels maps the accepted values of TALLY_LOG_LEVEL to their slog level.
// The match is exact: a lower-case "info" is a typo, and silently accepting it
// would hide the mistake behind a working service.
var logLevels = map[string]slog.Level{
	"DEBUG": slog.LevelDebug,
	"INFO":  slog.LevelInfo,
	"WARN":  slog.LevelWarn,
	"ERROR": slog.LevelError,
}

// Config is the demo console's resolved configuration. Every field is final by
// the time Load returns: file-backed secrets hold their content, and the values
// the console cannot run without are present.
type Config struct {
	// LogLevel is the slog threshold, one of DEBUG, INFO, WARN, or ERROR.
	LogLevel string `env:"TALLY_LOG_LEVEL" envDefault:"INFO"`
	// HTTPPort is the port the console listens on, bound to the loopback
	// address only. The default is 8095 because 8090 and 8091 are taken by the
	// simulator compose stack.
	HTTPPort int `env:"TALLY_CONSOLE_HTTP_PORT" envDefault:"8095"`
	// ReportingURL is the base URL of the Reporting API without the /api/v1
	// suffix. It has to be set, and it has to be https unless its host is
	// loopback, because the API token rides on every call.
	ReportingURL string `env:"TALLY_CONSOLE_REPORTING_URL"`
	// APIToken is the bearer token the console reads the Reporting API with. It
	// has to carry the admin role, because the dead-letter list the overview
	// reads is admin-only. It has to be set. Supports the *_FILE convention.
	APIToken string `env:"TALLY_CONSOLE_API_TOKEN"`
	// CAFile is a PEM file holding the CA that signed the Reporting API's
	// certificate. Empty trusts the system store.
	CAFile string `env:"TALLY_CONSOLE_CA_FILE"`
	// EngineDBURL is the PostgreSQL connection string of the engine database,
	// opened for reads only. It has to be set. Supports the *_FILE convention.
	EngineDBURL string `env:"TALLY_CONSOLE_ENGINE_DB_URL"`
}

// Load reads the environment, resolves the file-backed secrets, and checks
// every value the console needs. The console serves one thing and reads from
// two places, so what it requires does not depend on the caller: a
// configuration Load accepts is one the server can start on.
func Load() (Config, error) {
	cfg, err := env.ParseAs[Config]()
	if err != nil {
		return Config{}, fmt.Errorf("parsing the environment: %w", err)
	}

	if cfg.APIToken, err = envsecret.Resolve(envAPIToken, cfg.APIToken); err != nil {
		return Config{}, err
	}
	if cfg.EngineDBURL, err = envsecret.Resolve(envEngineDBURL, cfg.EngineDBURL); err != nil {
		return Config{}, err
	}

	if _, ok := logLevels[cfg.LogLevel]; !ok {
		return Config{}, fmt.Errorf("%s: %q must be DEBUG, INFO, WARN, or ERROR", envLogLevel, cfg.LogLevel)
	}
	if cfg.HTTPPort < 1 || cfg.HTTPPort > 65535 {
		return Config{}, fmt.Errorf("%s: %d must be between 1 and 65535", envHTTPPort, cfg.HTTPPort)
	}

	if cfg.ReportingURL == "" {
		return Config{}, fmt.Errorf("%s: must be set", envReportingURL)
	}
	parsed, err := url.Parse(cfg.ReportingURL)
	if err != nil {
		return Config{}, fmt.Errorf("%s: %q is not a URL: %w", envReportingURL, cfg.ReportingURL, err)
	}
	// A relative reference parses without complaint, and a client built on one
	// would fail per request instead of at startup.
	if parsed.Scheme == "" || parsed.Hostname() == "" {
		return Config{}, fmt.Errorf("%s: %q is not a URL", envReportingURL, cfg.ReportingURL)
	}
	// The token rides on every call, so a plaintext base URL puts an admin
	// credential on the wire in the clear. Loopback is exempt: that is where a
	// test's server and a port-forwarded cluster listen. This rule lives here
	// and only here, so the client the console builds takes the URL as given.
	if parsed.Scheme != "https" && !isLoopback(parsed.Hostname()) {
		return Config{}, fmt.Errorf("%s: %q is not https, so the token in %s would travel in cleartext", envReportingURL, cfg.ReportingURL, envAPIToken)
	}

	if cfg.APIToken == "" {
		return Config{}, fmt.Errorf("%s: must be set", envAPIToken)
	}
	if cfg.EngineDBURL == "" {
		return Config{}, fmt.Errorf("%s: must be set", envEngineDBURL)
	}

	return cfg, nil
}

// isLoopback reports whether a host names the machine the console runs on. The
// name is taken as well as the address, because that is what a port-forward is
// reached under.
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// SlogLevel is the slog level that LogLevel names. A Config that never went
// through Load, and so carries an unchecked level, logs at info.
func (c Config) SlogLevel() slog.Level {
	if level, ok := logLevels[c.LogLevel]; ok {
		return level
	}
	return slog.LevelInfo
}
