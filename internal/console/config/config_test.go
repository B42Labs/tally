package config_test

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/b42labs/tally/internal/console/config"
)

// fieldNames are the Go names of Config's fields. No error may quote one: an
// operator fixes an environment variable, and a field name is not something
// they can set.
var fieldNames = []string{"LogLevel", "HTTPAddr", "HTTPPort", "ReportingURL", "APIToken", "CAFile", "EngineDBURL", "Htpasswd", "OpsPort", "MetricsEnabled", "ReportingInsecure"}

// testHtpasswd is an operator credential. Load checks only that it is present,
// so it does not have to parse.
const testHtpasswd = "operator:hash"

// setEnv applies vars and blanks every other variable the package reads, so a
// test never inherits a value from the developer's shell. A variable set to the
// empty string falls back to its default exactly as an unset one does.
func setEnv(t *testing.T, vars map[string]string) {
	t.Helper()

	for name := range vars {
		if !slices.Contains(config.EnvNames, name) {
			t.Fatalf("test sets %s, which the package does not read", name)
		}
	}
	for _, name := range config.EnvNames {
		t.Setenv(name, vars[name])
	}
}

// validEnv is the smallest environment Load accepts: the four values that have
// no default. A case that exercises one variable starts from it and changes
// that one.
func validEnv() map[string]string {
	return map[string]string{
		"TALLY_CONSOLE_REPORTING_URL": "https://api.example.com",
		"TALLY_CONSOLE_API_TOKEN":     "s3cr3t",
		"TALLY_CONSOLE_ENGINE_DB_URL": "postgres://tally:tally@127.0.0.1:1/tally_engine",
		"TALLY_CONSOLE_HTPASSWD":      testHtpasswd,
	}
}

