package config_test

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/b42labs/tally/internal/reporting/config"
)

// setEnv applies vars and blanks every other variable the package reads, so a
// test never inherits a value from the developer's shell. t.Setenv restores the
// previous environment when the test ends, and a variable set to the empty
// string falls back to its default exactly as an unset one does.
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

// writeSecret writes a file-mounted secret and returns its path.
func writeSecret(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func TestLoadAppliesDefaults(t *testing.T) {
	setEnv(t, map[string]string{
		"TALLY_REPORTING_DB_URL":         "postgres://tally@localhost/tally",
		"TALLY_REPORTING_INTERNAL_TOKEN": "s3cret",
	})

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}

	if cfg.LogLevel != "INFO" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "INFO")
	}
	if cfg.HTTPPort != 8080 {
		t.Errorf("HTTPPort = %d, want 8080", cfg.HTTPPort)
	}
	if cfg.AuthMode != "enforced" {
		t.Errorf("AuthMode = %q, want %q", cfg.AuthMode, "enforced")
	}
	if cfg.UnhealthyThresholdSeconds != 600 {
		t.Errorf("UnhealthyThresholdSeconds = %d, want 600", cfg.UnhealthyThresholdSeconds)
	}
	if cfg.DBMaxConns != 10 {
		t.Errorf("DBMaxConns = %d, want 10", cfg.DBMaxConns)
	}
	if cfg.OIDCJWKSURL != "" {
		t.Errorf("OIDCJWKSURL = %q, want empty", cfg.OIDCJWKSURL)
	}
	if err := cfg.ValidateServer(); err != nil {
		t.Errorf("ValidateServer() error = %v, want nil", err)
	}
}

func TestLoadReadsExplicitValues(t *testing.T) {
	setEnv(t, map[string]string{
		"TALLY_LOG_LEVEL":                       "DEBUG",
		"TALLY_REPORTING_HTTP_PORT":             "9090",
		"TALLY_REPORTING_DB_URL":                "postgres://tally@db/tally",
		"TALLY_REPORTING_AUTH_MODE":             "disabled",
		"TALLY_REPORTING_UNHEALTHY_THRESHOLD_S": "30",
		"TALLY_REPORTING_DB_MAX_CONNS":          "4",
	})

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}

	if cfg.DBMaxConns != 4 {
		t.Errorf("DBMaxConns = %d, want 4", cfg.DBMaxConns)
	}

	if cfg.LogLevel != "DEBUG" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "DEBUG")
	}
	if cfg.HTTPPort != 9090 {
		t.Errorf("HTTPPort = %d, want 9090", cfg.HTTPPort)
	}
	if cfg.AuthMode != "disabled" {
		t.Errorf("AuthMode = %q, want %q", cfg.AuthMode, "disabled")
	}
	if cfg.UnhealthyThresholdSeconds != 30 {
		t.Errorf("UnhealthyThresholdSeconds = %d, want 30", cfg.UnhealthyThresholdSeconds)
	}
}

func TestLoadRejectsUnparsablePort(t *testing.T) {
	setEnv(t, map[string]string{
		"TALLY_REPORTING_HTTP_PORT": "http",
		"TALLY_REPORTING_DB_URL":    "postgres://tally@localhost/tally",
	})

	_, err := config.Load()
	if err == nil {
		t.Fatal("Load() error = nil, want an error")
	}
	if prefix := "parsing the environment:"; !strings.HasPrefix(err.Error(), prefix) {
		t.Errorf("Load() error = %q, want it to start with %q", err, prefix)
	}
}

func TestLoadRejectsInvalidEnumValues(t *testing.T) {
	tests := []struct {
		name     string
		vars     map[string]string
		wantText string
	}{
		{
			name:     "unknown log level",
			vars:     map[string]string{"TALLY_LOG_LEVEL": "TRACE"},
			wantText: "TRACE",
		},
		{
			name:     "log level in lower case",
			vars:     map[string]string{"TALLY_LOG_LEVEL": "info"},
			wantText: "info",
		},
		{
			name:     "unknown auth mode",
			vars:     map[string]string{"TALLY_REPORTING_AUTH_MODE": "off"},
			wantText: "off",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			vars := map[string]string{"TALLY_REPORTING_DB_URL": "postgres://tally@localhost/tally"}
			for name, value := range tc.vars {
				vars[name] = value
			}
			setEnv(t, vars)

			_, err := config.Load()
			if err == nil {
				t.Fatal("Load() error = nil, want an error")
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("Load() error = %q, want it to name %q", err, tc.wantText)
			}
		})
	}
}

