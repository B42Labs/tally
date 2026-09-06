package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/b42labs/tally/internal/core/event"
	"github.com/b42labs/tally/internal/refdoc"
)

// completeConfig is a run's input with nothing missing. Every case below is
// refused before the first request, so the URL is never dialled and the pricing
// file is never read: both only have to be non-empty.
func completeConfig() config {
	return config{
		cloud:        "os-prod-eu1",
		project:      "proj-456",
		month:        "2026-03",
		reportingURL: "https://api.example",
		pricingPath:  "absent.yaml",
		token:        testToken,
	}
}

// TestRunRejectsMissingConfiguration holds run to what it refuses before it
// calls the API. Started on an incomplete configuration, a run would report a
// connection or a pricing problem for a flag nobody passed, and an unset
// TALLY_SLICE_TOKEN would reach the reader as an unauthorized request rather
// than as the name of the variable to set.
func TestRunRejectsMissingConfiguration(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*config)
		want   string
	}{
		"a missing cloud":         {change: func(c *config) { c.cloud = "" }, want: "--cloud"},
		"a missing project":       {change: func(c *config) { c.project = "" }, want: "--project"},
		"a missing month":         {change: func(c *config) { c.month = "" }, want: "--month"},
		"a missing reporting URL": {change: func(c *config) { c.reportingURL = "" }, want: "--reporting-url"},
		"a missing pricing file":  {change: func(c *config) { c.pricingPath = "" }, want: "--pricing"},
		// The variable's name is stated rather than read from tokenEnv: renaming
		// it would break every documented run, so the test has to notice.
		"a missing token": {change: func(c *config) { c.token = "" }, want: "TALLY_SLICE_TOKEN"},
		// The month is checked before the pricing file is opened, so these two
		// fail on the month rather than on the path that is not there.
		"a month without its leading zero": {change: func(c *config) { c.month = "2026-3" }, want: `"2026-3"`},
		"a month that does not exist":      {change: func(c *config) { c.month = "2026-13" }, want: `"2026-13"`},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := completeConfig()
			tc.change(&cfg)

			var out bytes.Buffer
			err := run(t.Context(), cfg, &out)
			if err == nil {
				t.Fatalf("run() error = nil, want an error naming %s", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("run() error = %v, want it to name %s", err, tc.want)
			}
			if out.Len() != 0 {
				t.Errorf("run() wrote %q, want nothing written before the configuration is checked", out.String())
			}
		})
	}
}

// TestRunPrintsTheDocumentAndThenFails is the command's stated contract: a run
// whose numbers broke an invariant still writes them, and only then reports the
// failure. An instance whose history carries no size cannot be rated, and the
// numbers of every other instance of the project are what the run is for, so
// the unrateable one becomes a violation next to them rather than a run that
// printed nothing.
func TestRunPrintsTheDocumentAndThenFails(t *testing.T) {
	histories := map[string][]event.Stored{
		"abc-123": {
			storedEvent(t, "e1", "compute.instance.create.end", "2026-03-01T00:00:00Z", "active", goldenSize()),
		},
		// A history the collector joined mid-life: it starts without a create, so
		// no event ever carried a size, and the fold has none to rate.
		"def-456": {
			storedEvent(t, "e1", "compute.instance.power_on", "2026-03-01T00:00:00Z", "active", nil),
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/resources" {
			_, _ = fmt.Fprint(w, `{"items":[{"resource_id":"abc-123"},{"resource_id":"def-456"}],"next_cursor":null}`)
			return
		}
		for resourceID, history := range histories {
			if strings.Contains(r.URL.Path, resourceID) {
				_ = json.NewEncoder(w).Encode(map[string]any{"items": history})
				return
			}
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	var out bytes.Buffer
	err := run(t.Context(), config{
		cloud:        "os-prod-eu1",
		project:      "proj-456",
		month:        "2026-03",
		reportingURL: server.URL,
		pricingPath:  filepath.Join("..", "..", "pricing", "prototype.yaml"),
		token:        testToken,
	}, &out)

	if err == nil {
		t.Fatalf("run() error = nil, want the broken invariants reported")
	}
	if !strings.Contains(err.Error(), "1 broken metering invariants") {
		t.Errorf("run() error = %v, want it to count the broken invariants", err)
	}

	doc := decodeSliceDocument(t, out.String())
	if len(doc.Resources) != 2 {
		t.Fatalf("the document rates %d resources, want both:\n%s", len(doc.Resources), out.String())
	}

	rated, refused := doc.Resources[0], doc.Resources[1]
	if len(rated.Violations) != 0 {
		t.Errorf("abc-123 violations = %v, want none", rated.Violations)
	}
	assertNumber(t, "abc-123.total", rated.Total, "148.80")

	if len(refused.Violations) != 1 || !strings.Contains(refused.Violations[0], "could not be rated") {
		t.Errorf("def-456 violations = %v, want the rating's refusal", refused.Violations)
	}
	if len(refused.Records) != 0 {
		t.Errorf("def-456 carries %d records, want none", len(refused.Records))
	}
	assertNumber(t, "def-456.total", refused.Total, "0.00")
}

// TestRunReportsAHistoryItCouldNotRead keeps one instance the API will not
// serve from withholding the project's numbers. A resource a retention job
// purges between the listing and the walk answers 404, and so does a gateway
// having a bad minute: a run that stopped there would print nothing at all, not
// even the instances it had already rated.
func TestRunReportsAHistoryItCouldNotRead(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/resources":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"items":[{"resource_id":"abc-123"},{"resource_id":"gone-789"}],"next_cursor":null}`)
		case strings.Contains(r.URL.Path, "abc-123"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []event.Stored{
				storedEvent(t, "e1", "compute.instance.create.end", "2026-03-01T00:00:00Z", "active", goldenSize()),
			}})
		default:
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprint(w, `{"type":"urn:tally:error:not_found","title":"Not Found","status":404}`)
		}
	}))
	defer server.Close()

	var out bytes.Buffer
	err := run(t.Context(), config{
		cloud:        "os-prod-eu1",
		project:      "proj-456",
		month:        "2026-03",
		reportingURL: server.URL,
		pricingPath:  filepath.Join("..", "..", "pricing", "prototype.yaml"),
		token:        testToken,
	}, &out)

	if err == nil {
		t.Fatalf("run() error = nil, want the unread history reported")
	}
	if !strings.Contains(err.Error(), "1 broken metering invariants") {
		t.Errorf("run() error = %v, want it to count the broken invariants", err)
	}

	doc := decodeSliceDocument(t, out.String())
	if len(doc.Resources) != 2 {
		t.Fatalf("the document rates %d resources, want both:\n%s", len(doc.Resources), out.String())
	}

	rated, unread := doc.Resources[0], doc.Resources[1]
	if len(rated.Violations) != 0 {
		t.Errorf("abc-123 violations = %v, want none", rated.Violations)
	}
	assertNumber(t, "abc-123.total", rated.Total, "148.80")

	// The failure is named rather than summarized: the reader has to be able to
	// tell a purged resource from a gateway that answered for one call.
	if len(unread.Violations) != 1 {
		t.Fatalf("gone-789 violations = %v, want the fetch's refusal", unread.Violations)
	}
	for _, want := range []string{"could not be read", "urn:tally:error:not_found"} {
		if !strings.Contains(unread.Violations[0], want) {
			t.Errorf("gone-789 violation = %q, want it to name %s", unread.Violations[0], want)
		}
	}
	assertNumber(t, "gone-789.total", unread.Total, "0.00")
}

