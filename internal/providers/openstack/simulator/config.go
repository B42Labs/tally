// Package simulator publishes one simulated month of oslo.messaging
// notifications. It renders a seeded month of nova, cinder, neutron, and glance
// notifications onto a RabbitMQ broker, or into files when no broker is
// configured, and a virtual clock decides how much wall time that month costs.
//
// The notifications are the ones a real deployment publishes, so the collector
// in internal/providers/openstack consumes them unmodified: the simulator sits
// on the producing side of the bus and nothing on the consuming side knows it
// is there.
//
// This file parses the simulator's environment configuration. Configuration
// comes from the environment alone, prefixed TALLY_, and secrets also accept
// the *_FILE convention; the normative specification is
// roadmap/00-conventions.md section 8.
package simulator

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/caarlos0/env/v11"
)

// fileSuffix names the companion variable of a secret: the value of
// TALLY_SIM_AMQP_URL is read from the file at TALLY_SIM_AMQP_URL_FILE when that
// variable holds a path.
const fileSuffix = "_FILE"

// The variables this package reads. They are named here because the errors
// quote them: an operator gets the variable to fix, not a Go field name.
const (
	envLogLevel = "TALLY_LOG_LEVEL"
	envHTTPAddr = "TALLY_SIM_HTTP_ADDR"
	envHTTPPort = "TALLY_SIM_HTTP_PORT"
	envAMQPURL  = "TALLY_SIM_AMQP_URL"
	envCloud    = "TALLY_SIM_CLOUD"
)

// EnvNames is every variable this package reads, including the *_FILE companion
// of the secret. Tests blank all of them, so a value in the developer's shell
// never reaches the code under test.
var EnvNames = []string{
	envLogLevel,
	envHTTPAddr,
	envHTTPPort,
	envAMQPURL,
	envAMQPURL + fileSuffix,
	envCloud,
}

// loopback is the address the control endpoint binds unless a deployment names
// another one. It is the default of TALLY_SIM_HTTP_ADDR and the answer for a
// Config that never went through Load.
const loopback = "127.0.0.1"

// logLevels maps the accepted values of TALLY_LOG_LEVEL to their slog level.
// The match is exact: a lower-case "info" is a typo, and silently accepting it
// would hide the mistake behind a working service.
var logLevels = map[string]slog.Level{
	"DEBUG": slog.LevelDebug,
	"INFO":  slog.LevelInfo,
	"WARN":  slog.LevelWarn,
	"ERROR": slog.LevelError,
}

// Config is the simulator's resolved configuration. Every field is final by the
// time Load returns: a file-backed broker URL holds the file's content, and the
// log level is one this package accepts.
type Config struct {
	// LogLevel is the slog threshold, one of DEBUG, INFO, WARN, or ERROR. Both
	// subcommands read it.
	LogLevel string `env:"TALLY_LOG_LEVEL" envDefault:"INFO"`
	// HTTPAddr is the address the control endpoint binds. It defaults to loopback
	// because the endpoint carries no credential and PUT /clock changes the pace
	// of a run: a simulator on a host beside a control plane would otherwise
	// answer everybody on the management network. A deployment that means to
	// publish the port sets 0.0.0.0, which is what the compose stack does.
	HTTPAddr string `env:"TALLY_SIM_HTTP_ADDR" envDefault:"127.0.0.1"`
	// HTTPPort is the port the control endpoint listens on. It is served only
	// while the simulator publishes, because there is nothing to control once the
	// month is over.
	HTTPPort int `env:"TALLY_SIM_HTTP_PORT" envDefault:"8080"`
	// AMQPURL is the broker the notifications are published to. It carries the
	// broker password, so it supports the *_FILE convention. Empty puts run in
	// file mode, where the month is written out instead of published.
	AMQPURL string `env:"TALLY_SIM_AMQP_URL"`
	// Cloud is read by run alone: it is the salt of every generated identifier
	// and the cloud of events.jsonl. It has no default because a guessed cloud
	// silently books usage to the wrong one.
	Cloud string `env:"TALLY_SIM_CLOUD"`
}

// Load reads the environment, resolves the file-backed broker URL, and checks
// the log level. It does not check whether the required values are present:
// which ones are required depends on the subcommand, which is what ValidateRun
// and ValidateReplay decide.
func Load() (Config, error) {
	cfg, err := env.ParseAs[Config]()
	if err != nil {
		return Config{}, fmt.Errorf("parsing the environment: %w", err)
	}

	if cfg.AMQPURL, err = resolveFileSecret(envAMQPURL, cfg.AMQPURL); err != nil {
		return Config{}, err
	}

	if _, ok := logLevels[cfg.LogLevel]; !ok {
		return Config{}, fmt.Errorf("%s: %q must be DEBUG, INFO, WARN, or ERROR", envLogLevel, cfg.LogLevel)
	}

	return cfg, nil
}

// resolveFileSecret applies the *_FILE convention to one variable: when its
// companion holds a path, the file's content becomes the value. Kubernetes
// writes Secret volumes with a trailing newline, so one is trimmed. An empty
// file is rejected because it usually means the secret was never populated,
// which would otherwise surface much later as an authentication failure.
func resolveFileSecret(name, value string) (string, error) {
	fileVar := name + fileSuffix
	path := os.Getenv(fileVar)
	if path == "" {
		return value, nil
	}
	if value != "" {
		return "", fmt.Errorf("set %s or %s, not both", name, fileVar)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", fileVar, err)
	}
	secret := strings.TrimSuffix(string(content), "\n")
	if secret == "" {
		return "", fmt.Errorf("%s: file %s is empty", fileVar, path)
	}
	return secret, nil
}

// ValidateRun is the gate of the subcommand that generates the month. It asks
// for the cloud alone: the broker is optional because run --out writes the
// month to files and never connects to one.
func (c Config) ValidateRun() error {
	if c.Cloud == "" {
		return fmt.Errorf("%s: must be set", envCloud)
	}
	return nil
}

// ValidateReplay is the gate of the subcommand that publishes an already
// generated month. Replaying carries the cloud in the recorded notifications,
// so the broker is all it needs.
func (c Config) ValidateReplay() error {
	if c.AMQPURL == "" {
		return fmt.Errorf("%s: must be set", envAMQPURL)
	}
	return nil
}

// ControlAddr is the address the control endpoint listens on. An empty
// HTTPAddr, which is a Config that never went through Load, binds loopback
// rather than every interface: the endpoint carries no credential, so the wider
// bind is a choice a deployment makes and not one a zero value falls into.
func (c Config) ControlAddr() string {
	addr := c.HTTPAddr
	if addr == "" {
		addr = loopback
	}
	return net.JoinHostPort(addr, strconv.Itoa(c.HTTPPort))
}

// SlogLevel is the slog level that LogLevel names. A Config that never went
// through Load, and so carries an unchecked level, logs at info.
func (c Config) SlogLevel() slog.Level {
	if level, ok := logLevels[c.LogLevel]; ok {
		return level
	}
	return slog.LevelInfo
}
