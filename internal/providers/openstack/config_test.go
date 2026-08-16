package openstack

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// setEnv applies vars and blanks every other variable the package reads, so a
// test never inherits a value from the developer's shell. A blank variable and
// an unset one mean the same thing here: env falls back to the envDefault for
// both. t.Setenv restores the previous environment when the test ends.
func setEnv(t *testing.T, vars map[string]string) {
	t.Helper()

	for name := range vars {
		if !slices.Contains(EnvNames, name) {
			t.Fatalf("test sets %s, which the package does not read", name)
		}
	}
	for _, name := range EnvNames {
		t.Setenv(name, vars[name])
	}
}

// writeSecret writes a file-mounted secret and returns its path.
func writeSecret(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// serveVars is a configuration ValidateServe accepts, so a test that varies one
// setting states only that setting.
func serveVars() map[string]string {
	return map[string]string{
		"TALLY_OSC_AMQP_URL":      "amqp://tally:s3cret@rabbit:5672/",
		"TALLY_OSC_CLOUD":         "prod",
		"TALLY_OSC_REPORTING_URL": "https://tally.example.com",
		"TALLY_OSC_TOKEN":         "t0ken",
		"TALLY_OSC_BUFFER_PATH":   "/var/lib/tally/outbox.db",
	}
}

// withVars returns the serve configuration with vars applied on top.
func withVars(vars map[string]string) map[string]string {
	merged := serveVars()
	for name, value := range vars {
		merged[name] = value
	}
	return merged
}

func TestLoadAppliesDefaults(t *testing.T) {
	setEnv(t, serveVars())

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}

	if cfg.LogLevel != "INFO" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "INFO")
	}
	if !cfg.MetricsEnabled {
		t.Error("MetricsEnabled = false, want true")
	}
	if cfg.HTTPPort != 8080 {
		t.Errorf("HTTPPort = %d, want 8080", cfg.HTTPPort)
	}
	if want := []string{"nova", "neutron", "cinder", "glance"}; !slices.Equal(cfg.Exchanges, want) {
		t.Errorf("Exchanges = %q, want %q", cfg.Exchanges, want)
	}
	if want := []string{"notifications.info"}; !slices.Equal(cfg.Topics, want) {
		t.Errorf("Topics = %q, want %q", cfg.Topics, want)
	}
	if cfg.BatchMax != 500 {
		t.Errorf("BatchMax = %d, want 500", cfg.BatchMax)
	}
	if cfg.FlushIntervalSeconds != 5 {
		t.Errorf("FlushIntervalSeconds = %d, want 5", cfg.FlushIntervalSeconds)
	}
	if cfg.BufferMaxEvents != 1000000 {
		t.Errorf("BufferMaxEvents = %d, want 1000000", cfg.BufferMaxEvents)
	}
	if cfg.Prefetch != 100 {
		t.Errorf("Prefetch = %d, want 100", cfg.Prefetch)
	}
	if cfg.UnhealthyThresholdSeconds != 600 {
		t.Errorf("UnhealthyThresholdSeconds = %d, want 600", cfg.UnhealthyThresholdSeconds)
	}
	if err := cfg.ValidateServe(); err != nil {
		t.Errorf("ValidateServe() error = %v, want nil", err)
	}
}

