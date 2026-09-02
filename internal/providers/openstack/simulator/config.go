// Package simulator publishes one simulated month of oslo.messaging
// notifications. It renders a seeded month of nova, cinder, neutron, glance,
// and octavia notifications onto a RabbitMQ broker, or into files when no
// broker is configured, and a virtual clock decides how much wall time that
// month costs.
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
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/caarlos0/env/v11"
)

// fileSuffix names the companion variable of a secret: the value of
// TALLY_SIM_AMQP_URL is read from the file at TALLY_SIM_AMQP_URL_FILE when that
// variable holds a path, and TALLY_SIM_API_TOKEN from TALLY_SIM_API_TOKEN_FILE
// and TALLY_SIM_OTLP_PASSWORD from TALLY_SIM_OTLP_PASSWORD_FILE the same way.
const fileSuffix = "_FILE"

// The variables this package reads. They are named here because the errors
// quote them: an operator gets the variable to fix, not a Go field name.
const (
	envLogLevel = "TALLY_LOG_LEVEL"
	envHTTPAddr = "TALLY_SIM_HTTP_ADDR"
	envHTTPPort = "TALLY_SIM_HTTP_PORT"
	envAMQPURL  = "TALLY_SIM_AMQP_URL"
	envCloud    = "TALLY_SIM_CLOUD"

	envReportingURL      = "TALLY_SIM_REPORTING_URL"
	envReportingInsecure = "TALLY_SIM_REPORTING_INSECURE"
	envAPIToken          = "TALLY_SIM_API_TOKEN"
	envGardenCloud       = "TALLY_SIM_GARDEN_CLOUD"

	envOTLPURL        = "TALLY_SIM_OTLP_URL"
	envOTLPUser       = "TALLY_SIM_OTLP_USER"
	envOTLPPassword   = "TALLY_SIM_OTLP_PASSWORD"
	envOTLPInsecure   = "TALLY_SIM_OTLP_INSECURE"
	envMetricsEnabled = "TALLY_METRICS_ENABLED"
)

// EnvNames is every variable this package reads, including the *_FILE
// companions of the secrets. Tests blank all of them, so a value in the
// developer's shell never reaches the code under test.
var EnvNames = []string{
	envLogLevel,
	envHTTPAddr,
	envHTTPPort,
	envAMQPURL,
	envAMQPURL + fileSuffix,
	envCloud,
	envReportingURL,
	envReportingInsecure,
	envAPIToken,
	envAPIToken + fileSuffix,
	envGardenCloud,
	envOTLPURL,
	envOTLPUser,
	envOTLPPassword,
	envOTLPPassword + fileSuffix,
	envOTLPInsecure,
	envMetricsEnabled,
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
	// ReportingURL is the Reporting API the projects are registered with. run
	// reads it when --register-projects is on and ignores it otherwise. It must
	// be an absolute https URL, because the api token travels on it;
	// ReportingInsecure is what allows a plaintext one.
	ReportingURL string `env:"TALLY_SIM_REPORTING_URL"`
	// ReportingInsecure allows an http Reporting API. It exists for a simulator
	// and an API on the same machine, and for development; anywhere else it puts
	// an api token of role admin on the wire in cleartext.
	ReportingInsecure bool `env:"TALLY_SIM_REPORTING_INSECURE" envDefault:"false"`
	// APIToken is an api token of role admin, which POST /api/v1/projects and
	// POST /api/v1/projects/{id}/relations demand. It supports the *_FILE
	// convention.
	APIToken string `env:"TALLY_SIM_API_TOKEN"`
	// GardenCloud is the cloud the two Gardener projects are registered under.
	// It has no default, for the reason Cloud has none.
	GardenCloud string `env:"TALLY_SIM_GARDEN_CLOUD"`
	// OTLPURL is the OTLP/HTTP endpoint the traffic and inventory series of a run
	// are pushed to. Empty is a run without a push. It has to be absolute and
	// carry a host, and to be https unless OTLPInsecure allows a plaintext one,
	// because the Basic password travels on it. run reads it alone.
	OTLPURL string `env:"TALLY_SIM_OTLP_URL"`
	// OTLPUser is the Basic user of the push. The endpoint in front of the
	// collector takes Basic auth, so a URL without a user reaches nothing.
	OTLPUser string `env:"TALLY_SIM_OTLP_USER"`
	// OTLPPassword is the Basic password of the push. It supports the *_FILE
	// convention.
	OTLPPassword string `env:"TALLY_SIM_OTLP_PASSWORD"`
	// OTLPInsecure allows an http endpoint. It exists for a simulator and a
	// collector on the same machine, and for development; anywhere else it puts
	// the Basic password on the wire in cleartext.
	OTLPInsecure bool `env:"TALLY_SIM_OTLP_INSECURE" envDefault:"false"`
	// MetricsEnabled serves the inventory on GET /metrics of the control listener
	// while a run publishes, where it stands in for the OpenStack database
	// exporter a deployment scrapes. False registers no route, and the fake
	// OpenStack API then answers that path with its own 404. A Config that never
	// went through Load carries false, which is why a test that needs the
	// endpoint sets it. The variable has no SIM infix because roadmap section 8
	// lists it among the common variables every service reads.
	MetricsEnabled bool `env:"TALLY_METRICS_ENABLED" envDefault:"true"`
}

