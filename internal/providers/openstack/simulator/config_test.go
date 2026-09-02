package simulator

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestValidateRegistrationAsksForWhatARegistrationNeeds covers the gate of
// run --register-projects. Each refusal names the variable an operator sets,
// and the token is the one value no message quotes: an error pasted into a
// ticket must not carry the credential it is about.
func TestValidateRegistrationAsksForWhatARegistrationNeeds(t *testing.T) {
	// The environment of a registration that works, which every refusal below
	// takes one value out of or spoils.
	complete := Config{
		Cloud:        "os-sim",
		ReportingURL: "https://reporting.example",
		APIToken:     "tly_a_secret-of-the-test",
		GardenCloud:  "garden-sim",
	}

	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{
			name: "no Reporting API",
			cfg:  Config{Cloud: "os-sim", APIToken: "tly_a_x", GardenCloud: "garden-sim"},
			want: "TALLY_SIM_REPORTING_URL: must be set when --register-projects is on",
		},
		{
			name: "a Reporting API that is not an HTTP URL",
			cfg: Config{
				Cloud: "os-sim", ReportingURL: "ftp://api",
				APIToken: "tly_a_x", GardenCloud: "garden-sim",
			},
			want: `TALLY_SIM_REPORTING_URL: "ftp://api" must be an absolute http(s) URL with no ` +
				"query or fragment, because the registry route is appended to it",
		},
		{
			name: "a Reporting API without a host",
			cfg: Config{
				Cloud: "os-sim", ReportingURL: "https://",
				APIToken: "tly_a_x", GardenCloud: "garden-sim",
			},
			want: `TALLY_SIM_REPORTING_URL: "https://" must be an absolute http(s) URL with no ` +
				"query or fragment, because the registry route is appended to it",
		},
		{
			name: "no token",
			cfg: Config{
				Cloud: "os-sim", ReportingURL: "https://reporting.example",
				GardenCloud: "garden-sim",
			},
			want: "TALLY_SIM_API_TOKEN: must be set when --register-projects is on",
		},
		{
			name: "no cloud for the Gardener projects",
			cfg: Config{
				Cloud: "os-sim", ReportingURL: "https://reporting.example",
				APIToken: "tly_a_x",
			},
			want: "TALLY_SIM_GARDEN_CLOUD: must be set when --register-projects is on",
		},
		{
			name: "the Gardener projects under the tenants' cloud",
			cfg: Config{
				Cloud: "os-sim", ReportingURL: "https://reporting.example",
				APIToken: "tly_a_x", GardenCloud: "os-sim",
			},
			want: `TALLY_SIM_GARDEN_CLOUD: "os-sim" must differ from TALLY_SIM_CLOUD: ` +
				"a cloud is one installation of one platform",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.ValidateRegistration()
			if err == nil {
				t.Fatalf("ValidateRegistration() error = nil, want %q", tc.want)
			}
			if err.Error() != tc.want {
				t.Errorf("ValidateRegistration() error = %q, want %q", err, tc.want)
			}
			if got := err.Error(); tc.cfg.APIToken != "" && strings.Contains(got, tc.cfg.APIToken) {
				t.Errorf("ValidateRegistration() error = %q, want it to keep the token out", got)
			}
		})
	}

	t.Run("an environment that registers", func(t *testing.T) {
		if err := complete.ValidateRegistration(); err != nil {
			t.Errorf("ValidateRegistration() error = %v, want nil", err)
		}
	})
}