// writeSecret writes a file-mounted secret and returns its path. The trailing
// newline is the one a Kubernetes Secret volume carries.
func writeSecret(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func TestLoad(t *testing.T) {
	tests := []struct {
		name string
		// env builds the environment the case runs under. It takes the testing
		// handle because the file-backed cases write a temporary file first.
		env func(t *testing.T) map[string]string
		// want is the whole configuration Load has to return. It is checked
		// only when wantErr is empty.
		want config.Config
		// wantErr are substrings the error has to contain.
		wantErr []string
	}{
		{
			name: "the defaults apply when only the required variables are set",
			env:  func(*testing.T) map[string]string { return validEnv() },
			want: wantLoaded(func(*config.Config) {}),
		},
		{
			name: "a lower-case log level is refused",
			env: func(*testing.T) map[string]string {
				vars := validEnv()
				vars["TALLY_LOG_LEVEL"] = "info"
				return vars
			},
			wantErr: []string{"TALLY_LOG_LEVEL", `"info"`},
		},
		{
			name: "port zero is refused",
			env: func(*testing.T) map[string]string {
				vars := validEnv()
				vars["TALLY_CONSOLE_HTTP_PORT"] = "0"
				return vars
			},
			wantErr: []string{"TALLY_CONSOLE_HTTP_PORT", "1 and 65535"},
		},
		{
			name: "a port past the last one is refused",
			env: func(*testing.T) map[string]string {
				vars := validEnv()
				vars["TALLY_CONSOLE_HTTP_PORT"] = "65536"
				return vars
			},
			wantErr: []string{"TALLY_CONSOLE_HTTP_PORT", "1 and 65535"},
		},
		{
			name: "the reporting URL is required",
			env: func(*testing.T) map[string]string {
				vars := validEnv()
				delete(vars, "TALLY_CONSOLE_REPORTING_URL")
				return vars
			},
			wantErr: []string{"TALLY_CONSOLE_REPORTING_URL: must be set"},
		},
		{
			name: "a malformed reporting URL is refused",
			env: func(*testing.T) map[string]string {
				vars := validEnv()
				vars["TALLY_CONSOLE_REPORTING_URL"] = "://bad"
				return vars
			},
			wantErr: []string{"TALLY_CONSOLE_REPORTING_URL", "is not a URL"},
		},
		{
			name: "a reporting URL without a scheme is refused",
			env: func(*testing.T) map[string]string {
				vars := validEnv()
				vars["TALLY_CONSOLE_REPORTING_URL"] = "api.example.com"
				return vars
			},
			wantErr: []string{"TALLY_CONSOLE_REPORTING_URL", "is not a URL"},
		},
		{
			name: "a plaintext reporting URL is refused",
			env: func(*testing.T) map[string]string {
				vars := validEnv()
				vars["TALLY_CONSOLE_REPORTING_URL"] = "http://api.example.com"
				return vars
			},
			wantErr: []string{"TALLY_CONSOLE_API_TOKEN", "cleartext"},
		},
		{
			name: "a plaintext loopback address is accepted",
			env: func(*testing.T) map[string]string {
				vars := validEnv()
				vars["TALLY_CONSOLE_REPORTING_URL"] = "http://127.0.0.1:8080"
				return vars
			},
			want: wantLoaded(func(c *config.Config) { c.ReportingURL = "http://127.0.0.1:8080" }),
		},
		{
			name: "a plaintext localhost URL is accepted",
			env: func(*testing.T) map[string]string {
				vars := validEnv()
				vars["TALLY_CONSOLE_REPORTING_URL"] = "http://localhost:8080"
				return vars
			},
			want: wantLoaded(func(c *config.Config) { c.ReportingURL = "http://localhost:8080" }),
		},
		{
			name: "the three operations settings fall back to their defaults when blank",
			env: func(*testing.T) map[string]string {
				vars := validEnv()
				vars["TALLY_CONSOLE_OPS_PORT"] = ""
				vars["TALLY_METRICS_ENABLED"] = ""
				vars["TALLY_CONSOLE_REPORTING_INSECURE"] = ""
				return vars
			},
			want: wantLoaded(func(*config.Config) {}),
		},
		{
			name: "ops port zero is refused",
			env: func(*testing.T) map[string]string {
				vars := validEnv()
				vars["TALLY_CONSOLE_OPS_PORT"] = "0"
				return vars
			},
			wantErr: []string{"TALLY_CONSOLE_OPS_PORT", "1 and 65535"},
		},
		{
			name: "an ops port past the last one is refused",
			env: func(*testing.T) map[string]string {
				vars := validEnv()
				vars["TALLY_CONSOLE_OPS_PORT"] = "65536"
				return vars
			},
			wantErr: []string{"TALLY_CONSOLE_OPS_PORT", "1 and 65535"},
		},
		{
			name: "an ops port equal to the default page port is refused",
			env: func(*testing.T) map[string]string {
				vars := validEnv()
				vars["TALLY_CONSOLE_OPS_PORT"] = "8095"
				return vars
			},
			wantErr: []string{"TALLY_CONSOLE_OPS_PORT", "must differ from", "TALLY_CONSOLE_HTTP_PORT"},
		},
		{
			name: "a page port equal to the default ops port is refused",
			env: func(*testing.T) map[string]string {
				vars := validEnv()
				vars["TALLY_CONSOLE_HTTP_PORT"] = "8096"
				return vars
			},
			wantErr: []string{"TALLY_CONSOLE_OPS_PORT", "must differ from", "TALLY_CONSOLE_HTTP_PORT"},
		},
		{
			name: "an ops port and a page port of their own are accepted",
			env: func(*testing.T) map[string]string {
				vars := validEnv()
				vars["TALLY_CONSOLE_HTTP_PORT"] = "9000"
				vars["TALLY_CONSOLE_OPS_PORT"] = "9001"
				return vars
			},
			want: wantLoaded(func(c *config.Config) { c.HTTPPort, c.OpsPort = 9000, 9001 }),
		},
		{
			name: "the plaintext switch allows an http URL off the loopback",
			env: func(*testing.T) map[string]string {
				vars := validEnv()
				vars["TALLY_CONSOLE_REPORTING_URL"] = "http://reporting-api"
				vars["TALLY_CONSOLE_REPORTING_INSECURE"] = "true"
				return vars
			},
			want: wantLoaded(func(c *config.Config) {
				c.ReportingURL = "http://reporting-api"
				c.ReportingInsecure = true
			}),
		},
		{
			name: "an http URL off the loopback is refused with the switch unset",
			env: func(*testing.T) map[string]string {
				vars := validEnv()
				vars["TALLY_CONSOLE_REPORTING_URL"] = "http://reporting-api"
				return vars
			},
			wantErr: []string{"TALLY_CONSOLE_API_TOKEN", "cleartext", "set TALLY_CONSOLE_REPORTING_INSECURE=true"},
		},
		{
			name: "an http URL off the loopback is refused with the switch false",
			env: func(*testing.T) map[string]string {
				vars := validEnv()
				vars["TALLY_CONSOLE_REPORTING_URL"] = "http://reporting-api"
				vars["TALLY_CONSOLE_REPORTING_INSECURE"] = "false"
				return vars
			},
			wantErr: []string{"TALLY_CONSOLE_API_TOKEN", "cleartext", "set TALLY_CONSOLE_REPORTING_INSECURE=true"},
		},
		{
			name: "an https URL loads with the switch on",
			env: func(*testing.T) map[string]string {
				vars := validEnv()
				vars["TALLY_CONSOLE_REPORTING_INSECURE"] = "true"
				return vars
			},
			want: wantLoaded(func(c *config.Config) { c.ReportingInsecure = true }),
		},
		{
			name: "an ftp URL on the loopback is refused",
			env: func(*testing.T) map[string]string {
				vars := validEnv()
				vars["TALLY_CONSOLE_REPORTING_URL"] = "ftp://127.0.0.1"
				return vars
			},
			wantErr: []string{"TALLY_CONSOLE_REPORTING_URL", "must be an http or https URL"},
		},
		{
			name: "an ftp URL is refused with the switch on",
			env: func(*testing.T) map[string]string {
				vars := validEnv()
				vars["TALLY_CONSOLE_REPORTING_URL"] = "ftp://reporting-api"
				vars["TALLY_CONSOLE_REPORTING_INSECURE"] = "true"
				return vars
			},
			wantErr: []string{"TALLY_CONSOLE_REPORTING_URL", "must be an http or https URL"},
		},
		{
			name: "the metrics route can be switched off",
			env: func(*testing.T) map[string]string {
				vars := validEnv()
				vars["TALLY_METRICS_ENABLED"] = "false"
				return vars
			},
			want: wantLoaded(func(c *config.Config) { c.MetricsEnabled = false }),
		},
		{
			name: "the API token is required",
			env: func(*testing.T) map[string]string {
				vars := validEnv()
				delete(vars, "TALLY_CONSOLE_API_TOKEN")
				return vars
			},
			wantErr: []string{"TALLY_CONSOLE_API_TOKEN: must be set"},
		},
		{
			name: "the API token is read from the file its companion names",
			env: func(t *testing.T) map[string]string {
				vars := validEnv()
				delete(vars, "TALLY_CONSOLE_API_TOKEN")
				vars["TALLY_CONSOLE_API_TOKEN_FILE"] = writeSecret(t, "file-token\n")
				return vars
			},
			want: wantLoaded(func(c *config.Config) { c.APIToken = "file-token" }),
		},
		{
			name: "an API token given inline and in a file is refused",
			env: func(t *testing.T) map[string]string {
				vars := validEnv()
				vars["TALLY_CONSOLE_API_TOKEN_FILE"] = writeSecret(t, "file-token\n")
				return vars
			},
			wantErr: []string{"TALLY_CONSOLE_API_TOKEN", "TALLY_CONSOLE_API_TOKEN_FILE", "not both"},
		},
		{
			name: "the engine database URL is required",
			env: func(*testing.T) map[string]string {
				vars := validEnv()
				delete(vars, "TALLY_CONSOLE_ENGINE_DB_URL")
				return vars
			},
			wantErr: []string{"TALLY_CONSOLE_ENGINE_DB_URL: must be set"},
		},
		{
			name: "the engine database URL is read from the file its companion names",
			env: func(t *testing.T) map[string]string {
				vars := validEnv()
				delete(vars, "TALLY_CONSOLE_ENGINE_DB_URL")
				vars["TALLY_CONSOLE_ENGINE_DB_URL_FILE"] = writeSecret(t, "postgres://tally@127.0.0.1:1/from_file\n")
				return vars
			},
			want: wantLoaded(func(c *config.Config) { c.EngineDBURL = "postgres://tally@127.0.0.1:1/from_file" }),
		},
		{
			name: "an address set to the empty string is the loopback one",
			env: func(*testing.T) map[string]string {
				vars := validEnv()
				vars["TALLY_CONSOLE_HTTP_ADDR"] = ""
				return vars
			},
			want: wantLoaded(func(*config.Config) {}),
		},
		{
			name: "the IPv4 wildcard address is accepted",
			env: func(*testing.T) map[string]string {
				vars := validEnv()
				vars["TALLY_CONSOLE_HTTP_ADDR"] = "0.0.0.0"
				return vars
			},
			want: wantLoaded(func(c *config.Config) { c.HTTPAddr = "0.0.0.0" }),
		},
		{
			name: "the IPv6 wildcard address is accepted",
			env: func(*testing.T) map[string]string {
				vars := validEnv()
				vars["TALLY_CONSOLE_HTTP_ADDR"] = "::"
				return vars
			},
			want: wantLoaded(func(c *config.Config) { c.HTTPAddr = "::" }),
		},
		{
			name: "a host name is refused as the address",
			env: func(*testing.T) map[string]string {
				vars := validEnv()
				vars["TALLY_CONSOLE_HTTP_ADDR"] = "console.example.com"
				return vars
			},
			wantErr: []string{"TALLY_CONSOLE_HTTP_ADDR", "must be an IP address", `"console.example.com"`},
		},
		{
			name: "a host and port pair is refused as the address",
			env: func(*testing.T) map[string]string {
				vars := validEnv()
				vars["TALLY_CONSOLE_HTTP_ADDR"] = "127.0.0.1:8095"
				return vars
			},
			wantErr: []string{"TALLY_CONSOLE_HTTP_ADDR", "must be an IP address"},
		},
		{
			name: "the operator credential is required",
			env: func(*testing.T) map[string]string {
				vars := validEnv()
				delete(vars, "TALLY_CONSOLE_HTPASSWD")
				return vars
			},
			wantErr: []string{"TALLY_CONSOLE_HTPASSWD: must be set"},
		},
		{
			name: "the operator credential is read from the file its companion names",
			env: func(t *testing.T) map[string]string {
				vars := validEnv()
				delete(vars, "TALLY_CONSOLE_HTPASSWD")
				vars["TALLY_CONSOLE_HTPASSWD_FILE"] = writeSecret(t, "file-operator:hash\n")
				return vars
			},
			want: wantLoaded(func(c *config.Config) { c.Htpasswd = "file-operator:hash" }),
		},
		{
			name: "an operator credential given inline and in a file is refused",
			env: func(t *testing.T) map[string]string {
				vars := validEnv()
				vars["TALLY_CONSOLE_HTPASSWD_FILE"] = writeSecret(t, "file-operator:hash\n")
				return vars
			},
			wantErr: []string{"TALLY_CONSOLE_HTPASSWD", "TALLY_CONSOLE_HTPASSWD_FILE", "not both"},
		},
		{
			name: "an empty file for the operator credential is refused",
			env: func(t *testing.T) map[string]string {
				vars := validEnv()
				delete(vars, "TALLY_CONSOLE_HTPASSWD")
				vars["TALLY_CONSOLE_HTPASSWD_FILE"] = writeSecret(t, "")
				return vars
			},
			wantErr: []string{"TALLY_CONSOLE_HTPASSWD_FILE", "is empty"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setEnv(t, tt.env(t))

			cfg, err := config.Load()

			if len(tt.wantErr) > 0 {
				if err == nil {
					t.Fatalf("Load() error = nil, want one containing %q", tt.wantErr)
				}
				for _, want := range tt.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("Load() error = %q, want it to contain %q", err, want)
					}
				}
				for _, field := range fieldNames {
					if strings.Contains(err.Error(), field) {
						t.Errorf("Load() error = %q, want the variable rather than the field name %s", err, field)
					}
				}
				return
			}

			if err != nil {
				t.Fatalf("Load() error = %v, want nil", err)
			}
			if cfg != tt.want {
				t.Errorf("Load() = %+v, want %+v", cfg, tt.want)
			}
		})
	}
}