// Load reads the environment, resolves the file-backed secrets, and checks the
// log level. It does not check whether the required values are present: which
// ones are required depends on the subcommand and on its switches, which is
// what ValidateRun, ValidateReplay, ValidateRegistration, and ValidateMetrics
// decide.
func Load() (Config, error) {
	cfg, err := env.ParseAs[Config]()
	if err != nil {
		return Config{}, fmt.Errorf("parsing the environment: %w", err)
	}

	if cfg.AMQPURL, err = resolveFileSecret(envAMQPURL, cfg.AMQPURL); err != nil {
		return Config{}, err
	}
	if cfg.APIToken, err = resolveFileSecret(envAPIToken, cfg.APIToken); err != nil {
		return Config{}, err
	}
	if cfg.OTLPPassword, err = resolveFileSecret(envOTLPPassword, cfg.OTLPPassword); err != nil {
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

// ValidateRegistration is the gate of run --register-projects. It asks for the
// Reporting API, an api token for it, and the cloud the Gardener projects are
// registered under, none of which a run without the switch reads.
//
// The two clouds have to differ because a cloud is one installation of one
// platform: a Gardener project registered under the tenants' cloud would key a
// row of the OpenStack installation, and the relation would then point the
// project at itself.
func (c Config) ValidateRegistration() error {
	if c.ReportingURL == "" {
		return fmt.Errorf("%s: must be set when --register-projects is on", envReportingURL)
	}
	if err := c.validateReportingURL(); err != nil {
		return err
	}
	// The token is named and never quoted: an error an operator pastes into a
	// ticket must not carry the credential it is about.
	if c.APIToken == "" {
		return fmt.Errorf("%s: must be set when --register-projects is on", envAPIToken)
	}
	if c.GardenCloud == "" {
		return fmt.Errorf("%s: must be set when --register-projects is on", envGardenCloud)
	}
	if c.GardenCloud == c.Cloud {
		return fmt.Errorf("%s: %q must differ from %s: a cloud is one installation of one platform",
			envGardenCloud, c.GardenCloud, envCloud)
	}
	return nil
}

// validateReportingURL holds the destination of a registration to what the
// registrar does with it. The URL carries no credential, unlike the broker URL,
// so the refusals quote it: what an operator mistyped is what they need to see.
//
// The scheme is checked for the reason the collector's is, in
// internal/providers/openstack/config.go, and with more at stake: the token
// travels in a header on every request, and it is of role admin, so it is not
// scoped to one (platform, cloud) pair the way an ingest token is. Anybody on
// the path of a plaintext registration reads a credential that writes the whole
// project registry.
func (c Config) validateReportingURL() error {
	parsed, err := url.Parse(c.ReportingURL)
	// The query and the fragment are looked for in the value itself rather than
	// in the parsed URL, because an empty one of either does not survive the
	// parse: a trailing "?" is recorded in ForceQuery and leaves RawQuery empty,
	// and a "#" with nothing behind it parses to an empty Fragment. Both would
	// pass a check on the parsed fields and then swallow the appended route —
	// https://host# becomes https://host#/api/v1/projects, which posts the token
	// and the row to / instead.
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") ||
		strings.ContainsAny(c.ReportingURL, "?#") {
		return fmt.Errorf("%s: %q must be an absolute http(s) URL with no query or fragment, "+
			"because the registry route is appended to it", envReportingURL, c.ReportingURL)
	}
	if parsed.Scheme != "https" && !c.ReportingInsecure {
		return fmt.Errorf("%s: %q must use https, because the admin api token travels on it; "+
			"set %s=true to allow plaintext", envReportingURL, c.ReportingURL, envReportingInsecure)
	}
	return nil
}

// ValidateMetrics is the gate of the push side of the metrics, checked before a
// run dials the broker. An empty TALLY_SIM_OTLP_URL is a run without a push and
// passes: the month is generated and published, and the samples it places stay
// in the process.
//
// A URL that is set is one a whole month is posted to, so what it needs is
// asked for now rather than at the first flush, an hour into a paced run.
func (c Config) ValidateMetrics() error {
	if c.OTLPURL == "" {
		return nil
	}
	if c.OTLPUser == "" {
		return fmt.Errorf("%s: must be set when %s is set", envOTLPUser, envOTLPURL)
	}
	if c.OTLPPassword == "" {
		return fmt.Errorf("%s: must be set when %s is set", envOTLPPassword, envOTLPURL)
	}
	// The value is named and not quoted, unlike the Reporting API's: a mistyped
	// endpoint may carry userinfo, and an error an operator pastes into a ticket
	// must not carry a credential.
	parsed, err := url.Parse(c.OTLPURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return fmt.Errorf("%s: must be an absolute http(s) URL with a host", envOTLPURL)
	}
	// The credential of a push is the Basic one, and the URL is where it must
	// not be. A token carried as the userinfo or as a query parameter is one no
	// message can be trusted to keep out: an http client redacts the password
	// half of a userinfo before it wraps a transport error and leaves the user
	// and the query standing, and that error is logged as JSON and shipped from
	// there. So the shape is refused rather than redacted afterwards.
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("%s: must carry no userinfo, query or fragment; the credential of a push "+
			"belongs in %s and %s", envOTLPURL, envOTLPUser, envOTLPPassword)
	}
	if parsed.Scheme != "https" && !c.OTLPInsecure {
		return fmt.Errorf("%s: must use https, because the Basic password travels on it; "+
			"set %s=true to allow plaintext", envOTLPURL, envOTLPInsecure)
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