func TestLoadReadsExplicitValues(t *testing.T) {
	setEnv(t, withVars(map[string]string{
		"TALLY_LOG_LEVEL":                 "DEBUG",
		"TALLY_METRICS_ENABLED":           "false",
		"TALLY_OSC_HTTP_PORT":             "9090",
		"TALLY_OSC_EXCHANGES":             "nova,octavia",
		"TALLY_OSC_TOPICS":                "notifications.info,notifications.error",
		"TALLY_OSC_BATCH_MAX":             "50",
		"TALLY_OSC_FLUSH_INTERVAL_S":      "1",
		"TALLY_OSC_BUFFER_MAX_EVENTS":     "250000",
		"TALLY_OSC_PREFETCH":              "10",
		"TALLY_OSC_UNHEALTHY_THRESHOLD_S": "30",
	}))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}

	if cfg.LogLevel != "DEBUG" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "DEBUG")
	}
	if cfg.MetricsEnabled {
		t.Error("MetricsEnabled = true, want false")
	}
	if cfg.HTTPPort != 9090 {
		t.Errorf("HTTPPort = %d, want 9090", cfg.HTTPPort)
	}
	if want := []string{"nova", "octavia"}; !slices.Equal(cfg.Exchanges, want) {
		t.Errorf("Exchanges = %q, want %q", cfg.Exchanges, want)
	}
	if want := []string{"notifications.info", "notifications.error"}; !slices.Equal(cfg.Topics, want) {
		t.Errorf("Topics = %q, want %q", cfg.Topics, want)
	}
	if cfg.BatchMax != 50 {
		t.Errorf("BatchMax = %d, want 50", cfg.BatchMax)
	}
	if cfg.FlushIntervalSeconds != 1 {
		t.Errorf("FlushIntervalSeconds = %d, want 1", cfg.FlushIntervalSeconds)
	}
	if cfg.BufferMaxEvents != 250000 {
		t.Errorf("BufferMaxEvents = %d, want 250000", cfg.BufferMaxEvents)
	}
	if cfg.Prefetch != 10 {
		t.Errorf("Prefetch = %d, want 10", cfg.Prefetch)
	}
	if cfg.UnhealthyThresholdSeconds != 30 {
		t.Errorf("UnhealthyThresholdSeconds = %d, want 30", cfg.UnhealthyThresholdSeconds)
	}
}

func TestLoadRejectsAnUnparsablePort(t *testing.T) {
	setEnv(t, withVars(map[string]string{"TALLY_OSC_HTTP_PORT": "http"}))

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want an error")
	}
	if prefix := "parsing the environment:"; !strings.HasPrefix(err.Error(), prefix) {
		t.Errorf("Load() error = %q, want it to start with %q", err, prefix)
	}
}

// TestLoadRejectsABatchSizePastTheAPICap covers both ends of the batch bound: a
// batch of zero never posts anything, and one past the cap is refused by the
// ingest API on every flush.
func TestLoadRejectsABatchSizePastTheAPICap(t *testing.T) {
	t.Run("rejected", func(t *testing.T) {
		for _, value := range []string{"0", "-1", "1001"} {
			t.Run(value, func(t *testing.T) {
				setEnv(t, withVars(map[string]string{"TALLY_OSC_BATCH_MAX": value}))

				_, err := Load()
				if err == nil {
					t.Fatal("Load() error = nil, want an error")
				}
				if !strings.Contains(err.Error(), "TALLY_OSC_BATCH_MAX") {
					t.Errorf("Load() error = %q, want it to name TALLY_OSC_BATCH_MAX", err)
				}
			})
		}
	})

	t.Run("accepted", func(t *testing.T) {
		for _, tc := range []struct {
			value string
			want  int
		}{
			{value: "1", want: 1},
			{value: "1000", want: 1000},
		} {
			t.Run(tc.value, func(t *testing.T) {
				setEnv(t, withVars(map[string]string{"TALLY_OSC_BATCH_MAX": tc.value}))

				cfg, err := Load()
				if err != nil {
					t.Fatalf("Load() error = %v, want nil", err)
				}
				if cfg.BatchMax != tc.want {
					t.Errorf("BatchMax = %d, want %d", cfg.BatchMax, tc.want)
				}
			})
		}
	})
}

