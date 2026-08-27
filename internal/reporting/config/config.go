// Package config parses the Reporting API's environment configuration. The HTTP
// server and the admin CLI load the same Config and then run the validation
// gate that fits them, so the CLI is never blocked by a setting only a
// listening server needs.
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
	"os"
	"slices"

	"github.com/caarlos0/env/v11"

	"github.com/b42labs/tally/internal/core/envsecret"
	"github.com/b42labs/tally/internal/core/project"
)

// The variables this package reads. They are named here because the errors
// quote them: an operator gets the variable to fix, not a Go field name.
const (
	envLogLevel           = "TALLY_LOG_LEVEL"
	envHTTPPort           = "TALLY_REPORTING_HTTP_PORT"
	envDBURL              = "TALLY_REPORTING_DB_URL"
	envDBMaxConns         = "TALLY_REPORTING_DB_MAX_CONNS"
	envAuthMode           = "TALLY_REPORTING_AUTH_MODE"
	envInternalToken      = "TALLY_REPORTING_INTERNAL_TOKEN"
	envUnhealthyThreshold = "TALLY_REPORTING_UNHEALTHY_THRESHOLD_S"
	envOIDCJWKSURL        = "TALLY_REPORTING_OIDC_JWKS_URL"
	envRequireSizeSchema  = "TALLY_INGEST_REQUIRE_SIZE_SCHEMA"
	envAttributingTypes   = "TALLY_REPORTING_ATTRIBUTING_RELATION_TYPES"
	envCloudsConfig       = "TALLY_REPORTING_CLOUDS_CONFIG"
	envMetricsEnabled     = "TALLY_METRICS_ENABLED"
	envMetricsRefresh     = "TALLY_REPORTING_METRICS_REFRESH_S"
)

// EnvNames is every variable this package reads, including the *_FILE
// companions of the secrets. Tests blank all of them, so a value in the
// developer's shell never reaches the code under test.
var EnvNames = []string{
	envLogLevel,
	envHTTPPort,
	envDBURL,
	envDBURL + envsecret.Suffix,
	envDBMaxConns,
	envAuthMode,
	envInternalToken,
	envInternalToken + envsecret.Suffix,
	envUnhealthyThreshold,
	envOIDCJWKSURL,
	envRequireSizeSchema,
	envAttributingTypes,
	envCloudsConfig,
	envMetricsEnabled,
	envMetricsRefresh,
}

// The accepted values of TALLY_REPORTING_AUTH_MODE.
const (
	authModeEnforced = "enforced"
	authModeDisabled = "disabled"
)

// logLevels maps the accepted values of TALLY_LOG_LEVEL to their slog level.
// The match is exact: a lower-case "info" is a typo, and silently accepting it
// would hide the mistake behind a working service.
var logLevels = map[string]slog.Level{
	"DEBUG": slog.LevelDebug,
	"INFO":  slog.LevelInfo,
	"WARN":  slog.LevelWarn,
	"ERROR": slog.LevelError,
}

