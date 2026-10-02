package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/routers"

	"github.com/b42labs/tally/internal/reporting/httpapi/problem"
)

// fixtureSpec is a contract with a typed query parameter and a request body.
// The shipped contract has neither yet, so the validation mapping is exercised
// against this one.
const fixtureSpec = `
openapi: 3.0.3
info:
  title: Validation fixture
  version: 0.0.1
paths:
  /widgets:
    get:
      operationId: listWidgets
      parameters:
        - name: limit
          in: query
          required: true
          schema:
            type: integer
      responses:
        "200":
          description: The widgets.
    post:
      operationId: createWidget
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              required: [size]
              properties:
                size:
                  type: integer
      responses:
        "201":
          description: The widget.
`

func TestRouterRejectsRequestsOutsideTheContract(t *testing.T) {
	handler := newTestRouter(t)

	t.Run("answers an unknown path with a not_found problem", func(t *testing.T) {
		rec := serve(handler, httptest.NewRequest(http.MethodGet, "/nope", nil))

		assertProblem(t, rec, http.StatusNotFound, problem.TypeNotFound)
	})

	t.Run("answers a known path addressed with the wrong method", func(t *testing.T) {
		rec := serve(handler, httptest.NewRequest(http.MethodPost, "/healthz", nil))

		assertProblem(t, rec, http.StatusMethodNotAllowed, problem.TypeMethodNotAllowed)
	})

	t.Run("lets a request the contract describes through", func(t *testing.T) {
		rec := serve(handler, httptest.NewRequest(http.MethodGet, "/readyz", nil))

		if got := rec.Code; got != http.StatusOK {
			t.Errorf("status = %d, want %d (body %q)", got, http.StatusOK, rec.Body)
		}
		if got := rec.Body.String(); got != "ok" {
			t.Errorf("body = %q, want %q", got, "ok")
		}
	})
}

func TestValidatorRejectsABrokenParameter(t *testing.T) {
	spec, err := loadSpec(func() (*openapi3.T, error) {
		return openapi3.NewLoader().LoadFromData([]byte(fixtureSpec))
	})
	if err != nil {
		t.Fatalf("loading the fixture spec: %v", err)
	}

	handler := newValidator(spec)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	t.Run("answers a parameter of the wrong type with a validation problem", func(t *testing.T) {
		rec := serve(handler, httptest.NewRequest(http.MethodGet, "/widgets?limit=not-a-number", nil))

		assertProblem(t, rec, http.StatusBadRequest, problem.TypeValidation)
	})

	t.Run("answers a missing required parameter with a validation problem", func(t *testing.T) {
		rec := serve(handler, httptest.NewRequest(http.MethodGet, "/widgets", nil))

		assertProblem(t, rec, http.StatusBadRequest, problem.TypeValidation)
	})

	t.Run("lets a well-formed parameter through", func(t *testing.T) {
		rec := serve(handler, httptest.NewRequest(http.MethodGet, "/widgets?limit=10", nil))

		if got := rec.Code; got != http.StatusOK {
			t.Errorf("status = %d, want %d (body %q)", got, http.StatusOK, rec.Body)
		}
	})
}

// securedSpec declares one operation behind a scheme the dispatch enforces and
// one behind a scheme this API has no middleware for. The shipped contract
// carries only the first kind, so the second is exercised against this fixture.
const securedSpec = `
openapi: 3.0.3
info:
  title: Security fixture
  version: 0.0.1
paths:
  /widgets:
    get:
      operationId: listWidgets
      security:
        - apiToken: []
      responses:
        "200":
          description: The widgets.
  /gadgets:
    get:
      operationId: listGadgets
      security:
        - apiKeyAuth: []
      responses:
        "200":
          description: The gadgets.
components:
  securitySchemes:
    apiToken:
      type: http
      scheme: bearer
    apiKeyAuth:
      type: apiKey
      in: header
      name: X-API-Key
`