// wantLoaded is what Load returns for validEnv, changed by adjust.
func wantLoaded(adjust func(*config.Config)) config.Config {
	want := config.Config{
		LogLevel:       "INFO",
		HTTPAddr:       "127.0.0.1",
		HTTPPort:       8095,
		OpsPort:        8096,
		MetricsEnabled: true,
		ReportingURL:   "https://api.example.com",
		APIToken:       "s3cr3t",
		EngineDBURL:    "postgres://tally:tally@127.0.0.1:1/tally_engine",
		Htpasswd:       testHtpasswd,
	}
	adjust(&want)
	return want
}

// TestLoadDefaultsAnUnsetAddressToLoopback removes the variable itself, because
// setEnv blanks a variable instead of removing it.
func TestLoadDefaultsAnUnsetAddressToLoopback(t *testing.T) {
	setEnv(t, validEnv())
	if err := os.Unsetenv("TALLY_CONSOLE_HTTP_ADDR"); err != nil {
		t.Fatalf("Unsetenv: %v", err)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if cfg.HTTPAddr != "127.0.0.1" {
		t.Errorf("Load() address = %q, want 127.0.0.1", cfg.HTTPAddr)
	}
}

// TestLoadDefaultsTheUnsetOperationsSettings removes the variables themselves,
// for the reason the test above gives.
func TestLoadDefaultsTheUnsetOperationsSettings(t *testing.T) {
	setEnv(t, validEnv())
	for _, name := range []string{"TALLY_CONSOLE_OPS_PORT", "TALLY_METRICS_ENABLED", "TALLY_CONSOLE_REPORTING_INSECURE"} {
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("Unsetenv: %v", err)
		}
	}

	got, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if want := wantLoaded(func(*config.Config) {}); got != want {
		t.Errorf("Load() = %+v, want %+v", got, want)
	}
}