// Config is the Reporting API's resolved configuration. Every field is final by
// the time Load returns: file-backed secrets hold their content, and the two
// enumerated fields hold a value this package accepts.
type Config struct {
	// LogLevel is the slog threshold, one of DEBUG, INFO, WARN, or ERROR.
	LogLevel string `env:"TALLY_LOG_LEVEL" envDefault:"INFO"`
	// HTTPPort is the port the API server listens on.
	HTTPPort int `env:"TALLY_REPORTING_HTTP_PORT" envDefault:"8080"`
	// DBURL is the PostgreSQL connection string. It has no default because a
	// guessed database is worse than none. Supports the *_FILE convention.
	DBURL string `env:"TALLY_REPORTING_DB_URL"`
	// DBMaxConns bounds the connection pool. It is sized against the database's
	// max_connections divided by the replica count, which is what pgxpool cannot
	// know: left to itself it derives the bound from the node's CPU count, so a
	// pod landing on a large node opens far more connections than the database
	// budgeted for.
	DBMaxConns int32 `env:"TALLY_REPORTING_DB_MAX_CONNS" envDefault:"10"`
	// AuthMode is "enforced" or "disabled". Disabled short-circuits every
	// authentication middleware and exists for development and tests only.
	AuthMode string `env:"TALLY_REPORTING_AUTH_MODE" envDefault:"enforced"`
	// InternalToken is the shared secret guarding the /internal/* routes.
	// Supports the *_FILE convention.
	InternalToken string `env:"TALLY_REPORTING_INTERNAL_TOKEN"`
	// UnhealthyThresholdSeconds is how long readiness may keep failing before
	// liveness fails too and the orchestrator restarts the pod.
	UnhealthyThresholdSeconds int `env:"TALLY_REPORTING_UNHEALTHY_THRESHOLD_S" envDefault:"600"`
	// OIDCJWKSURL points at an OIDC provider's JWKS. It is the extension point
	// for accepting Bearer JWTs, and nothing implements it yet, so a set value
	// refuses startup instead of being ignored.
	OIDCJWKSURL string `env:"TALLY_REPORTING_OIDC_JWKS_URL"`
	// RequireSizeSchema is ingest's strict mode: when true, an event whose
	// payload carries a size for a (platform, resource_type) pair with no
	// registered schema is rejected; the default accepts it unvalidated, so a
	// registry that lags behind a new resource type does not stop collection.
	// The variable has no REPORTING infix because roadmap section WP 1.3 names
	// it TALLY_INGEST_REQUIRE_SIZE_SCHEMA.
	RequireSizeSchema bool `env:"TALLY_INGEST_REQUIRE_SIZE_SCHEMA" envDefault:"false"`
	// AttributingRelationTypes are the relation types the cycle guard walks when
	// a relation is created; an empty list disables the guard. Setting the
	// variable to the empty string yields that empty list. "member_of" and
	// "managed_by" reach a virtual project and attribute no cost, so a list
	// naming either is refused.
	AttributingRelationTypes []string `env:"TALLY_REPORTING_ATTRIBUTING_RELATION_TYPES" envDefault:"infrastructure_tenant"`
	// CloudsConfigPath is the path to the deployment's clouds YAML, the file the
	// reconciliation framework reads at startup to learn which clouds it can
	// sync. It has no *_FILE companion because the value is a path already, not
	// a secret. Unset is valid and means no clouds are configured, so every sync
	// answers 404. Whether the file exists and parses is checked by
	// reconciliation.LoadConfig, not here.
	CloudsConfigPath string `env:"TALLY_REPORTING_CLOUDS_CONFIG"`
	// MetricsEnabled exposes the instrumentation: false makes GET /metrics answer
	// 404 and stops the gauge refresher. The instruments still exist and keep
	// counting either way, so turning the flag back on costs nothing and loses
	// only the samples of the window it was off. The variable has no REPORTING
	// infix because roadmap section 8 lists it among the common variables every
	// service reads.
	MetricsEnabled bool `env:"TALLY_METRICS_ENABLED" envDefault:"true"`
	// MetricsRefreshSeconds is the interval on which the tally_current_resources
	// gauge is re-derived from the projection.
	MetricsRefreshSeconds int `env:"TALLY_REPORTING_METRICS_REFRESH_S" envDefault:"60"`
}