func TestLoadRejectsNonPositiveBounds(t *testing.T) {
	tests := []struct {
		name     string
		vars     map[string]string
		wantText string
	}{
		{
			name:     "a zero unhealthy threshold would fail liveness on the first outage",
			vars:     map[string]string{"TALLY_REPORTING_UNHEALTHY_THRESHOLD_S": "0"},
			wantText: "TALLY_REPORTING_UNHEALTHY_THRESHOLD_S",
		},
		{
			name:     "a negative unhealthy threshold behaves the same way",
			vars:     map[string]string{"TALLY_REPORTING_UNHEALTHY_THRESHOLD_S": "-1"},
			wantText: "TALLY_REPORTING_UNHEALTHY_THRESHOLD_S",
		},
		{
			name:     "a zero pool bound leaves no connection to query with",
			vars:     map[string]string{"TALLY_REPORTING_DB_MAX_CONNS": "0"},
			wantText: "TALLY_REPORTING_DB_MAX_CONNS",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			vars := map[string]string{"TALLY_REPORTING_DB_URL": "postgres://tally@localhost/tally"}
			for name, value := range tc.vars {
				vars[name] = value
			}
			setEnv(t, vars)

			_, err := config.Load()
			if err == nil {
				t.Fatal("Load() error = nil, want an error")
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("Load() error = %q, want it to name %q", err, tc.wantText)
			}
		})
	}
}

// TestEnvNamesCoversEveryField keeps the exported list and the struct tags from
// drifting apart. A variable missing from EnvNames is one the tests stop
// blanking, which makes them depend on the shell they run in.
func TestEnvNamesCoversEveryField(t *testing.T) {
	fields := reflect.TypeFor[config.Config]()
	tagged := make(map[string]bool, fields.NumField())

	for i := range fields.NumField() {
		name, ok := fields.Field(i).Tag.Lookup("env")
		if !ok {
			t.Errorf("field %s carries no env tag", fields.Field(i).Name)
			continue
		}
		tagged[name] = true
		if !slices.Contains(config.EnvNames, name) {
			t.Errorf("EnvNames is missing %s, which field %s reads", name, fields.Field(i).Name)
		}
	}

	for _, name := range config.EnvNames {
		// The *_FILE companions have no field of their own.
		if !tagged[name] && !tagged[strings.TrimSuffix(name, "_FILE")] {
			t.Errorf("EnvNames holds %s, which no field reads", name)
		}
	}
}

func TestLoadResolvesFileSecrets(t *testing.T) {
	t.Run("database url comes from the file with the trailing newline trimmed", func(t *testing.T) {
		setEnv(t, map[string]string{
			"TALLY_REPORTING_DB_URL_FILE":    writeSecret(t, "postgres://tally@localhost/tally\n"),
			"TALLY_REPORTING_INTERNAL_TOKEN": "s3cret",
		})

		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("Load() error = %v, want nil", err)
		}
		if want := "postgres://tally@localhost/tally"; cfg.DBURL != want {
			t.Errorf("DBURL = %q, want %q", cfg.DBURL, want)
		}
	})

	t.Run("internal token comes from the file without a trailing newline", func(t *testing.T) {
		setEnv(t, map[string]string{
			"TALLY_REPORTING_DB_URL":              "postgres://tally@localhost/tally",
			"TALLY_REPORTING_INTERNAL_TOKEN_FILE": writeSecret(t, "s3cret"),
		})

		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("Load() error = %v, want nil", err)
		}
		if cfg.InternalToken != "s3cret" {
			t.Errorf("InternalToken = %q, want %q", cfg.InternalToken, "s3cret")
		}
	})

	t.Run("the variable and its file companion are mutually exclusive", func(t *testing.T) {
		setEnv(t, map[string]string{
			"TALLY_REPORTING_DB_URL":      "postgres://tally@localhost/tally",
			"TALLY_REPORTING_DB_URL_FILE": writeSecret(t, "postgres://tally@db/tally\n"),
		})

		_, err := config.Load()
		if err == nil {
			t.Fatal("Load() error = nil, want an error")
		}
		want := "set TALLY_REPORTING_DB_URL or TALLY_REPORTING_DB_URL_FILE, not both"
		if err.Error() != want {
			t.Errorf("Load() error = %q, want %q", err, want)
		}
	})

	t.Run("a missing file is reported with its cause", func(t *testing.T) {
		setEnv(t, map[string]string{
			"TALLY_REPORTING_DB_URL_FILE": filepath.Join(t.TempDir(), "absent"),
		})

		_, err := config.Load()
		if err == nil {
			t.Fatal("Load() error = nil, want an error")
		}
		if prefix := "reading TALLY_REPORTING_DB_URL_FILE:"; !strings.HasPrefix(err.Error(), prefix) {
			t.Errorf("Load() error = %q, want it to start with %q", err, prefix)
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("Load() error = %v, want it to wrap os.ErrNotExist", err)
		}
	})

	t.Run("an empty file is rejected", func(t *testing.T) {
		tests := []struct {
			name    string
			content string
		}{
			{name: "no bytes", content: ""},
			{name: "only a newline", content: "\n"},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				setEnv(t, map[string]string{
					"TALLY_REPORTING_DB_URL_FILE": writeSecret(t, tc.content),
				})

				_, err := config.Load()
				if err == nil {
					t.Fatal("Load() error = nil, want an error")
				}
				if !strings.Contains(err.Error(), "TALLY_REPORTING_DB_URL_FILE") {
					t.Errorf("Load() error = %q, want it to name TALLY_REPORTING_DB_URL_FILE", err)
				}
			})
		}
	})
}