// TestValidatorLeavesTheBearerSchemesToTheDispatch pins both halves of the
// authentication seam.
//
// The contract's bearer schemes pass this layer whatever the request carries,
// because the credential behind them is checked by the dispatch middleware that
// runs once chi has matched a route, and only there is the guard a route needs
// known. A scheme no middleware covers is refused here instead, which is what
// keeps someone from putting a route online by declaring a security scheme the
// dispatch knows nothing about.
func TestValidatorLeavesTheBearerSchemesToTheDispatch(t *testing.T) {
	spec, err := loadSpec(func() (*openapi3.T, error) {
		return openapi3.NewLoader().LoadFromData([]byte(securedSpec))
	})
	if err != nil {
		t.Fatalf("loading the fixture spec: %v", err)
	}

	served := false
	handler := newValidator(spec)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served = true
		w.WriteHeader(http.StatusOK)
	}))

	for name, req := range map[string]*http.Request{
		"without a credential": httptest.NewRequest(http.MethodGet, "/widgets", nil),
		"with one":             requestWithBearer(t, "/widgets", "any-token"),
	} {
		t.Run("passes the bearer scheme on "+name, func(t *testing.T) {
			served = false

			rec := serve(handler, req)

			if got := rec.Code; got != http.StatusOK {
				t.Errorf("status = %d, want %d (body %q)", got, http.StatusOK, rec.Body)
			}
			if !served {
				t.Error("the handler did not run, want the request left to the next layer")
			}
		})
	}

	t.Run("refuses a scheme nothing enforces", func(t *testing.T) {
		served = false

		rec := serve(handler, httptest.NewRequest(http.MethodGet, "/gadgets", nil))

		assertProblem(t, rec, http.StatusUnauthorized, problem.TypeUnauthorized)
		if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
			t.Errorf("WWW-Authenticate = %q, want %q", got, "Bearer")
		}
		if served {
			t.Error("the handler ran, want the operation refused")
		}
	})
}

// TestRouterCapsTheRequestBody pins the ceiling an anonymous caller is bounded
// by. The contract validator reads and decodes a body before the dispatch
// middleware checks any credential, so a body past the cap has to be refused
// here: the batch limit in the ingest handler sits behind both and would only
// fire once this process had already paid for the whole document.
func TestRouterCapsTheRequestBody(t *testing.T) {
	handler := newTestRouter(t)

	oversized := bytes.Repeat([]byte("a"), maxRequestBody+1)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/events", bytes.NewReader(oversized))
	r.Header.Set("Content-Type", "application/json")

	rec := serve(handler, r)

	assertProblem(t, rec, http.StatusRequestEntityTooLarge, problem.TypePayloadTooLarge)
}

