package config_test

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/b42labs/tally/internal/engine/config"
)

// setEnv applies vars and removes every other variable the package reads, so a
// test never inherits a value from the developer's shell. t.Setenv restores the
// previous environment when the test ends; removing a variable afterwards keeps
// that restoration intact, and it lets a test set a variable to the empty
// string to mean the empty value rather than the default.
func setEnv(t *testing.T, vars map[string]string) {
	t.Helper()

	for name := range vars {
		if !slices.Contains(config.EnvNames, name) {
			t.Fatalf("test sets %s, which the package does not read", name)
		}
	}
	for _, name := range config.EnvNames {
		value, set := vars[name]
		t.Setenv(name, value)
		if set {
			continue
		}
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("Unsetenv(%s): %v", name, err)
		}
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

func TestLoadDefaults(t *testing.T) {
	setEnv(t, nil)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}

	if cfg.LogLevel != "INFO" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "INFO")
	}
	if cfg.GraceHours != 72 {
		t.Errorf("GraceHours = %d, want 72", cfg.GraceHours)
	}
	if cfg.AutoFinalize {
		t.Error("AutoFinalize = true, want false")
	}
	if want := []string{"infrastructure_tenant"}; !slices.Equal(cfg.AttributingRelationTypes, want) {
		t.Errorf("AttributingRelationTypes = %q, want %q", cfg.AttributingRelationTypes, want)
	}
	if want := []string{"managed_by", "member_of"}; !slices.Equal(cfg.AdjustmentRelationTypes, want) {
		t.Errorf("AdjustmentRelationTypes = %q, want %q", cfg.AdjustmentRelationTypes, want)
	}
	if cfg.AdjustmentDepth != 3 {
		t.Errorf("AdjustmentDepth = %d, want 3", cfg.AdjustmentDepth)
	}
	if want := "/etc/tally/counter-sources.yaml"; cfg.CounterSourcesPath != want {
		t.Errorf("CounterSourcesPath = %q, want %q", cfg.CounterSourcesPath, want)
	}
}

func TestLoadResolvesFileSecrets(t *testing.T) {
	t.Run("the engine database url comes from the file with the trailing newline trimmed", func(t *testing.T) {
		setEnv(t, map[string]string{
			"TALLY_ENGINE_DB_URL_FILE": writeSecret(t, "postgres://tally@localhost/engine\n"),
		})

		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("Load() error = %v, want nil", err)
		}
		if want := "postgres://tally@localhost/engine"; cfg.DBURL != want {
			t.Errorf("DBURL = %q, want %q", cfg.DBURL, want)
		}
	})

	t.Run("the reporting database url comes from the file without a trailing newline", func(t *testing.T) {
		setEnv(t, map[string]string{
			"TALLY_ENGINE_REPORTING_DB_URL_FILE": writeSecret(t, "postgres://tally-ro@localhost/tally"),
		})

		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("Load() error = %v, want nil", err)
		}
		if want := "postgres://tally-ro@localhost/tally"; cfg.ReportingDBURL != want {
			t.Errorf("ReportingDBURL = %q, want %q", cfg.ReportingDBURL, want)
		}
	})

	t.Run("the variable and its file companion are mutually exclusive", func(t *testing.T) {
		setEnv(t, map[string]string{
			"TALLY_ENGINE_DB_URL":      "postgres://tally@localhost/engine",
			"TALLY_ENGINE_DB_URL_FILE": writeSecret(t, "postgres://tally@db/engine\n"),
		})

		_, err := config.Load()
		if err == nil {
			t.Fatal("Load() error = nil, want an error")
		}
		want := "set TALLY_ENGINE_DB_URL or TALLY_ENGINE_DB_URL_FILE, not both"
		if err.Error() != want {
			t.Errorf("Load() error = %q, want %q", err, want)
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
				path := writeSecret(t, tc.content)
				setEnv(t, map[string]string{"TALLY_ENGINE_DB_URL_FILE": path})

				_, err := config.Load()
				if err == nil {
					t.Fatal("Load() error = nil, want an error")
				}
				want := fmt.Sprintf("TALLY_ENGINE_DB_URL_FILE: file %s is empty", path)
				if err.Error() != want {
					t.Errorf("Load() error = %q, want %q", err, want)
				}
			})
		}
	})
}