func TestValidateServer(t *testing.T) {
	tests := []struct {
		name     string
		vars     map[string]string
		wantText string
	}{
		{
			name: "a complete configuration passes",
			vars: map[string]string{
				"TALLY_REPORTING_DB_URL":         "postgres://tally@localhost/tally",
				"TALLY_REPORTING_INTERNAL_TOKEN": "s3cret",
			},
		},
		{
			name:     "the database url is required",
			vars:     map[string]string{"TALLY_REPORTING_INTERNAL_TOKEN": "s3cret"},
			wantText: "TALLY_REPORTING_DB_URL",
		},
		{
			name:     "enforced auth requires the internal token",
			vars:     map[string]string{"TALLY_REPORTING_DB_URL": "postgres://tally@localhost/tally"},
			wantText: "TALLY_REPORTING_INTERNAL_TOKEN",
		},
		{
			name: "disabled auth needs no internal token",
			vars: map[string]string{
				"TALLY_REPORTING_DB_URL":    "postgres://tally@localhost/tally",
				"TALLY_REPORTING_AUTH_MODE": "disabled",
			},
		},
		{
			name: "a configured jwks url refuses startup",
			vars: map[string]string{
				"TALLY_REPORTING_DB_URL":         "postgres://tally@localhost/tally",
				"TALLY_REPORTING_INTERNAL_TOKEN": "s3cret",
				"TALLY_REPORTING_OIDC_JWKS_URL":  "https://example.com/jwks",
			},
			wantText: "not implemented",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setEnv(t, tc.vars)

			cfg, err := config.Load()
			if err != nil {
				t.Fatalf("Load() error = %v, want nil", err)
			}

			err = cfg.ValidateServer()
			switch {
			case tc.wantText == "" && err != nil:
				t.Errorf("ValidateServer() error = %v, want nil", err)
			case tc.wantText != "" && err == nil:
				t.Errorf("ValidateServer() error = nil, want it to mention %q", tc.wantText)
			case tc.wantText != "" && !strings.Contains(err.Error(), tc.wantText):
				t.Errorf("ValidateServer() error = %q, want it to mention %q", err, tc.wantText)
			}
		})
	}
}

func TestValidateAdmin(t *testing.T) {
	t.Run("the database url alone is enough", func(t *testing.T) {
		setEnv(t, map[string]string{"TALLY_REPORTING_DB_URL": "postgres://tally@localhost/tally"})

		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("Load() error = %v, want nil", err)
		}
		if err := cfg.ValidateAdmin(); err != nil {
			t.Errorf("ValidateAdmin() error = %v, want nil", err)
		}
	})

	t.Run("the database url is required", func(t *testing.T) {
		setEnv(t, map[string]string{"TALLY_REPORTING_INTERNAL_TOKEN": "s3cret"})

		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("Load() error = %v, want nil", err)
		}

		err = cfg.ValidateAdmin()
		if err == nil {
			t.Fatal("ValidateAdmin() error = nil, want an error")
		}
		if !strings.Contains(err.Error(), "TALLY_REPORTING_DB_URL") {
			t.Errorf("ValidateAdmin() error = %q, want it to name TALLY_REPORTING_DB_URL", err)
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
			setEnv(t, map[string]string{
				"TALLY_LOG_LEVEL":        tc.level,
				"TALLY_REPORTING_DB_URL": "postgres://tally@localhost/tally",
			})

			cfg, err := config.Load()
			if err != nil {
				t.Fatalf("Load() error = %v, want nil", err)
			}
			if got := cfg.SlogLevel(); got != tc.want {
				t.Errorf("SlogLevel() = %v, want %v", got, tc.want)
			}
		})
	}
}
