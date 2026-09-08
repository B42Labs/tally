package config_test

import (
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
var fieldNames = []string{"LogLevel", "HTTPPort", "ReportingURL", "APIToken", "CAFile", "EngineDBURL"}

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

// validEnv is the smallest environment Load accepts: the three values that have
// no default. A case that exercises one variable starts from it and changes
// that one.
func validEnv() map[string]string {
	return map[string]string{
		"TALLY_CONSOLE_REPORTING_URL": "https://api.example.com",
		"TALLY_CONSOLE_API_TOKEN":     "s3cr3t",
		"TALLY_CONSOLE_ENGINE_DB_URL": "postgres://tally:tally@127.0.0.1:1/tally_engine",
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
			want: config.Config{
				LogLevel:     "INFO",
				HTTPPort:     8095,
				ReportingURL: "https://api.example.com",
				APIToken:     "s3cr3t",
				EngineDBURL:  "postgres://tally:tally@127.0.0.1:1/tally_engine",
			},
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
			want: config.Config{
				LogLevel:     "INFO",
				HTTPPort:     8095,
				ReportingURL: "http://127.0.0.1:8080",
				APIToken:     "s3cr3t",
				EngineDBURL:  "postgres://tally:tally@127.0.0.1:1/tally_engine",
			},
		},
		{
			name: "a plaintext localhost URL is accepted",
			env: func(*testing.T) map[string]string {
				vars := validEnv()
				vars["TALLY_CONSOLE_REPORTING_URL"] = "http://localhost:8080"
				return vars
			},
			want: config.Config{
				LogLevel:     "INFO",
				HTTPPort:     8095,
				ReportingURL: "http://localhost:8080",
				APIToken:     "s3cr3t",
				EngineDBURL:  "postgres://tally:tally@127.0.0.1:1/tally_engine",
			},
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
			want: config.Config{
				LogLevel:     "INFO",
				HTTPPort:     8095,
				ReportingURL: "https://api.example.com",
				APIToken:     "file-token",
				EngineDBURL:  "postgres://tally:tally@127.0.0.1:1/tally_engine",
			},
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
			want: config.Config{
				LogLevel:     "INFO",
				HTTPPort:     8095,
				ReportingURL: "https://api.example.com",
				APIToken:     "s3cr3t",
				EngineDBURL:  "postgres://tally@127.0.0.1:1/from_file",
			},
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