// TestValidationFailuresNameTheOffendingValue proves the two halves of the
// error contract: the client gets the location as a field error, the way the
// contract's errors array promises, and never the validator's own message,
// which carries the library's types and the value that was sent.
func TestValidationFailuresNameTheOffendingValue(t *testing.T) {
	spec, err := loadSpec(func() (*openapi3.T, error) {
		return openapi3.NewLoader().LoadFromData([]byte(fixtureSpec))
	})
	if err != nil {
		t.Fatalf("loading the fixture spec: %v", err)
	}
	handler := newValidator(spec)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	tests := map[string]struct {
		request *http.Request
		wantLoc string
	}{
		"a parameter of the wrong type": {
			request: httptest.NewRequest(http.MethodGet, "/widgets?limit=not-a-number", nil),
			wantLoc: "query.limit",
		},
		"a body member of the wrong type": {
			request: jsonRequest(t, "/widgets", `{"size": "large"}`),
			wantLoc: "body.size",
		},
		"a body missing a required member": {
			request: jsonRequest(t, "/widgets", `{}`),
			wantLoc: "body.size",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			rec := serve(handler, tc.request)

			assertProblem(t, rec, http.StatusBadRequest, problem.TypeValidation)

			var body struct {
				Detail string               `json:"detail"`
				Errors []problem.FieldError `json:"errors"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decoding the body %q: %v", rec.Body.String(), err)
			}
			if strings.Contains(body.Detail, "openapi3filter") {
				t.Errorf("detail = %q, want it free of the validator's internals", body.Detail)
			}
			if len(body.Errors) != 1 {
				t.Fatalf("errors = %v, want one entry naming the offending value", body.Errors)
			}
			if body.Errors[0].Loc != tc.wantLoc {
				t.Errorf("errors[0].loc = %q, want %q", body.Errors[0].Loc, tc.wantLoc)
			}
			if body.Errors[0].Msg == "" {
				t.Error("errors[0].msg is empty, want what is wrong with the value")
			}
		})
	}
}

// TestRouterAnswersARepeatedParameterWithAProblem pins the shape of a refusal
// the validator does not make. It reads one occurrence of a repeated query
// parameter and lets the request through, so the generated wrapper is what
// refuses to bind it, and its answer has to be the problem every other error
// is. The requests carry no credential: binding runs before the dispatch
// middleware checks one.
func TestRouterAnswersARepeatedParameterWithAProblem(t *testing.T) {
	handler := newTestRouter(t)

	tests := map[string]struct {
		target  string
		wantLoc string
	}{
		"a repeated group_by on the event statistics": {
			target:  "/api/v1/stats/events?group_by=event_type&group_by=source&from=2026-01-01T00:00:00Z&to=2026-01-02T00:00:00Z&interval=1h",
			wantLoc: "query.group_by",
		},
		"a repeated group_by on the resource statistics": {
			target:  "/api/v1/stats/resources?group_by=cloud&group_by=resource_type",
			wantLoc: "query.group_by",
		},
		"a repeated interval": {
			target:  "/api/v1/stats/events?group_by=cloud,event_type&from=2026-01-01T00:00:00Z&to=2026-01-02T00:00:00Z&interval=1h&interval=1d",
			wantLoc: "query.interval",
		},
		"a repeated status": {
			target:  "/api/v1/stats/resources?group_by=cloud,resource_type&status=active&status=all",
			wantLoc: "query.status",
		},
		"a repeated limit": {
			target:  "/api/v1/events?limit=1&limit=2",
			wantLoc: "query.limit",
		},
		"a repeated cloud": {
			target:  "/api/v1/events?cloud=a&cloud=b",
			wantLoc: "query.cloud",
		},
		"a repeated cursor": {
			target:  "/api/v1/events?cursor=a&cursor=b",
			wantLoc: "query.cursor",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			rec := serve(handler, httptest.NewRequest(http.MethodGet, tc.target, nil))

			assertProblem(t, rec, http.StatusBadRequest, problem.TypeValidation)

			// The whole body is pinned, which is also what keeps the binding
			// library's own text out of it.
			want := refusedParameterBody(tc.wantLoc, "is given more than once")
			if got := strings.TrimSpace(rec.Body.String()); got != want {
				t.Errorf("body = %s, want %s", got, want)
			}
			if got := rec.Header().Get(requestIDHeader); got == "" {
				t.Errorf("%s response header is empty, want the id of the request", requestIDHeader)
			}
		})
	}
}

// TestRouterNamesTheParameterOfAValueEitherLayerRefuses sends values given once
// that the validator and the generated wrapper parse by different rules. The
// validator's integer and date-time checks are looser than the binding in some
// places and as strict in others, and a client is not to see which of the two
// refused: either way the answer names the parameter.
func TestRouterNamesTheParameterOfAValueEitherLayerRefuses(t *testing.T) {
	handler := newTestRouter(t)

	const window = "&to=2026-03-02T00:00:00Z&interval=1h"
	tests := map[string]struct {
		target  string
		wantLoc string
	}{
		"an integer in another base": {
			target:  "/api/v1/events?limit=0x10",
			wantLoc: "query.limit",
		},
		"an integer with an exponent": {
			target:  "/api/v1/events?limit=1e1",
			wantLoc: "query.limit",
		},
		"a day the month does not have": {
			target:  "/api/v1/stats/events?group_by=cloud,event_type&from=2026-02-30T00:00:00Z" + window,
			wantLoc: "query.from",
		},
		"a second the minute does not have": {
			target:  "/api/v1/events?to=2026-03-01T00:00:60Z",
			wantLoc: "query.to",
		},
		"an instant no calendar has on the resource statistics": {
			target:  "/api/v1/stats/resources?group_by=cloud,resource_type&at=2026-02-31T00:00:00Z",
			wantLoc: "query.at",
		},
		"an instant without a zone": {
			target:  "/api/v1/stats/events?group_by=cloud,event_type&from=2026-03-01T00:00:00" + window,
			wantLoc: "query.from",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			rec := serve(handler, httptest.NewRequest(http.MethodGet, tc.target, nil))

			assertProblem(t, rec, http.StatusBadRequest, problem.TypeValidation)
			want := refusedParameterBody(tc.wantLoc, "does not match the contract")
			if got := strings.TrimSpace(rec.Body.String()); got != want {
				t.Errorf("body = %s, want %s", got, want)
			}
		})
	}
}

// FuzzRouterAnswersARefusedQueryWithAProblem drives the query string, which is
// the input the validator and the generated wrapper each parse by rules of
// their own. The requests carry no credential, so a 400 comes from one of those
// two layers.
//
// The invariant is that a client cannot tell them apart by the shape of the
// answer: whichever layer refuses a query, the 400 is the validation problem
// and never the binding library's plain text.
func FuzzRouterAnswersARefusedQueryWithAProblem(f *testing.F) {
	routes := []string{"/api/v1/events", "/api/v1/stats/events", "/api/v1/stats/resources"}
	for _, seed := range []string{
		"group_by=cloud&group_by=resource_type",
		"group_by=cloud,event_type&from=2026-01-01T00:00:00Z&to=2026-01-02T00:00:00Z&interval=1h&interval=1d",
		"limit=1&limit=2",
		"limit=0x10",
		"limit=",
		"from=2026-02-31T00:00:00Z",
		"cursor=a&cursor=b",
	} {
		for route := range routes {
			f.Add(uint8(route), seed)
		}
	}

	handler := newTestRouter(f)
	f.Fuzz(func(t *testing.T, route uint8, rawQuery string) {
		req := httptest.NewRequest(http.MethodGet, routes[int(route)%len(routes)], nil)
		req.URL.RawQuery = rawQuery

		rec := serve(handler, req)

		if rec.Code == http.StatusBadRequest {
			assertProblem(t, rec, http.StatusBadRequest, problem.TypeValidation)
		}
	})
}

// TestWriteBindingProblem hands the hook its failures directly, which is the
// only way to reach the ones no request through the router is known to cause:
// those it has no query parameter to name. They leave as the detail alone, and
// the log line is where their cause is kept.
func TestWriteBindingProblem(t *testing.T) {
	const logMessage = "binding the request parameters"

	repeated := &InvalidParamFormatError{ParamName: "group_by", Err: errors.New("secret-value")}

	tests := map[string]struct {
		target string
		err    error
		// want is the one field error the answer carries. Leaving it nil asks
		// for the detail alone.
		want *problem.FieldError
		// wantLogs is how many lines the hook itself logs.
		wantLogs int
		// absent is a text of the error the body must not carry.
		absent string
	}{
		"names a parameter the request repeats": {
			target: "/x?group_by=a&group_by=b",
			err:    repeated,
			want:   &problem.FieldError{Loc: "query.group_by", Msg: "is given more than once"},
			absent: "secret-value",
		},
		"names a parameter the request gives once": {
			target: "/x?group_by=a",
			err:    repeated,
			want:   &problem.FieldError{Loc: "query.group_by", Msg: "does not match the contract"},
			absent: "secret-value",
		},
		"a parameter absent from the query, as a path parameter is, is not attributed": {
			target:   "/x",
			err:      repeated,
			wantLogs: 1,
			absent:   "secret-value",
		},
		"an empty parameter name is not attributed": {
			target:   "/x",
			err:      &InvalidParamFormatError{ParamName: "", Err: errors.New("x")},
			wantLogs: 1,
		},
		"a missing required parameter is not attributed, even when the request repeats it": {
			target:   "/x?limit=1&limit=2",
			err:      &RequiredParamError{ParamName: "limit"},
			wantLogs: 1,
		},
		"an error of another type is not attributed": {
			target:   "/x",
			err:      errors.New("boom"),
			wantLogs: 1,
			absent:   "boom",
		},
		"a nil error is answered without a panic": {
			target:   "/x",
			err:      nil,
			wantLogs: 1,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			logs, handler := chain(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeBindingProblem(w, r, tc.err)
			}))

			rec := serve(handler, request(t, http.MethodGet, tc.target, nil))

			// A panic would leave as the 500 of the recoverer in the chain.
			assertProblem(t, rec, http.StatusBadRequest, problem.TypeValidation)

			var body struct {
				Detail string                `json:"detail"`
				Errors *[]problem.FieldError `json:"errors"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decoding the body %q: %v", rec.Body.String(), err)
			}
			if want := "the request does not match the API contract"; body.Detail != want {
				t.Errorf("detail = %q, want %q", body.Detail, want)
			}
			if tc.absent != "" && strings.Contains(rec.Body.String(), tc.absent) {
				t.Errorf("body = %q, want it free of the error's text %q", rec.Body.String(), tc.absent)
			}

			switch {
			case tc.want == nil && body.Errors != nil:
				t.Errorf("errors = %v, want no errors member", *body.Errors)
			case tc.want != nil:
				want := []problem.FieldError{*tc.want}
				if body.Errors == nil || !slices.Equal(*body.Errors, want) {
					t.Errorf("body = %s, want the errors %v", rec.Body.String(), want)
				}
			}

			// The request logging middleware writes a line of its own for every
			// request, so the hook's lines are picked by their message.
			var logged []map[string]any
			for _, line := range logLines(t, logs) {
				if line["msg"] == logMessage {
					logged = append(logged, line)
				}
			}
			if len(logged) != tc.wantLogs {
				t.Fatalf("logged %d lines %q, want %d:\n%s", len(logged), logMessage, tc.wantLogs, logs)
			}
			for _, line := range logged {
				if got := line["level"]; got != "WARN" {
					t.Errorf("log level = %v, want WARN", got)
				}
				// The response carries none of the cause, so the line has to
				// carry all of it. A nil error is logged as a JSON null.
				var want any
				if tc.err != nil {
					want = tc.err.Error()
				}
				if got, ok := line["error"]; !ok || got != want {
					t.Errorf("log line %v, want its error attribute to be %v", line, want)
				}
			}
		})
	}
}