func TestLoadRejectsNonPositiveBounds(t *testing.T) {
	tests := []struct {
		name  string
		vars  map[string]string
		wants string
	}{
		{
			name:  "a zero flush interval leaves the sender no interval to wait",
			vars:  map[string]string{"TALLY_OSC_FLUSH_INTERVAL_S": "0"},
			wants: "TALLY_OSC_FLUSH_INTERVAL_S",
		},
		{
			name:  "a negative flush interval behaves the same way",
			vars:  map[string]string{"TALLY_OSC_FLUSH_INTERVAL_S": "-5"},
			wants: "TALLY_OSC_FLUSH_INTERVAL_S",
		},
		{
			name:  "a zero buffer bound stops consumption while the outbox is empty",
			vars:  map[string]string{"TALLY_OSC_BUFFER_MAX_EVENTS": "0"},
			wants: "TALLY_OSC_BUFFER_MAX_EVENTS",
		},
		{
			name:  "a negative buffer bound behaves the same way",
			vars:  map[string]string{"TALLY_OSC_BUFFER_MAX_EVENTS": "-1"},
			wants: "TALLY_OSC_BUFFER_MAX_EVENTS",
		},
		{
			name:  "a zero prefetch means unlimited to the broker",
			vars:  map[string]string{"TALLY_OSC_PREFETCH": "0"},
			wants: "TALLY_OSC_PREFETCH",
		},
		{
			name:  "a negative prefetch is not a QoS the broker accepts",
			vars:  map[string]string{"TALLY_OSC_PREFETCH": "-10"},
			wants: "TALLY_OSC_PREFETCH",
		},
		{
			name:  "a zero unhealthy threshold would fail liveness on the first outage",
			vars:  map[string]string{"TALLY_OSC_UNHEALTHY_THRESHOLD_S": "0"},
			wants: "TALLY_OSC_UNHEALTHY_THRESHOLD_S",
		},
		{
			name:  "a negative unhealthy threshold behaves the same way",
			vars:  map[string]string{"TALLY_OSC_UNHEALTHY_THRESHOLD_S": "-1"},
			wants: "TALLY_OSC_UNHEALTHY_THRESHOLD_S",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setEnv(t, withVars(tc.vars))

			_, err := Load()
			if err == nil {
				t.Fatal("Load() error = nil, want an error")
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("Load() error = %q, want it to name %q", err, tc.wants)
			}
		})
	}
}

func TestLoadRejectsAnUnknownLogLevel(t *testing.T) {
	for _, level := range []string{"TRACE", "info"} {
		t.Run(level, func(t *testing.T) {
			setEnv(t, withVars(map[string]string{"TALLY_LOG_LEVEL": level}))

			_, err := Load()
			if err == nil {
				t.Fatal("Load() error = nil, want an error")
			}
			if !strings.Contains(err.Error(), level) {
				t.Errorf("Load() error = %q, want it to name %q", err, level)
			}
		})
	}
}

func TestLoadResolvesFileSecrets(t *testing.T) {
	secrets := []struct {
		name  string
		value string
		read  func(Config) string
	}{
		{
			name:  "TALLY_OSC_AMQP_URL",
			value: "amqp://tally:s3cret@rabbit:5672/",
			read:  func(c Config) string { return c.AMQPURL },
		},
		{
			name:  "TALLY_OSC_TOKEN",
			value: "t0ken",
			read:  func(c Config) string { return c.Token },
		},
	}

	for _, secret := range secrets {
		t.Run(secret.name, func(t *testing.T) {
			fileVar := secret.name + "_FILE"

			t.Run("the value comes from the file with the trailing newline trimmed", func(t *testing.T) {
				vars := withVars(map[string]string{fileVar: writeSecret(t, secret.value+"\n")})
				delete(vars, secret.name)
				setEnv(t, vars)

				cfg, err := Load()
				if err != nil {
					t.Fatalf("Load() error = %v, want nil", err)
				}
				if got := secret.read(cfg); got != secret.value {
					t.Errorf("%s = %q, want %q", secret.name, got, secret.value)
				}
			})

			t.Run("the variable and its file companion are mutually exclusive", func(t *testing.T) {
				setEnv(t, withVars(map[string]string{fileVar: writeSecret(t, secret.value)}))

				_, err := Load()
				if err == nil {
					t.Fatal("Load() error = nil, want an error")
				}
				want := "set " + secret.name + " or " + fileVar + ", not both"
				if err.Error() != want {
					t.Errorf("Load() error = %q, want %q", err, want)
				}
			})

			t.Run("a missing file is reported with its cause", func(t *testing.T) {
				vars := withVars(map[string]string{fileVar: filepath.Join(t.TempDir(), "absent")})
				delete(vars, secret.name)
				setEnv(t, vars)

				_, err := Load()
				if err == nil {
					t.Fatal("Load() error = nil, want an error")
				}
				if prefix := "reading " + fileVar + ":"; !strings.HasPrefix(err.Error(), prefix) {
					t.Errorf("Load() error = %q, want it to start with %q", err, prefix)
				}
				if !errors.Is(err, os.ErrNotExist) {
					t.Errorf("Load() error = %v, want it to wrap os.ErrNotExist", err)
				}
			})

			t.Run("an empty file is rejected", func(t *testing.T) {
				for _, tc := range []struct {
					name    string
					content string
				}{
					{name: "no bytes", content: ""},
					{name: "only a newline", content: "\n"},
				} {
					t.Run(tc.name, func(t *testing.T) {
						vars := withVars(map[string]string{fileVar: writeSecret(t, tc.content)})
						delete(vars, secret.name)
						setEnv(t, vars)

						_, err := Load()
						if err == nil {
							t.Fatal("Load() error = nil, want an error")
						}
						if !strings.Contains(err.Error(), fileVar) {
							t.Errorf("Load() error = %q, want it to name %q", err, fileVar)
						}
					})
				}
			})
		})
	}
}

