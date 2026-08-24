// Package config parses the metering engine's environment configuration. Every
// subcommand of the engine CLI loads the same Config and then runs the
// validation gate that fits it, so a subcommand is never blocked by a setting
// only another one needs.
//
// Configuration comes from the environment alone, prefixed TALLY_. Secrets also
// accept the *_FILE convention, which is how a Kubernetes Secret volume reaches
// the process without the value appearing in a pod spec.
//
// The normative specification is roadmap/00-conventions.md section 8 and the
// configuration block of roadmap/03-phase-3-metering-rating.md.
package config

import (
	"fmt"
	"os"
	"slices"

	"github.com/caarlos0/env/v11"

	"github.com/b42labs/tally/internal/core/envsecret"
)

// The variables this package reads. They are named here because the errors
// quote them: an operator gets the variable to fix, not a Go field name.
const (
	envLogLevel         = "TALLY_LOG_LEVEL"
	envDBURL            = "TALLY_ENGINE_DB_URL"
	envReportingDBURL   = "TALLY_ENGINE_REPORTING_DB_URL"
	envVMURL            = "TALLY_ENGINE_VM_URL"
	envGraceHours       = "TALLY_ENGINE_GRACE_HOURS"
	envAutoFinalize     = "TALLY_ENGINE_AUTO_FINALIZE"
	envAttributingTypes = "TALLY_ENGINE_ATTRIBUTING_RELATION_TYPES"
	envCounterSources   = "TALLY_ENGINE_COUNTER_SOURCES"
)

// EnvNames is every variable this package reads, including the *_FILE
// companions of the secrets. Tests blank all of them, so a value in the
// developer's shell never reaches the code under test.
var EnvNames = []string{
	envLogLevel,
	envDBURL,
	envDBURL + envsecret.Suffix,
	envReportingDBURL,
	envReportingDBURL + envsecret.Suffix,
	envVMURL,
	envGraceHours,
	envAutoFinalize,
	envAttributingTypes,
	envCounterSources,
}

// logLevels are the accepted values of TALLY_LOG_LEVEL. The match is exact: a
// lower-case "info" is a typo, and silently accepting it would hide the mistake
// behind a working service.
var logLevels = []string{"DEBUG", "INFO", "WARN", "ERROR"}

// Config is the metering engine's resolved configuration. Every field is final
// by the time Load returns: file-backed secrets hold their content, and the one
// enumerated field holds a value this package accepts.
type Config struct {
	// LogLevel is the slog threshold, one of DEBUG, INFO, WARN, or ERROR.
	LogLevel string `env:"TALLY_LOG_LEVEL" envDefault:"INFO"`
	// DBURL is the PostgreSQL connection string of the engine's own database,
	// which holds the runs and their records. It has no default because a
	// guessed database is worse than none. Supports the *_FILE convention.
	DBURL string `env:"TALLY_ENGINE_DB_URL"`
	// ReportingDBURL is the PostgreSQL connection string of the Reporting API's
	// database, which the engine reads events and resources from. It has no
	// default for the same reason as DBURL. Migration 0008 of the reporting
	// chain creates the group role tally_engine_reader and grants it SELECT on
	// the four tables metering reads; a deployment connects as a login role that
	// is a member of it. Supports the *_FILE convention.
	ReportingDBURL string `env:"TALLY_ENGINE_REPORTING_DB_URL"`
	// VMURL is the base URL of the VictoriaMetrics instance the metricsql
	// counter sources are queried against. It is needed only when the counter
	// sources file declares metricsql sources. It has no default because the
	// endpoint differs per deployment, and no gate here: the first subcommand
	// that queries VictoriaMetrics adds its own.
	VMURL string `env:"TALLY_ENGINE_VM_URL"`
	// GraceHours is how long a run waits after its billing period ends before it
	// executes, so events that arrive late still reach the run that bills them.
	// Zero is a grace window of no hours, which runs the period the moment it
	// closes.
	GraceHours int `env:"TALLY_ENGINE_GRACE_HOURS" envDefault:"72"`
	// AutoFinalize lets a completed run finalize itself. It defaults to false
	// because a finalized run is immutable and its data may reach an ERP, so the
	// step gets a human gate.
	AutoFinalize bool `env:"TALLY_ENGINE_AUTO_FINALIZE" envDefault:"false"`
	// AttributingRelationTypes are the relation types attribution walks when it
	// bills a project under its attributor; an empty list bills every project on
	// its own. Setting the variable to the empty string yields that empty list.
	AttributingRelationTypes []string `env:"TALLY_ENGINE_ATTRIBUTING_RELATION_TYPES" envDefault:"infrastructure_tenant"`
	// CounterSourcesPath is the path to the counter sources YAML, the file that
	// declares which counters exist and how each one is measured;
	// cmd/tally-engine/counter-sources.example.yaml shows the format. Setting
	// the variable to the empty string means no counter sources, which
	// counters.Load reads as the zero configuration; a path to a file that does
	// not exist is an error when the file is read, not a run without counters.
	// It has no *_FILE companion because the value is a path already, not a
	// secret. Whether the file parses is checked by the package that reads it,
	// not here.
	CounterSourcesPath string `env:"TALLY_ENGINE_COUNTER_SOURCES" envDefault:"/etc/tally/counter-sources.yaml"`
}

