package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/b42labs/tally/internal/console/signin"
)

// scrape serves one request against m's handler and returns the body.
func scrape(t *testing.T, m *Metrics) string {
	t.Helper()

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	return rec.Body.String()
}

func refusals(reason string) string {
	return `tally_console_sign_in_refusals_total{reason="` + reason + `"}`
}

func TestAFreshRegistryExposesEveryReasonAtZeroAndNoRequest(t *testing.T) {
	got := scrape(t, New(prometheus.NewRegistry()))

	for _, reason := range signin.Reasons {
		if want := refusals(reason) + " 0"; !strings.Contains(got, want) {
			t.Errorf("the first scrape does not carry %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "go_goroutines") {
		t.Errorf("the first scrape does not carry the Go collector:\n%s", got)
	}
	if strings.Contains(got, "tally_console_requests_total{") {
		t.Errorf("the first scrape carries a request sample:\n%s", got)
	}
}

func TestRequestCountsByRouteAndStatus(t *testing.T) {
	m := New(prometheus.NewRegistry())

	m.Request("/project", http.StatusBadGateway)
	m.Request("/project", http.StatusBadGateway)

	want := `tally_console_requests_total{route="/project",status="502"} 2`
	if got := scrape(t, m); !strings.Contains(got, want) {
		t.Errorf("the scrape does not carry %q:\n%s", want, got)
	}
}

func TestRequestRecordsAnEmptyRouteAsUnmatched(t *testing.T) {
	m := New(prometheus.NewRegistry())

	m.Request("", http.StatusNotFound)

	want := `tally_console_requests_total{route="unmatched",status="404"} 1`
	if got := scrape(t, m); !strings.Contains(got, want) {
		t.Errorf("the scrape does not carry %q:\n%s", want, got)
	}
}

func TestSignInRefusedRaisesThatReasonAlone(t *testing.T) {
	m := New(prometheus.NewRegistry())

	m.SignInRefused(signin.ReasonBusy)

	got := scrape(t, m)
	for _, reason := range signin.Reasons {
		value := " 0"
		if reason == signin.ReasonBusy {
			value = " 1"
		}
		if want := refusals(reason) + value; !strings.Contains(got, want) {
			t.Errorf("the scrape does not carry %q:\n%s", want, got)
		}
	}
}

func TestANilMetricsRecordsNothingInsteadOfPanicking(t *testing.T) {
	var m *Metrics

	m.Request("/project", http.StatusOK)
	m.SignInRefused(signin.ReasonBusy)
}

func TestAScrapePastTheInFlightBoundIsRefusedWhicheverHandlerCallServesIt(t *testing.T) {
	// The in-flight bound is a bound only while the scrapes share it, so every
	// scrape goes through a Handler call of its own. A handler built per call
	// would answer the last scrape 200.
	reg := prometheus.NewRegistry()
	m := New(reg)

	// The first maxScrapesInFlight scrapes hold their slot inside the registry
	// until release is closed; a later scrape that reaches it gathers at once.
	entered := make(chan struct{})
	release := make(chan struct{})
	var gathers atomic.Int32
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "test_blocking_gauge",
		Help: "Holds the first scrapes inside the registry.",
	}, func() float64 {
		if gathers.Add(1) <= maxScrapesInFlight {
			entered <- struct{}{}
			<-release
		}
		return 0
	}))

	var wg sync.WaitGroup
	for range maxScrapesInFlight {
		wg.Go(func() {
			m.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/metrics", nil))
		})
		<-entered
	}
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	close(release)
	wg.Wait()

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("a scrape past %d in flight is answered %d, want %d", maxScrapesInFlight, rec.Code, http.StatusServiceUnavailable)
	}
}