func TestValidationProblem(t *testing.T) {
	tests := map[string]struct {
		err       error
		suggested int
		wantCode  int
		wantType  string
	}{
		"a method mismatch outweighs the suggested 404": {
			err:       routers.ErrMethodNotAllowed,
			suggested: http.StatusNotFound,
			wantCode:  http.StatusMethodNotAllowed,
			wantType:  problem.TypeMethodNotAllowed,
		},
		"an unmatched path is not found": {
			err:       routers.ErrPathNotFound,
			suggested: http.StatusNotFound,
			wantCode:  http.StatusNotFound,
			wantType:  problem.TypeNotFound,
		},
		"a bad request is a validation failure": {
			err:       io.ErrUnexpectedEOF,
			suggested: http.StatusBadRequest,
			wantCode:  http.StatusBadRequest,
			wantType:  problem.TypeValidation,
		},
		"an unprocessable entity is a validation failure": {
			err:       io.ErrUnexpectedEOF,
			suggested: http.StatusUnprocessableEntity,
			wantCode:  http.StatusBadRequest,
			wantType:  problem.TypeValidation,
		},
		"a failure the validator cannot classify stays internal": {
			err:       io.ErrUnexpectedEOF,
			suggested: http.StatusInternalServerError,
			wantCode:  http.StatusInternalServerError,
			wantType:  problem.TypeInternal,
		},
		"a rejected credential is unauthorized rather than malformed": {
			err:       io.ErrUnexpectedEOF,
			suggested: http.StatusUnauthorized,
			wantCode:  http.StatusUnauthorized,
			wantType:  problem.TypeUnauthorized,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			gotCode, gotType, gotTitle := validationProblem(tc.err, tc.suggested)

			if gotCode != tc.wantCode {
				t.Errorf("status = %d, want %d", gotCode, tc.wantCode)
			}
			if gotType != tc.wantType {
				t.Errorf("type = %q, want %q", gotType, tc.wantType)
			}
			if gotTitle == "" {
				t.Error("title is empty, want a summary of the problem")
			}
		})
	}
}