// TestBrokenResourceBoundsTheViolation keeps one refused answer from growing
// the document without a bound. The text is the API's own diagnosis, and one
// line of it is held per resource until the run encodes the document, so a
// gateway answering every call with an oversized problem detail would otherwise
// take the run out of memory before it printed anything.
func TestBrokenResourceBoundsTheViolation(t *testing.T) {
	t.Run("an oversized violation is cut to the limit", func(t *testing.T) {
		resource := brokenResource("abc-123", strings.Repeat("a", 4*violationLimit))

		if len(resource.Violations) != 1 {
			t.Fatalf("Violations = %v, want one", resource.Violations)
		}
		// The ellipsis is what is left over the limit: it tells the reader they
		// are not looking at the whole of what the API answered.
		if got, want := len(resource.Violations[0]), violationLimit+len("…"); got > want {
			t.Errorf("the violation is %d bytes, want at most %d", got, want)
		}
		if !strings.HasSuffix(resource.Violations[0], "…") {
			t.Error("the violation does not end in the ellipsis that marks the cut")
		}
	})

	// The cut lands on a byte whose position the API chose, so a multi-byte
	// character straddling it must not reach the document half-encoded.
	t.Run("the cut leaves valid UTF-8", func(t *testing.T) {
		resource := brokenResource("abc-123", strings.Repeat("ä", violationLimit))

		if !utf8.ValidString(resource.Violations[0]) {
			t.Errorf("the violation is not valid UTF-8: %q", resource.Violations[0])
		}
	})

	// A violation that fits is the ordinary case, and it is carried over whole.
	t.Run("a violation that fits is left alone", func(t *testing.T) {
		violation := "the history could not be read: the API answered 404"

		resource := brokenResource("abc-123", violation)

		if len(resource.Violations) != 1 || resource.Violations[0] != violation {
			t.Errorf("Violations = %v, want [%q]", resource.Violations, violation)
		}
	})
}

// TestReferencePageIsCurrent holds the command line reference page of this
// binary to the flags it documents. A flag added here without the page being
// regenerated fails this test rather than leaving a reader with a set the
// binary no longer parses.
func TestReferencePageIsCurrent(t *testing.T) {
	text, err := refdoc.Flags(newFlagSet(&config{}))
	if err != nil {
		t.Fatalf("rendering the flag set: %v", err)
	}

	refdoc.Verify(t, "../../docs/reference/command-line/tally-vertical-slice.md",
		map[string]string{"flags": text})
}
