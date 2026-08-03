package httpapi

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestRequestID(t *testing.T) {
	t.Run("adopts a well-formed incoming id and tags every log line with it", func(t *testing.T) {
		const incoming = "gateway-request.42_ab-CD"

		logs, handler := chain(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			Logger(r.Context()).Debug("handler ran")
			w.WriteHeader(http.StatusNoContent)
		}))

		rec := serve(handler, request(t, http.MethodGet, "/healthz", map[string]string{requestIDHeader: incoming}))

		if got := rec.Header().Get(requestIDHeader); got != incoming {
			t.Errorf("%s response header = %q, want %q", requestIDHeader, got, incoming)
		}

		lines := logLines(t, logs)
		if len(lines) < 2 {
			t.Fatalf("captured %d log lines, want the handler line and the request line", len(lines))
		}
		for i, line := range lines {
			if got := line["request_id"]; got != incoming {
				t.Errorf("log line %d has request_id = %v, want %q", i, got, incoming)
			}
		}
	})

	t.Run("replaces an id that breaks the format", func(t *testing.T) {
		for name, incoming := range map[string]string{
			"too long":      strings.Repeat("a", 65),
			"bad charset":   "has spaces and $igns",
			"empty":         "",
			"with newlines": "forged\nlog line",
		} {
			t.Run(name, func(t *testing.T) {
				_, handler := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusNoContent)
				}))

				rec := serve(handler, request(t, http.MethodGet, "/healthz", map[string]string{requestIDHeader: incoming}))

				got := rec.Header().Get(requestIDHeader)
				if got == incoming {
					t.Fatalf("%s response header = %q, want a generated id", requestIDHeader, got)
				}
				if _, err := uuid.Parse(got); err != nil {
					t.Errorf("parsing the generated id %q: %v", got, err)
				}
			})
		}
	})

	t.Run("generates an id when the header is absent", func(t *testing.T) {
		_, handler := chain(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := RequestID(r.Context()); got != w.Header().Get(requestIDHeader) {
				t.Errorf("RequestID() = %q, want the id in the %s header %q",
					got, requestIDHeader, w.Header().Get(requestIDHeader))
			}
			w.WriteHeader(http.StatusNoContent)
		}))

		rec := serve(handler, request(t, http.MethodGet, "/healthz", nil))

		if _, err := uuid.Parse(rec.Header().Get(requestIDHeader)); err != nil {
			t.Errorf("parsing the generated id %q: %v", rec.Header().Get(requestIDHeader), err)
		}
	})
}

func TestRequestLogging(t *testing.T) {
	t.Run("reports the method, path, and status of the served request", func(t *testing.T) {
		logs, handler := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTeapot)
		}))

		serve(handler, request(t, http.MethodGet, "/readyz", nil))

		line := lastLogLine(t, logs)
		for field, want := range map[string]any{
			"method": http.MethodGet,
			"path":   "/readyz",
			"status": float64(http.StatusTeapot),
			"msg":    "served request",
		} {
			if got := line[field]; got != want {
				t.Errorf("request log %s = %v, want %v", field, got, want)
			}
		}
		if _, ok := line["duration"]; !ok {
			t.Error("request log has no duration")
		}
	})

	t.Run("records the implicit 200 of a handler that only writes a body", func(t *testing.T) {
		logs, handler := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if _, err := w.Write([]byte("ok")); err != nil {
				t.Errorf("writing the body: %v", err)
			}
		}))

		serve(handler, request(t, http.MethodGet, "/healthz", nil))

		if got, want := lastLogLine(t, logs)["status"], float64(http.StatusOK); got != want {
			t.Errorf("request log status = %v, want %v", got, want)
		}
	})

	t.Run("never logs the Authorization header", func(t *testing.T) {
		const token = "supersecret"

		logs, handler := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

		serve(handler, request(t, http.MethodGet, "/healthz", map[string]string{
			"Authorization": "Bearer " + token,
		}))

		if strings.Contains(logs.String(), token) {
			t.Errorf("the captured log contains the bearer token:\n%s", logs)
		}
	})
}

func TestRecoverer(t *testing.T) {
	t.Run("renders a panic as an internal problem and keeps serving", func(t *testing.T) {
		logs, handler := chain(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/boom" {
				panic("handler exploded")
			}
			w.WriteHeader(http.StatusNoContent)
		}))

		rec := serve(handler, request(t, http.MethodGet, "/boom", nil))

		if got := rec.Code; got != http.StatusInternalServerError {
			t.Errorf("status = %d, want %d", got, http.StatusInternalServerError)
		}
		if got, want := rec.Header().Get("Content-Type"), "application/problem+json"; got != want {
			t.Errorf("Content-Type = %q, want %q", got, want)
		}

		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decoding the body %q: %v", rec.Body.String(), err)
		}
		if got, want := body["type"], "urn:tally:error:internal"; got != want {
			t.Errorf("body type = %v, want %v", got, want)
		}

		if !strings.Contains(logs.String(), "handler exploded") {
			t.Errorf("the captured log does not mention the panic:\n%s", logs)
		}
		if !strings.Contains(logs.String(), "stack") {
			t.Errorf("the captured log carries no stack:\n%s", logs)
		}

		// The whole point of recovering is that the next request still works.
		if got := serve(handler, request(t, http.MethodGet, "/healthz", nil)).Code; got != http.StatusNoContent {
			t.Errorf("status of the request after the panic = %d, want %d", got, http.StatusNoContent)
		}
	})

	t.Run("leaves a response that a panicking handler already started alone", func(t *testing.T) {
		_, handler := chain(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			if _, err := w.Write([]byte("partial")); err != nil {
				t.Errorf("writing the body: %v", err)
			}
			panic("too late")
		}))

		rec := serve(handler, request(t, http.MethodGet, "/healthz", nil))

		if got := rec.Code; got != http.StatusOK {
			t.Errorf("status = %d, want the %d the handler already sent", got, http.StatusOK)
		}
		if got := rec.Body.String(); got != "partial" {
			t.Errorf("body = %q, want the partial body the handler wrote", got)
		}
	})
}

// chain wraps next in the middleware stack the router mounts, and returns the
// buffer every log line of those requests lands in.
func chain(t *testing.T, next http.Handler) (*bytes.Buffer, http.Handler) {
	t.Helper()

	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	return logs, newRequestID(logger)(requestLogging(recoverer(next)))
}

// request builds a request with the given headers set.
func request(t *testing.T, method, target string, headers map[string]string) *http.Request {
	t.Helper()

	r := httptest.NewRequest(method, target, nil)
	for name, value := range headers {
		r.Header.Set(name, value)
	}
	return r
}

// serve runs one request through handler and returns what it wrote.
func serve(handler http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, r)
	return rec
}

// logLines decodes every captured log line.
func logLines(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()

	var lines []map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if raw == "" {
			continue
		}
		var line map[string]any
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatalf("decoding the log line %q: %v", raw, err)
		}
		lines = append(lines, line)
	}
	return lines
}

// lastLogLine returns the line the request logging middleware wrote, which is
// the final one of a served request.
func lastLogLine(t *testing.T, logs *bytes.Buffer) map[string]any {
	t.Helper()

	lines := logLines(t, logs)
	if len(lines) == 0 {
		t.Fatal("no log line was captured")
	}
	return lines[len(lines)-1]
}