// TestLoadReportsAnUnparsableValue stays out of the TestLoad table, because the
// library names the Go field here and the table forbids that.
func TestLoadReportsAnUnparsableValue(t *testing.T) {
	cases := []struct {
		name, variable, value string
	}{
		{name: "a metrics switch that is no bool", variable: "TALLY_METRICS_ENABLED", value: "maybe"},
		{name: "an ops port that is no number", variable: "TALLY_CONSOLE_OPS_PORT", value: "http"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			vars := validEnv()
			vars[c.variable] = c.value
			setEnv(t, vars)

			_, err := config.Load()
			if err == nil || !strings.Contains(err.Error(), "parsing the environment") {
				t.Errorf("Load() error = %v, want one containing %q", err, "parsing the environment")
			}
		})
	}
}

func TestLoadWrapsAMissingCredentialFile(t *testing.T) {
	vars := validEnv()
	delete(vars, "TALLY_CONSOLE_HTPASSWD")
	vars["TALLY_CONSOLE_HTPASSWD_FILE"] = filepath.Join(t.TempDir(), "absent")
	setEnv(t, vars)

	_, err := config.Load()
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Load() error = %v, want one wrapping os.ErrNotExist", err)
	}
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
		// A Config that never went through Load carries an unchecked level.
		{level: "info", want: slog.LevelInfo},
		{level: "", want: slog.LevelInfo},
	}

	for _, tt := range tests {
		// The zero Config's level has no name to run under.
		name := tt.level
		if name == "" {
			name = "the empty level"
		}
		t.Run(name, func(t *testing.T) {
			if got := (config.Config{LogLevel: tt.level}).SlogLevel(); got != tt.want {
				t.Errorf("SlogLevel() = %v, want %v", got, tt.want)
			}
		})
	}
}