func TestValidateServe(t *testing.T) {
	tests := []struct {
		name    string
		missing string
	}{
		{name: "a complete configuration passes"},
		{name: "the broker url is required", missing: "TALLY_OSC_AMQP_URL"},
		{name: "the cloud is required", missing: "TALLY_OSC_CLOUD"},
		{name: "the reporting url is required", missing: "TALLY_OSC_REPORTING_URL"},
		{name: "the token is required", missing: "TALLY_OSC_TOKEN"},
		{name: "the buffer path is required", missing: "TALLY_OSC_BUFFER_PATH"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			vars := serveVars()
			delete(vars, tc.missing)
			setEnv(t, vars)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() error = %v, want nil", err)
			}

			err = cfg.ValidateServe()
			switch {
			case tc.missing == "" && err != nil:
				t.Errorf("ValidateServe() error = %v, want nil", err)
			case tc.missing != "" && err == nil:
				t.Errorf("ValidateServe() error = nil, want it to name %s", tc.missing)
			case tc.missing != "" && !strings.Contains(err.Error(), tc.missing):
				t.Errorf("ValidateServe() error = %q, want it to name %s", err, tc.missing)
			}
		})
	}
}

// TestValidateServeChecksTheReportingURL covers the value the sender builds its
// endpoint from and sends the ingest token to: a shape it can post to at all,
// and a scheme that does not put the token on the wire in cleartext.
func TestValidateServeChecksTheReportingURL(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		insecure string
		wants    string
	}{
		{name: "an https URL passes", url: "https://tally.example.com"},
		{
			name:     "an http URL passes once it is allowed explicitly",
			url:      "http://tally-reporting.tally.svc:8080",
			insecure: "true",
		},
		{
			name:  "an http URL is refused by default",
			url:   "http://tally.example.com",
			wants: envReportingInsecure,
		},
		{
			name:  "a host without a scheme is refused",
			url:   "tally-reporting:8080",
			wants: envReportingURL,
		},
		{
			name:  "a scheme that is not http(s) is refused",
			url:   "ftp://tally.example.com",
			wants: envReportingURL,
		},
		{
			name:     "a plaintext scheme is still refused when it names no host",
			url:      "http://",
			insecure: "true",
			wants:    envReportingURL,
		},
		// The ingest path is appended to this value, so a query would swallow it:
		// https://host?tenant=acme becomes a POST to / with the query
		// tenant=acme/api/v1/events, which the API answers 404 and the sender
		// retries until the outbox is full. A base path is not the same thing and
		// stays allowed, because that is how a reverse proxy prefix is written.
		{
			name:  "a query is refused",
			url:   "https://tally.example.com?tenant=acme",
			wants: envReportingURL,
		},
		{
			name:  "a fragment is refused",
			url:   "https://tally.example.com#tenant",
			wants: envReportingURL,
		},
		// An empty query or fragment swallows the appended path just as well, and
		// url.Parse hides both: a trailing "?" is recorded in ForceQuery and leaves
		// RawQuery empty, and a trailing "#" parses to an empty Fragment.
		// https://host# becomes https://host#/api/v1/events, which the client sends
		// as a POST to / with the ingest path as its fragment.
		{
			name:  "a trailing question mark is refused",
			url:   "https://tally.example.com?",
			wants: envReportingURL,
		},
		{
			name:  "a trailing hash is refused",
			url:   "https://tally.example.com#",
			wants: envReportingURL,
		},
		{name: "a base path passes", url: "https://tally.example.com/reporting"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setEnv(t, withVars(map[string]string{
				"TALLY_OSC_REPORTING_URL":      tc.url,
				"TALLY_OSC_REPORTING_INSECURE": tc.insecure,
			}))

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() error = %v, want nil", err)
			}

			err = cfg.ValidateServe()
			switch {
			case tc.wants == "" && err != nil:
				t.Errorf("ValidateServe() error = %v, want nil for %q", err, tc.url)
			case tc.wants != "" && err == nil:
				t.Errorf("ValidateServe() error = nil, want it to name %s for %q", tc.wants, tc.url)
			case tc.wants != "" && !strings.Contains(err.Error(), tc.wants):
				t.Errorf("ValidateServe() error = %q, want it to name %s", err, tc.wants)
			}
		})
	}
}