// TestValidateRegistrationChecksTheReportingURL covers the value the registrar
// builds its routes from and sends the api token to: a shape it can post to at
// all, and a scheme that does not put that token on the wire in cleartext. The
// token is of role admin, so a plaintext registration hands whoever reads it
// the whole project registry, which is why plaintext has to be typed for the
// way the collector's is.
func TestValidateRegistrationChecksTheReportingURL(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		insecure bool
		wants    string
	}{
		{name: "an https URL passes", url: "https://reporting.example"},
		{name: "a base path passes", url: "https://reporting.example/reporting"},
		{
			name:     "an http URL passes once it is allowed explicitly",
			url:      "http://127.0.0.1:8080",
			insecure: true,
		},
		{
			name:  "an http URL is refused by default",
			url:   "http://reporting.example",
			wants: envReportingInsecure,
		},
		{
			name:  "a host without a scheme is refused",
			url:   "reporting.example:8080",
			wants: envReportingURL,
		},
		// The registry route is appended to this value, so a query or a fragment
		// swallows it: https://host# becomes https://host#/api/v1/projects, which
		// the client sends as a POST to / carrying the admin token. url.Parse hides
		// the empty forms of both, so the value itself is what is searched.
		{
			name:  "a query is refused",
			url:   "https://reporting.example?tenant=acme",
			wants: envReportingURL,
		},
		{
			name:  "a fragment is refused",
			url:   "https://reporting.example#tenant",
			wants: envReportingURL,
		},
		{
			name:  "a trailing question mark is refused",
			url:   "https://reporting.example?",
			wants: envReportingURL,
		},
		{
			name:  "a trailing hash is refused",
			url:   "https://reporting.example#",
			wants: envReportingURL,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{
				Cloud:             "os-sim",
				ReportingURL:      tc.url,
				ReportingInsecure: tc.insecure,
				APIToken:          "tly_a_secret-of-the-test",
				GardenCloud:       "garden-sim",
			}

			err := cfg.ValidateRegistration()

			if tc.wants == "" {
				if err != nil {
					t.Fatalf("ValidateRegistration() error = %v, want nil for %q", err, tc.url)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateRegistration() error = nil, want %q refused naming %s",
					tc.url, tc.wants)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("ValidateRegistration() error = %q, want it to name %s", err, tc.wants)
			}
			if strings.Contains(err.Error(), cfg.APIToken) {
				t.Errorf("ValidateRegistration() error = %q, want it to keep the token out", err)
			}
		})
	}
}

// TestValidateMetricsAsksForWhatAPushNeeds covers the gate of the metrics push.
// Each refusal names the variable an operator sets, and the Basic password is
// the value no message quotes, for the reason the api token is never quoted.
func TestValidateMetricsAsksForWhatAPushNeeds(t *testing.T) {
	const password = "s3cret-of-the-test"

	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{
			name: "no user",
			cfg:  Config{OTLPURL: "https://otlp.example/v1/metrics", OTLPPassword: password},
			want: "TALLY_SIM_OTLP_USER: must be set when TALLY_SIM_OTLP_URL is set",
		},
		{
			name: "no password",
			cfg:  Config{OTLPURL: "https://otlp.example/v1/metrics", OTLPUser: "tally"},
			want: "TALLY_SIM_OTLP_PASSWORD: must be set when TALLY_SIM_OTLP_URL is set",
		},
		{
			name: "an endpoint that is not a URL",
			cfg:  Config{OTLPURL: "not a url", OTLPUser: "tally", OTLPPassword: password},
			want: "TALLY_SIM_OTLP_URL: must be an absolute http(s) URL with a host",
		},
		{
			name: "an endpoint without a host",
			cfg:  Config{OTLPURL: "https://", OTLPUser: "tally", OTLPPassword: password},
			want: "TALLY_SIM_OTLP_URL: must be an absolute http(s) URL with a host",
		},
		{
			name: "an endpoint that is not HTTP",
			cfg: Config{
				OTLPURL: "ftp://otlp.example/v1/metrics", OTLPUser: "tally", OTLPPassword: password,
			},
			want: "TALLY_SIM_OTLP_URL: must be an absolute http(s) URL with a host",
		},
		{
			name: "an endpoint that carries a token as its user",
			cfg: Config{
				OTLPURL:  "https://" + password + "@otlp.example/v1/metrics",
				OTLPUser: "tally", OTLPPassword: password,
			},
			want: "TALLY_SIM_OTLP_URL: must carry no userinfo, query or fragment; " +
				"the credential of a push belongs in TALLY_SIM_OTLP_USER and TALLY_SIM_OTLP_PASSWORD",
		},
		{
			name: "an endpoint that carries an api key in its query",
			cfg: Config{
				OTLPURL:  "https://otlp.example/v1/metrics?api-key=" + password,
				OTLPUser: "tally", OTLPPassword: password,
			},
			want: "TALLY_SIM_OTLP_URL: must carry no userinfo, query or fragment; " +
				"the credential of a push belongs in TALLY_SIM_OTLP_USER and TALLY_SIM_OTLP_PASSWORD",
		},
		{
			name: "an endpoint that carries a fragment",
			cfg: Config{
				OTLPURL:  "https://otlp.example/v1/metrics#" + password,
				OTLPUser: "tally", OTLPPassword: password,
			},
			want: "TALLY_SIM_OTLP_URL: must carry no userinfo, query or fragment; " +
				"the credential of a push belongs in TALLY_SIM_OTLP_USER and TALLY_SIM_OTLP_PASSWORD",
		},
		{
			name: "a plaintext endpoint that was not allowed",
			cfg: Config{
				OTLPURL: "http://otlp.example/v1/metrics", OTLPUser: "tally", OTLPPassword: password,
			},
			want: "TALLY_SIM_OTLP_URL: must use https, because the Basic password travels on it; " +
				"set TALLY_SIM_OTLP_INSECURE=true to allow plaintext",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.ValidateMetrics()
			if err == nil {
				t.Fatalf("ValidateMetrics() error = nil, want %q", tc.want)
			}
			if err.Error() != tc.want {
				t.Errorf("ValidateMetrics() error = %q, want %q", err, tc.want)
			}
			if got := err.Error(); strings.Contains(got, password) {
				t.Errorf("ValidateMetrics() error = %q, want it to keep the password out", got)
			}
		})
	}

	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{
			name: "a run without a push",
			cfg:  Config{},
		},
		{
			name: "an https endpoint with its credentials",
			cfg: Config{
				OTLPURL: "https://otlp.example/v1/metrics", OTLPUser: "tally", OTLPPassword: password,
			},
		},
		{
			name: "a plaintext endpoint that was allowed",
			cfg: Config{
				OTLPURL: "http://127.0.0.1:4318/v1/metrics", OTLPUser: "tally",
				OTLPPassword: password, OTLPInsecure: true,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.cfg.ValidateMetrics(); err != nil {
				t.Errorf("ValidateMetrics() error = %v, want nil", err)
			}
		})
	}
}

