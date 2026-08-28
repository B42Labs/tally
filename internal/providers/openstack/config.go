// Package openstack is the OpenStack event collector: it consumes
// oslo.messaging notifications from AMQP, maps them to Tally events, buffers
// them in a SQLite outbox, and posts them to the Reporting API. This file
// parses the collector's environment configuration.
//
// The collector has two modes and each runs its own validation gate. Serving
// needs the whole pipeline, so it requires the broker, the cloud, the
// Reporting API and its token, and the buffer path. Dumping notifications maps
// nothing and posts nothing, so the broker alone is enough and the operator
// exploring an unknown deployment is not asked for values that mode never
// reads.
//
// Configuration comes from the environment alone, prefixed TALLY_. Secrets also
// accept the *_FILE convention, which is how a Kubernetes Secret volume reaches
// the process without the value appearing in a pod spec.
//
// The normative specification is roadmap/00-conventions.md section 8.
package openstack

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"

	"github.com/caarlos0/env/v11"
)

// fileSuffix names the companion variable of a secret: the value of
// TALLY_OSC_AMQP_URL is read from the file at TALLY_OSC_AMQP_URL_FILE when that
// variable holds a path.
const fileSuffix = "_FILE"

// maxBatchItems is the largest batch POST /api/v1/events accepts. The server
// answers a longer array with 413, so a batch size past it would turn every
// flush into a rejected request.
const maxBatchItems = 1000

// The variables this package reads. They are named here because the errors
// quote them: an operator gets the variable to fix, not a Go field name.
const (
	envLogLevel           = "TALLY_LOG_LEVEL"
	envMetricsEnabled     = "TALLY_METRICS_ENABLED"
	envHTTPPort           = "TALLY_OSC_HTTP_PORT"
	envAMQPURL            = "TALLY_OSC_AMQP_URL"
	envExchanges          = "TALLY_OSC_EXCHANGES"
	envTopics             = "TALLY_OSC_TOPICS"
	envCloud              = "TALLY_OSC_CLOUD"
	envReportingURL       = "TALLY_OSC_REPORTING_URL"
	envReportingInsecure  = "TALLY_OSC_REPORTING_INSECURE"
	envToken              = "TALLY_OSC_TOKEN"
	envBufferPath         = "TALLY_OSC_BUFFER_PATH"
	envBatchMax           = "TALLY_OSC_BATCH_MAX"
	envFlushInterval      = "TALLY_OSC_FLUSH_INTERVAL_S"
	envBufferMaxEvents    = "TALLY_OSC_BUFFER_MAX_EVENTS"
	envPrefetch           = "TALLY_OSC_PREFETCH"
	envUnhealthyThreshold = "TALLY_OSC_UNHEALTHY_THRESHOLD_S"
)