func TestNegativeGraceHoursRefused(t *testing.T) {
	setEnv(t, map[string]string{
		"TALLY_ENGINE_DB_URL":      "postgres://tally@localhost/engine",
		"TALLY_ENGINE_GRACE_HOURS": "-1",
	})

	_, err := config.Load()
	if err == nil {
		t.Fatal("Load() error = nil, want an error")
	}
	if !strings.Contains(err.Error(), "TALLY_ENGINE_GRACE_HOURS") {
		t.Errorf("Load() error = %q, want it to name TALLY_ENGINE_GRACE_HOURS", err)
	}
}

func TestAttributingTypesExplicitEmpty(t *testing.T) {
	t.Run("an explicitly empty value attributes nothing", func(t *testing.T) {
		setEnv(t, map[string]string{
			"TALLY_ENGINE_DB_URL":                     "postgres://tally@localhost/engine",
			"TALLY_ENGINE_ATTRIBUTING_RELATION_TYPES": "",
		})

		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("Load() error = %v, want nil", err)
		}
		if len(cfg.AttributingRelationTypes) != 0 {
			t.Errorf("AttributingRelationTypes = %q, want the empty list", cfg.AttributingRelationTypes)
		}
	})

	t.Run("a stray comma is rejected", func(t *testing.T) {
		setEnv(t, map[string]string{
			"TALLY_ENGINE_DB_URL":                     "postgres://tally@localhost/engine",
			"TALLY_ENGINE_ATTRIBUTING_RELATION_TYPES": "infrastructure_tenant,,other",
		})

		_, err := config.Load()
		if err == nil {
			t.Fatal("Load() error = nil, want an error")
		}
		if !strings.Contains(err.Error(), "TALLY_ENGINE_ATTRIBUTING_RELATION_TYPES") {
			t.Errorf("Load() error = %q, want it to name TALLY_ENGINE_ATTRIBUTING_RELATION_TYPES", err)
		}
	})

	t.Run("a virtual relation type is rejected", func(t *testing.T) {
		values := []string{"member_of", "managed_by", "infrastructure_tenant,managed_by"}

		for _, value := range values {
			t.Run(value, func(t *testing.T) {
				setEnv(t, map[string]string{
					"TALLY_ENGINE_DB_URL":                     "postgres://tally@localhost/engine",
					"TALLY_ENGINE_ATTRIBUTING_RELATION_TYPES": value,
				})

				_, err := config.Load()
				if err == nil {
					t.Fatal("Load() error = nil, want an error")
				}
				if !strings.Contains(err.Error(), "TALLY_ENGINE_ATTRIBUTING_RELATION_TYPES") {
					t.Errorf("Load() error = %q, want it to name TALLY_ENGINE_ATTRIBUTING_RELATION_TYPES", err)
				}
				if want := "reaches a virtual project and attributes no cost"; !strings.Contains(err.Error(), want) {
					t.Errorf("Load() error = %q, want it to contain %q", err, want)
				}
			})
		}
	})

	t.Run("the default is kept when the variable is unset", func(t *testing.T) {
		setEnv(t, map[string]string{"TALLY_ENGINE_DB_URL": "postgres://tally@localhost/engine"})

		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("Load() error = %v, want nil", err)
		}
		if want := []string{"infrastructure_tenant"}; !slices.Equal(cfg.AttributingRelationTypes, want) {
			t.Errorf("AttributingRelationTypes = %q, want %q", cfg.AttributingRelationTypes, want)
		}
	})
}

