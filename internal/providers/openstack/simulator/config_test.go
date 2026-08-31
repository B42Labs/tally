package simulator

import (
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