// TestLoadTrimsTheReportingURL guards the endpoint the sender concatenates: a
// base URL with the trailing slash an operator naturally writes would otherwise
// build //api/v1/events, which the Reporting API routes nowhere and the sender
// retries for as long as it runs.
func TestLoadTrimsTheReportingURL(t *testing.T) {
	setEnv(t, withVars(map[string]string{"TALLY_OSC_REPORTING_URL": "https://tally.example.com//"}))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}

	if want := "https://tally.example.com"; cfg.ReportingURL != want {
		t.Errorf("ReportingURL = %q, want %q", cfg.ReportingURL, want)
	}
}

func TestValidateDump(t *testing.T) {
	t.Run("the broker url alone is enough", func(t *testing.T) {
		setEnv(t, map[string]string{"TALLY_OSC_AMQP_URL": "amqp://tally:s3cret@rabbit:5672/"})

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() error = %v, want nil", err)
		}
		if err := cfg.ValidateDump(); err != nil {
			t.Errorf("ValidateDump() error = %v, want nil", err)
		}
	})

	t.Run("the broker url is required", func(t *testing.T) {
		vars := serveVars()
		delete(vars, "TALLY_OSC_AMQP_URL")
		setEnv(t, vars)

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() error = %v, want nil", err)
		}

		err = cfg.ValidateDump()
		if err == nil {
			t.Fatal("ValidateDump() error = nil, want an error")
		}
		if !strings.Contains(err.Error(), "TALLY_OSC_AMQP_URL") {
			t.Errorf("ValidateDump() error = %q, want it to name TALLY_OSC_AMQP_URL", err)
		}
	})
}

func TestSlogLevel(t *testing.T) {
	tests := []struct {
		level string
		want  slog.Level
	}{
		{level: "DEBUG", want: slog.LevelDebug},
		{level: "INFO", want: slog.LevelInfo},
		{level: "WARN", want: slog.LevelWarn},
		{level: "ERROR", want: slog.LevelError},
	}

	for _, tc := range tests {
		t.Run(tc.level, func(t *testing.T) {
			setEnv(t, withVars(map[string]string{"TALLY_LOG_LEVEL": tc.level}))

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() error = %v, want nil", err)
			}
			if got := cfg.SlogLevel(); got != tc.want {
				t.Errorf("SlogLevel() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestEnvNamesCoversEveryField keeps the exported list and the struct tags from
// drifting apart. A variable missing from EnvNames is one the tests stop
// blanking, which makes them depend on the shell they run in.
func TestEnvNamesCoversEveryField(t *testing.T) {
	fields := reflect.TypeFor[Config]()
	tagged := make(map[string]bool, fields.NumField())

	for i := range fields.NumField() {
		name, ok := fields.Field(i).Tag.Lookup("env")
		if !ok {
			t.Errorf("field %s carries no env tag", fields.Field(i).Name)
			continue
		}
		tagged[name] = true
		if !slices.Contains(EnvNames, name) {
			t.Errorf("EnvNames is missing %s, which field %s reads", name, fields.Field(i).Name)
		}
	}

	for _, name := range EnvNames {
		// The *_FILE companions have no field of their own.
		if !tagged[name] && !tagged[strings.TrimSuffix(name, fileSuffix)] {
			t.Errorf("EnvNames holds %s, which no field reads", name)
		}
	}
}