func TestNewRouterRefusesABrokenContract(t *testing.T) {
	// loadSpec is what NewRouter calls, so proving the wrap here proves that a
	// broken contract stops the server rather than starting one that validates
	// nothing.
	if _, err := loadSpec(func() (*openapi3.T, error) { return &openapi3.T{}, nil }); err == nil {
		t.Fatal("loadSpec() error = nil for a broken contract, want an error")
	}

	if _, err := NewRouter(Options{DB: healthyPinger(), UnhealthyThreshold: time.Minute}); err != nil {
		t.Errorf("NewRouter() error = %v for the shipped contract, want nil", err)
	}
}

// jsonRequest is a POST carrying a JSON body.
func jsonRequest(t *testing.T, path, body string) *http.Request {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// requestWithBearer is a GET carrying a bearer credential.
func requestWithBearer(t *testing.T, path, token string) *http.Request {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

// newTestRouter builds the router over a database that always answers.
func newTestRouter(t testing.TB) http.Handler {
	t.Helper()

	handler, err := NewRouter(Options{
		Logger:             slog.New(slog.NewJSONHandler(io.Discard, nil)),
		DB:                 healthyPinger(),
		UnhealthyThreshold: time.Minute,
	})
	if err != nil {
		t.Fatalf("NewRouter() error = %v, want nil", err)
	}
	return handler
}

// healthyPinger is a database that is always reachable and carries the schema
// this build expects.
func healthyPinger() DB {
	return pingerFunc(func(context.Context) error { return nil })
}

// refusedParameterBody is the whole body of the validation problem that names
// one query parameter, which is what a refused parameter is answered with.
func refusedParameterBody(loc, msg string) string {
	return `{"type":"urn:tally:error:validation","title":"Validation failed","status":400,` +
		`"detail":"the request does not match the API contract",` +
		`"errors":[{"loc":"` + loc + `","msg":"` + msg + `"}]}`
}

// assertProblem checks the status and the problem type of an error response.
func assertProblem(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantType string) {
	t.Helper()

	if got := rec.Code; got != wantStatus {
		t.Errorf("status = %d, want %d (body %q)", got, wantStatus, rec.Body)
	}
	if got := rec.Header().Get("Content-Type"); got != problem.ContentType {
		t.Errorf("Content-Type = %q, want %q", got, problem.ContentType)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the body %q: %v", rec.Body.String(), err)
	}
	if got := body["type"]; got != wantType {
		t.Errorf("body type = %v, want %v", got, wantType)
	}
	if got, want := body["status"], float64(wantStatus); got != want {
		t.Errorf("body status = %v, want %v", got, want)
	}
}