func TestAdjustmentTypesExplicitEmpty(t *testing.T) {
	t.Run("an explicitly empty value turns adjustments off", func(t *testing.T) {
		setEnv(t, map[string]string{
			"TALLY_ENGINE_DB_URL":                    "postgres://tally@localhost/engine",
			"TALLY_ENGINE_ADJUSTMENT_RELATION_TYPES": "",
		})

		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("Load() error = %v, want nil", err)
		}
		if len(cfg.AdjustmentRelationTypes) != 0 {
			t.Errorf("AdjustmentRelationTypes = %q, want the empty list", cfg.AdjustmentRelationTypes)
		}
	})

	t.Run("a named list replaces the default", func(t *testing.T) {
		tests := []struct {
			value string
			want  []string
		}{
			{value: "managed_by", want: []string{"managed_by"}},
			// The list is independent of the attributing one, so a type named in
			// both is walked by attribution and by adjustment resolution.
			{value: "infrastructure_tenant,member_of", want: []string{"infrastructure_tenant", "member_of"}},
		}

		for _, tc := range tests {
			t.Run(tc.value, func(t *testing.T) {
				setEnv(t, map[string]string{
					"TALLY_ENGINE_DB_URL":                    "postgres://tally@localhost/engine",
					"TALLY_ENGINE_ADJUSTMENT_RELATION_TYPES": tc.value,
				})

				cfg, err := config.Load()
				if err != nil {
					t.Fatalf("Load() error = %v, want nil", err)
				}
				if !slices.Equal(cfg.AdjustmentRelationTypes, tc.want) {
					t.Errorf("AdjustmentRelationTypes = %q, want %q", cfg.AdjustmentRelationTypes, tc.want)
				}
			})
		}
	})

	t.Run("a stray comma is rejected", func(t *testing.T) {
		setEnv(t, map[string]string{
			"TALLY_ENGINE_DB_URL":                    "postgres://tally@localhost/engine",
			"TALLY_ENGINE_ADJUSTMENT_RELATION_TYPES": "managed_by,,member_of",
		})

		_, err := config.Load()
		if err == nil {
			t.Fatal("Load() error = nil, want an error")
		}
		if !strings.Contains(err.Error(), "TALLY_ENGINE_ADJUSTMENT_RELATION_TYPES") {
			t.Errorf("Load() error = %q, want it to name TALLY_ENGINE_ADJUSTMENT_RELATION_TYPES", err)
		}
		if want := "must not contain an empty relation type"; !strings.Contains(err.Error(), want) {
			t.Errorf("Load() error = %q, want it to contain %q", err, want)
		}
	})
}

func TestAdjustmentDepth(t *testing.T) {
	t.Run("one level is the shallowest walk", func(t *testing.T) {
		setEnv(t, map[string]string{
			"TALLY_ENGINE_DB_URL":           "postgres://tally@localhost/engine",
			"TALLY_ENGINE_ADJUSTMENT_DEPTH": "1",
		})

		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("Load() error = %v, want nil", err)
		}
		if cfg.AdjustmentDepth != 1 {
			t.Errorf("AdjustmentDepth = %d, want 1", cfg.AdjustmentDepth)
		}
	})

	t.Run("a depth below one is rejected", func(t *testing.T) {
		for _, value := range []string{"0", "-1"} {
			t.Run(value, func(t *testing.T) {
				setEnv(t, map[string]string{
					"TALLY_ENGINE_DB_URL":           "postgres://tally@localhost/engine",
					"TALLY_ENGINE_ADJUSTMENT_DEPTH": value,
				})

				_, err := config.Load()
				if err == nil {
					t.Fatal("Load() error = nil, want an error")
				}
				if !strings.Contains(err.Error(), "TALLY_ENGINE_ADJUSTMENT_DEPTH") {
					t.Errorf("Load() error = %q, want it to name TALLY_ENGINE_ADJUSTMENT_DEPTH", err)
				}
				if want := "must be at least 1"; !strings.Contains(err.Error(), want) {
					t.Errorf("Load() error = %q, want it to contain %q", err, want)
				}
			})
		}
	})

	t.Run("a value that is not an integer is rejected", func(t *testing.T) {
		setEnv(t, map[string]string{
			"TALLY_ENGINE_DB_URL":           "postgres://tally@localhost/engine",
			"TALLY_ENGINE_ADJUSTMENT_DEPTH": "three",
		})

		_, err := config.Load()
		if err == nil {
			t.Fatal("Load() error = nil, want an error")
		}
		if want := "parsing the environment"; !strings.Contains(err.Error(), want) {
			t.Errorf("Load() error = %q, want it to contain %q", err, want)
		}
	})
}