// Load reads the environment, resolves the file-backed secrets, and checks the
// enumerated values. It does not check whether the required values are present:
// which ones are required depends on the caller, which is what ValidateServer
// and ValidateAdmin decide.
func Load() (Config, error) {
	cfg, err := env.ParseAs[Config]()
	if err != nil {
		return Config{}, fmt.Errorf("parsing the environment: %w", err)
	}

	if cfg.DBURL, err = envsecret.Resolve(envDBURL, cfg.DBURL); err != nil {
		return Config{}, err
	}
	if cfg.InternalToken, err = envsecret.Resolve(envInternalToken, cfg.InternalToken); err != nil {
		return Config{}, err
	}

	if _, ok := logLevels[cfg.LogLevel]; !ok {
		return Config{}, fmt.Errorf("%s: %q must be DEBUG, INFO, WARN, or ERROR", envLogLevel, cfg.LogLevel)
	}
	if cfg.AuthMode != authModeEnforced && cfg.AuthMode != authModeDisabled {
		return Config{}, fmt.Errorf("%s: %q must be %q or %q", envAuthMode, cfg.AuthMode, authModeEnforced, authModeDisabled)
	}
	// Zero reads as "no threshold" and does the opposite: the first failed ping
	// already outlasts it, so liveness fails immediately and every replica
	// restarts at once, which is the storm the threshold exists to prevent.
	if cfg.UnhealthyThresholdSeconds <= 0 {
		return Config{}, fmt.Errorf("%s: %d must be positive", envUnhealthyThreshold, cfg.UnhealthyThresholdSeconds)
	}
	if cfg.DBMaxConns <= 0 {
		return Config{}, fmt.Errorf("%s: %d must be positive", envDBMaxConns, cfg.DBMaxConns)
	}
	if cfg.MetricsRefreshSeconds <= 0 {
		return Config{}, fmt.Errorf("%s: %d must be positive", envMetricsRefresh, cfg.MetricsRefreshSeconds)
	}
	// env applies the default to a variable set to the empty string, the same as
	// to an unset one, so the raw value is what tells the two apart. Only the
	// explicit empty value disables the cycle guard; anything else that parses
	// into an empty entry is a stray comma, which would otherwise silently mean
	// a relation type no relation has.
	raw, isSet := os.LookupEnv(envAttributingTypes)
	if isSet && raw == "" {
		cfg.AttributingRelationTypes = []string{}
	}
	if slices.Contains(cfg.AttributingRelationTypes, "") {
		return Config{}, fmt.Errorf("%s: %q must not contain an empty relation type", envAttributingTypes, raw)
	}
	// "member_of" and "managed_by" carry structure rather than money: a reseller
	// is never billed its customer's usage, a meta-project never absorbs its
	// members' costs. A list naming either is refused, not defaulted away.
	for _, relationType := range cfg.AttributingRelationTypes {
		if project.IsVirtualRelationType(relationType) {
			return Config{}, fmt.Errorf("%s: %q reaches a virtual project and attributes no cost", envAttributingTypes, relationType)
		}
	}

	return cfg, nil
}

// ValidateServer is the API server's startup gate. It refuses a configuration
// the server cannot honor instead of starting and failing per request.
func (c Config) ValidateServer() error {
	if err := c.requireDBURL(); err != nil {
		return err
	}
	if c.AuthMode == authModeEnforced && c.InternalToken == "" {
		return fmt.Errorf("%s: must be set unless %s is %q", envInternalToken, envAuthMode, authModeDisabled)
	}
	if c.OIDCJWKSURL != "" {
		return fmt.Errorf("%s: OIDC authentication is not implemented", envOIDCJWKSURL)
	}
	return nil
}

// ValidateAdmin is the admin CLI's startup gate. The CLI works on the database
// directly and serves nothing, so the connection string is all it needs.
func (c Config) ValidateAdmin() error {
	return c.requireDBURL()
}

// requireDBURL holds the one check both gates share, so both name the missing
// variable the same way.
func (c Config) requireDBURL() error {
	if c.DBURL == "" {
		return fmt.Errorf("%s: must be set", envDBURL)
	}
	return nil
}

// SlogLevel is the slog level that LogLevel names. A Config that never went
// through Load, and so carries an unchecked level, logs at info.
func (c Config) SlogLevel() slog.Level {
	if level, ok := logLevels[c.LogLevel]; ok {
		return level
	}
	return slog.LevelInfo
}