// EnvNames is every variable this package reads, including the *_FILE
// companions of the secrets. Tests blank all of them, so a value in the
// developer's shell never reaches the code under test.
var EnvNames = []string{
	envLogLevel,
	envMetricsEnabled,
	envHTTPPort,
	envAMQPURL,
	envAMQPURL + fileSuffix,
	envExchanges,
	envTopics,
	envCloud,
	envReportingURL,
	envReportingInsecure,
	envToken,
	envToken + fileSuffix,
	envBufferPath,
	envBatchMax,
	envFlushInterval,
	envBufferMaxEvents,
	envPrefetch,
	envUnhealthyThreshold,
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

// Config is the collector's resolved configuration. Every field is final by the
// time Load returns: file-backed secrets hold their content, the log level is
// one this package accepts, and every bounded number is within its bounds.
type Config struct {
	// LogLevel is the slog threshold, one of DEBUG, INFO, WARN, or ERROR.
	LogLevel string `env:"TALLY_LOG_LEVEL" envDefault:"INFO"`
	// MetricsEnabled exposes the instrumentation: false makes GET /metrics answer
	// 404. The variable has no OSC infix because roadmap section 8 lists it among
	// the common variables every service reads.
	MetricsEnabled bool `env:"TALLY_METRICS_ENABLED" envDefault:"true"`
	// HTTPPort is the port the probe and metrics endpoints listen on. The
	// collector serves no API of its own.
	HTTPPort int `env:"TALLY_OSC_HTTP_PORT" envDefault:"8080"`
	// AMQPURL is the broker the notifications are consumed from. It carries the
	// broker password, so it supports the *_FILE convention.
	AMQPURL string `env:"TALLY_OSC_AMQP_URL"`
	// Exchanges are the service exchanges the collector binds its queue to. The
	// default covers nova, neutron, cinder and glance; a deployment that renamed
	// them through control_exchange lists its own.
	//
	// Octavia publishes on the exchange octavia, and the default leaves it out.
	// The exchanges are declared passively, so a collector refuses to run while
	// one it lists is missing from the broker, and a default naming octavia would
	// stop every deployment that runs none. A deployment with octavia lists
	// nova,neutron,cinder,glance,octavia.
	Exchanges []string `env:"TALLY_OSC_EXCHANGES" envSeparator:"," envDefault:"nova,neutron,cinder,glance"`
	// Topics are the notification topics bound on each exchange, matching the
	// notification_topics of the services being collected.
	Topics []string `env:"TALLY_OSC_TOPICS" envSeparator:"," envDefault:"notifications.info"`
	// Cloud is the cloud name every emitted event is attributed to. It has no
	// default because a guessed cloud silently books usage to the wrong one.
	Cloud string `env:"TALLY_OSC_CLOUD"`
	// ReportingURL is the base URL of the Reporting API the sender posts to. It
	// must be an absolute https URL, because the ingest token travels on it;
	// ReportingInsecure is what allows a plaintext one.
	ReportingURL string `env:"TALLY_OSC_REPORTING_URL"`
	// ReportingInsecure allows an http Reporting API. It exists for a collector
	// and an API on the same trusted network, and for development; anywhere else
	// it puts the ingest token on the wire in cleartext.
	ReportingInsecure bool `env:"TALLY_OSC_REPORTING_INSECURE" envDefault:"false"`
	// Token authenticates the sender against the Reporting API. Supports the
	// *_FILE convention.
	Token string `env:"TALLY_OSC_TOKEN"`
	// BufferPath is the SQLite file backing the outbox. It belongs on a volume
	// that outlives the container: everything consumed but not yet delivered
	// lives there and nowhere else.
	BufferPath string `env:"TALLY_OSC_BUFFER_PATH"`
	// BatchMax is how many buffered events one POST carries, bounded by what the
	// ingest API accepts.
	BatchMax int `env:"TALLY_OSC_BATCH_MAX" envDefault:"500"`
	// FlushIntervalSeconds is how long the sender waits before posting a batch
	// that has not filled up.
	FlushIntervalSeconds int `env:"TALLY_OSC_FLUSH_INTERVAL_S" envDefault:"5"`
	// BufferMaxEvents is the outbox depth at which the collector stops consuming.
	// The events then wait on the bus instead of being dropped, which is what
	// keeps an unreachable Reporting API from costing usage data.
	BufferMaxEvents int64 `env:"TALLY_OSC_BUFFER_MAX_EVENTS" envDefault:"1000000"`
	// Prefetch is the AMQP QoS bound: how many unacknowledged messages the broker
	// hands out. Acks follow the outbox insert, so this bounds how much work a
	// crash replays.
	Prefetch int `env:"TALLY_OSC_PREFETCH" envDefault:"100"`
	// UnhealthyThresholdSeconds is how long readiness may keep failing before
	// liveness fails too and the orchestrator restarts the pod.
	UnhealthyThresholdSeconds int `env:"TALLY_OSC_UNHEALTHY_THRESHOLD_S" envDefault:"600"`
}

// Load reads the environment, resolves the file-backed secrets, and checks the
// log level and the numeric bounds. It does not check whether the required
// values are present: which ones are required depends on the mode, which is
// what ValidateServe and ValidateDump decide.
func Load() (Config, error) {
	cfg, err := env.ParseAs[Config]()
	if err != nil {
		return Config{}, fmt.Errorf("parsing the environment: %w", err)
	}

	if cfg.AMQPURL, err = resolveFileSecret(envAMQPURL, cfg.AMQPURL); err != nil {
		return Config{}, err
	}
	if cfg.Token, err = resolveFileSecret(envToken, cfg.Token); err != nil {
		return Config{}, err
	}

	// The sender appends the ingest path to this value, so a trailing slash would
	// build //api/v1/events. Go's client sends that path as written and the
	// Reporting API routes nothing to it, which the sender sees as a status it
	// retries forever.
	cfg.ReportingURL = strings.TrimRight(cfg.ReportingURL, "/")

	if _, ok := logLevels[cfg.LogLevel]; !ok {
		return Config{}, fmt.Errorf("%s: %q must be DEBUG, INFO, WARN, or ERROR", envLogLevel, cfg.LogLevel)
	}
	// Every numeric has a default, so both modes reach these checks with a value
	// to check, and both are refused the same way.
	if cfg.BatchMax < 1 || cfg.BatchMax > maxBatchItems {
		return Config{}, fmt.Errorf("%s: %d must be between 1 and %d", envBatchMax, cfg.BatchMax, maxBatchItems)
	}
	// Zero reads as "flush immediately" and does the opposite: the ticker has no
	// interval to wait, so the sender spins instead of batching.
	if cfg.FlushIntervalSeconds <= 0 {
		return Config{}, fmt.Errorf("%s: %d must be positive", envFlushInterval, cfg.FlushIntervalSeconds)
	}
	// Zero would put the outbox over its bound while still empty, so the
	// collector would stop consuming before it ever started.
	if cfg.BufferMaxEvents <= 0 {
		return Config{}, fmt.Errorf("%s: %d must be positive", envBufferMaxEvents, cfg.BufferMaxEvents)
	}
	// A QoS of zero means unlimited in AMQP, which is the one value the bound
	// exists to rule out.
	if cfg.Prefetch <= 0 {
		return Config{}, fmt.Errorf("%s: %d must be positive", envPrefetch, cfg.Prefetch)
	}
	// Zero reads as "no threshold" and does the opposite: the first failed probe
	// already outlasts it, so liveness fails immediately and every replica
	// restarts at once, which is the storm the threshold exists to prevent.
	if cfg.UnhealthyThresholdSeconds <= 0 {
		return Config{}, fmt.Errorf("%s: %d must be positive", envUnhealthyThreshold, cfg.UnhealthyThresholdSeconds)
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

// ValidateServe is the collecting service's startup gate. It refuses a
// configuration the collector cannot honor instead of starting and losing the
// notifications it consumes.
func (c Config) ValidateServe() error {
	required := []struct {
		name  string
		value string
	}{
		{envAMQPURL, c.AMQPURL},
		{envCloud, c.Cloud},
		{envReportingURL, c.ReportingURL},
		{envToken, c.Token},
		{envBufferPath, c.BufferPath},
	}
	for _, r := range required {
		if r.value == "" {
			return fmt.Errorf("%s: must be set", r.name)
		}
	}
	return c.validateReportingURL()
}

// validateReportingURL checks the value the sender builds its endpoint from and
// sends the ingest token to.
//
// The shape is checked because the endpoint is that value with the ingest path
// appended: a host without a scheme fails every delivery attempt instead of the
// startup, and the events pile up in the outbox meanwhile. A query or a fragment
// fails the same way and less visibly, because appending to one puts the ingest
// path inside it: https://host?tenant=acme becomes a POST to / with the query
// tenant=acme/api/v1/events, which the API answers 404 and the sender retries
// forever.
//
// The scheme is checked because the token travels in a header on every flush.
// The collector runs next to the OpenStack control plane and the Reporting API
// does not, so the link between them is exactly the one the project's TLS story
// does not cover, and that management network already carries the broker
// credentials of every compute node. The token is scoped to one (platform,
// cloud) pair, grants unrestricted ingest for it, and cannot be recovered after
// it is issued.
func (c Config) validateReportingURL() error {
	parsed, err := url.Parse(c.ReportingURL)
	// The query and the fragment are looked for in the value itself rather than in
	// the parsed URL, because an empty one of either does not survive the parse: a
	// trailing "?" is recorded in ForceQuery and leaves RawQuery empty, and a "#"
	// with nothing behind it parses to an empty Fragment. Both would pass a check
	// on the parsed fields and then swallow the appended path — https://host#
	// becomes https://host#/api/v1/events, which is a POST to / that the API
	// answers 404.
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") ||
		strings.ContainsAny(c.ReportingURL, "?#") {
		return fmt.Errorf("%s: %q must be an absolute http(s) URL with no query or fragment, "+
			"because the ingest path is appended to it", envReportingURL, c.ReportingURL)
	}
	if parsed.Scheme != "https" && !c.ReportingInsecure {
		return fmt.Errorf("%s: %q must use https, because the ingest token travels on it; "+
			"set %s=true to allow plaintext", envReportingURL, c.ReportingURL, envReportingInsecure)
	}
	return nil
}

// ValidateDump is the notification dump's gate. Dumping prints what the broker
// delivers: it maps nothing and posts nothing, so it needs neither a cloud nor
// a destination to send to.
func (c Config) ValidateDump() error {
	if c.AMQPURL == "" {
		return fmt.Errorf("%s: must be set", envAMQPURL)
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