// TestLoadResolvesTheOTLPPasswordFile holds the Basic password to the *_FILE
// convention the other two secrets follow: the file's content becomes the
// value, both sources at once are a mistake, and an empty file is one too,
// because it usually means the Secret was never populated.
func TestLoadResolvesTheOTLPPasswordFile(t *testing.T) {
	t.Run("the file holds the password", func(t *testing.T) {
		blankEnvironment(t)
		t.Setenv(envOTLPPassword+fileSuffix, secretFile(t, "pw\n"))

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() error = %v, want nil", err)
		}
		if cfg.OTLPPassword != "pw" {
			t.Errorf("OTLPPassword = %q, want %q", cfg.OTLPPassword, "pw")
		}
	})

	t.Run("both sources at once", func(t *testing.T) {
		blankEnvironment(t)
		t.Setenv(envOTLPPassword, "pw")
		t.Setenv(envOTLPPassword+fileSuffix, secretFile(t, "pw\n"))

		_, err := Load()
		if err == nil {
			t.Fatal("Load() error = nil, want the two sources refused")
		}
		want := "set TALLY_SIM_OTLP_PASSWORD or TALLY_SIM_OTLP_PASSWORD_FILE, not both"
		if err.Error() != want {
			t.Errorf("Load() error = %q, want %q", err, want)
		}
	})

	t.Run("an empty file", func(t *testing.T) {
		blankEnvironment(t)
		path := secretFile(t, "")
		t.Setenv(envOTLPPassword+fileSuffix, path)

		_, err := Load()
		if err == nil {
			t.Fatal("Load() error = nil, want the empty file refused")
		}
		want := "TALLY_SIM_OTLP_PASSWORD_FILE: file " + path + " is empty"
		if err.Error() != want {
			t.Errorf("Load() error = %q, want %q", err, want)
		}
	})

	t.Run("the defaults of a run that sets neither", func(t *testing.T) {
		blankEnvironment(t)

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() error = %v, want nil", err)
		}
		if !cfg.MetricsEnabled {
			t.Errorf("MetricsEnabled = false, want true: the endpoint is served unless it is turned off")
		}
		if cfg.OTLPInsecure {
			t.Errorf("OTLPInsecure = true, want false: plaintext is typed for, not fallen into")
		}
	})
}

// TestEnvNamesListTheMetricsVariables keeps the metrics variables in the list
// the tests blank and the example file is held against: one that is missing
// from it reaches the code under test out of the developer's shell, and nobody
// documents it.
func TestEnvNamesListTheMetricsVariables(t *testing.T) {
	for _, name := range []string{
		envOTLPURL,
		envOTLPUser,
		envOTLPPassword,
		envOTLPPassword + fileSuffix,
		envOTLPInsecure,
		envMetricsEnabled,
	} {
		if !slices.Contains(EnvNames, name) {
			t.Errorf("EnvNames does not list %s, want it among %v", name, EnvNames)
		}
	}
}

// blankEnvironment blanks every variable this package reads, so a value in the
// developer's shell never reaches Load. A variable set to the empty string
// falls back to its default exactly as an unset one does, which is what the
// blanked TALLY_LOG_LEVEL relies on.
func blankEnvironment(t *testing.T) {
	t.Helper()

	for _, name := range EnvNames {
		t.Setenv(name, "")
	}
}

// secretFile writes content to a file in a temporary directory and returns its
// path, which is what a *_FILE companion points at.
func secretFile(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}