func TestCounterSourcesExplicitEmpty(t *testing.T) {
	t.Run("an explicitly empty value configures no counter sources", func(t *testing.T) {
		setEnv(t, map[string]string{
			"TALLY_ENGINE_DB_URL":          "postgres://tally@localhost/engine",
			"TALLY_ENGINE_COUNTER_SOURCES": "",
		})

		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("Load() error = %v, want nil", err)
		}
		if cfg.CounterSourcesPath != "" {
			t.Errorf("CounterSourcesPath = %q, want the empty path", cfg.CounterSourcesPath)
		}
	})

	t.Run("an unset variable keeps the default path", func(t *testing.T) {
		setEnv(t, nil)

		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("Load() error = %v, want nil", err)
		}
		if want := "/etc/tally/counter-sources.yaml"; cfg.CounterSourcesPath != want {
			t.Errorf("CounterSourcesPath = %q, want %q", cfg.CounterSourcesPath, want)
		}
	})
}

func TestLogLevelRejected(t *testing.T) {
	setEnv(t, map[string]string{
		"TALLY_ENGINE_DB_URL": "postgres://tally@localhost/engine",
		"TALLY_LOG_LEVEL":     "info",
	})

	_, err := config.Load()
	if err == nil {
		t.Fatal("Load() error = nil, want an error")
	}
	if !strings.Contains(err.Error(), "TALLY_LOG_LEVEL") {
		t.Errorf("Load() error = %q, want it to name TALLY_LOG_LEVEL", err)
	}
}

func TestValidateDB(t *testing.T) {
	t.Run("the engine database url is required", func(t *testing.T) {
		setEnv(t, map[string]string{"TALLY_ENGINE_REPORTING_DB_URL": "postgres://tally-ro@localhost/tally"})

		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("Load() error = %v, want nil", err)
		}

		err = cfg.ValidateDB()
		if err == nil {
			t.Fatal("ValidateDB() error = nil, want an error")
		}
		if want := "TALLY_ENGINE_DB_URL: must be set"; err.Error() != want {
			t.Errorf("ValidateDB() error = %q, want %q", err, want)
		}
	})

	t.Run("the engine database url alone is enough", func(t *testing.T) {
		setEnv(t, map[string]string{"TALLY_ENGINE_DB_URL": "postgres://tally@localhost/engine"})

		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("Load() error = %v, want nil", err)
		}
		if err := cfg.ValidateDB(); err != nil {
			t.Errorf("ValidateDB() error = %v, want nil", err)
		}
	})
}

func TestValidateReporting(t *testing.T) {
	t.Run("the reporting database url is required", func(t *testing.T) {
		setEnv(t, map[string]string{"TALLY_ENGINE_DB_URL": "postgres://tally@localhost/engine"})

		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("Load() error = %v, want nil", err)
		}

		err = cfg.ValidateReporting()
		if err == nil {
			t.Fatal("ValidateReporting() error = nil, want an error")
		}
		if want := "TALLY_ENGINE_REPORTING_DB_URL: must be set"; err.Error() != want {
			t.Errorf("ValidateReporting() error = %q, want %q", err, want)
		}
	})

	t.Run("the reporting database url alone is enough", func(t *testing.T) {
		setEnv(t, map[string]string{"TALLY_ENGINE_REPORTING_DB_URL": "postgres://tally-ro@localhost/tally"})

		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("Load() error = %v, want nil", err)
		}
		if err := cfg.ValidateReporting(); err != nil {
			t.Errorf("ValidateReporting() error = %v, want nil", err)
		}
	})
}

// TestEnvNamesCoversEveryField keeps the exported list and the struct tags from
// drifting apart. A variable missing from EnvNames is one the tests stop
// removing, which makes them depend on the shell they run in.
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
