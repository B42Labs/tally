package refdoc

import (
	"strings"
	"testing"
)

// fixtureEnvNames is what the fixture configuration reads, the list the *_FILE
// companion of its secret is looked up in.
var fixtureEnvNames = []string{"TALLY_TEST_DB_URL", "TALLY_TEST_DB_URL_FILE"}

// realConfigs are the configuration sources of this repository, every one of
// which the renderer has to get through.
var realConfigs = []string{
	"../reporting/config/config.go",
	"../engine/config/config.go",
	"../providers/openstack/config.go",
	"../providers/openstack/simulator/config.go",
}

func TestSettings(t *testing.T) {
	got, err := Settings(readFixture(t, "settings.go"), "Config", fixtureEnvNames)
	if err != nil {
		t.Fatalf("Settings() error = %v, want nil", err)
	}

	assertWant(t, "settings.want.md", got)
}

func TestSettingsRendersTheDefaultsAndTheSecret(t *testing.T) {
	got, err := Settings(readFixture(t, "settings.go"), "Config", fixtureEnvNames)
	if err != nil {
		t.Fatalf("Settings() error = %v, want nil", err)
	}

	rows := []string{
		// A variable with a default and a comment holding the column separator.
		"| `TALLY_TEST_LOG_LEVEL` | string | `INFO` | no | LogLevel is the threshold, one of DEBUG \\| INFO. |",
		// A secret: no default, and a *_FILE companion the list holds.
		"| `TALLY_TEST_DB_URL` | string | none | yes (`TALLY_TEST_DB_URL_FILE`) |",
		"| `TALLY_TEST_CLOUDS` | list, comma-separated | `one,two` | no |",
	}
	for _, row := range rows {
		if !strings.Contains(got, row) {
			t.Errorf("the table does not carry %q:\n%s", row, got)
		}
	}
}

func TestSettingsRendersTheConfigurationOfEveryService(t *testing.T) {
	for _, path := range realConfigs {
		t.Run(path, func(t *testing.T) {
			assertRendersRealSource(t, path, func(src []byte) (string, error) {
				return Settings(src, "Config", nil)
			})
		})
	}
}

func TestSettingsRejectsWhatItCannotRender(t *testing.T) {
	cases := map[string]struct {
		src  string
		want string
	}{
		"no such type": {
			src:  "package p\n",
			want: "refdoc: no type Config in the source",
		},
		"not a struct": {
			src:  "package p\n\ntype Config string\n",
			want: "refdoc: Config is not a struct",
		},
		"no env tag": {
			src:  "package p\n\ntype Config struct {\n\t// Port is the port.\n\tPort int\n}\n",
			want: "refdoc: Config carries no env-tagged field",
		},
		"no doc comment": {
			src:  "package p\n\ntype Config struct {\n\tPort int `env:\"TALLY_TEST_PORT\"`\n}\n",
			want: "refdoc: Config.Port has no doc comment",
		},
		"unsupported type": {
			src:  "package p\n\ntype Config struct {\n\t// Ratio is a share.\n\tRatio float64 `env:\"TALLY_TEST_RATIO\"`\n}\n",
			want: "refdoc: Config.Ratio: unsupported type float64",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Settings([]byte(tc.src), "Config", nil)
			if err == nil {
				t.Fatalf("Settings() error = nil, want %q", tc.want)
			}
			if err.Error() != tc.want {
				t.Errorf("Settings() error = %q, want %q", err, tc.want)
			}
		})
	}
}

func TestSettingsReportsWhereTheSourceBreaks(t *testing.T) {
	_, err := Settings([]byte("package p\n\ntype Config struct {\n"), "Config", nil)
	if err == nil {
		t.Fatal("Settings() error = nil, want a parse error")
	}
	if !strings.Contains(err.Error(), sourceName+":") {
		t.Errorf("Settings() error = %q, want it to carry the position in %s", err, sourceName)
	}
}