// Load reads the environment, resolves the file-backed secrets, and checks the
// values this package can check on its own. It does not check whether the
// required values are present: which ones are required depends on the
// subcommand, which is what the validation gates decide.
func Load() (Config, error) {
	cfg, err := env.ParseAs[Config]()
	if err != nil {
		return Config{}, fmt.Errorf("parsing the environment: %w", err)
	}

	if cfg.DBURL, err = envsecret.Resolve(envDBURL, cfg.DBURL); err != nil {
		return Config{}, err
	}
	if cfg.ReportingDBURL, err = envsecret.Resolve(envReportingDBURL, cfg.ReportingDBURL); err != nil {
		return Config{}, err
	}

	if !slices.Contains(logLevels, cfg.LogLevel) {
		return Config{}, fmt.Errorf("%s: %q must be DEBUG, INFO, WARN, or ERROR", envLogLevel, cfg.LogLevel)
	}
	// Zero is a legitimate grace window of no hours. A negative one would place
	// the run before the period it bills has ended.
	if cfg.GraceHours < 0 {
		return Config{}, fmt.Errorf("%s: %d must not be negative", envGraceHours, cfg.GraceHours)
	}
	// env applies the default to a variable set to the empty string, the same as
	// to an unset one, so the raw value is what tells the two apart. Only the
	// explicit empty value disables attribution; anything else that parses into
	// an empty entry is a stray comma, which would otherwise silently mean a
	// relation type no relation has.
	raw, isSet := os.LookupEnv(envAttributingTypes)
	if isSet && raw == "" {
		cfg.AttributingRelationTypes = []string{}
	}
	if slices.Contains(cfg.AttributingRelationTypes, "") {
		return Config{}, fmt.Errorf("%s: %q must not contain an empty relation type", envAttributingTypes, raw)
	}
	// The counter sources path is read the same way: only the explicit empty
	// value means no counter sources, which counters.Load honors by reading
	// nothing, while an unset variable keeps the default path.
	if raw, isSet := os.LookupEnv(envCounterSources); isSet && raw == "" {
		cfg.CounterSourcesPath = ""
	}

	return cfg, nil
}

// ValidateDB is the gate of the subcommands backed by the engine's database. It
// refuses a configuration they cannot honor instead of failing at the first
// query. The VictoriaMetrics URL gets its gate from the package that first uses
// it.
func (c Config) ValidateDB() error {
	if c.DBURL == "" {
		return fmt.Errorf("%s: must be set", envDBURL)
	}
	return nil
}

// ValidateReporting is the gate of the subcommands that read the reporting
// database. It refuses a configuration they cannot honor instead of failing at
// the first query.
func (c Config) ValidateReporting() error {
	if c.ReportingDBURL == "" {
		return fmt.Errorf("%s: must be set", envReportingDBURL)
	}
	return nil
}
