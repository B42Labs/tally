package problem_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/b42labs/tally/internal/reporting/httpapi/problem"
)

func TestWrite(t *testing.T) {
	t.Run("serves the problem media type and the requested status", func(t *testing.T) {
		rec := httptest.NewRecorder()

		problem.Write(rec, http.StatusNotFound, problem.TypeNotFound, "Not found", "")

		if got := rec.Code; got != http.StatusNotFound {
			t.Errorf("status = %d, want %d", got, http.StatusNotFound)
		}
		if got := rec.Header().Get("Content-Type"); got != problem.ContentType {
			t.Errorf("Content-Type = %q, want %q", got, problem.ContentType)
		}
	})

	t.Run("repeats the status inside the body", func(t *testing.T) {
		rec := httptest.NewRecorder()

		problem.Write(rec, http.StatusServiceUnavailable, problem.TypeUnavailable,
			"Service unavailable", "the database is unreachable")

		body := decode(t, rec)
		if got, want := body["status"], float64(http.StatusServiceUnavailable); got != want {
			t.Errorf("body status = %v, want %v", got, want)
		}
		if got, want := body["type"], problem.TypeUnavailable; got != want {
			t.Errorf("body type = %v, want %v", got, want)
		}
		if got, want := body["title"], "Service unavailable"; got != want {
			t.Errorf("body title = %v, want %v", got, want)
		}
		if got, want := body["detail"], "the database is unreachable"; got != want {
			t.Errorf("body detail = %v, want %v", got, want)
		}
	})

	t.Run("omits detail and errors when they are empty", func(t *testing.T) {
		rec := httptest.NewRecorder()

		problem.Write(rec, http.StatusInternalServerError, problem.TypeInternal, "Internal error", "")

		body := decode(t, rec)
		for _, field := range []string{"detail", "errors"} {
			if _, ok := body[field]; ok {
				t.Errorf("body has %q = %v, want it omitted", field, body[field])
			}
		}
	})

	t.Run("carries every field error it is given", func(t *testing.T) {
		rec := httptest.NewRecorder()

		problem.Write(rec, http.StatusBadRequest, problem.TypeValidation, "Validation failed", "two fields are wrong",
			problem.FieldError{Loc: "body.resource_id", Msg: "must not be empty"},
			problem.FieldError{Loc: "query.limit", Msg: "must be at most 1000"},
		)

		var got struct {
			Errors []problem.FieldError `json:"errors"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decoding the body %q: %v", rec.Body.String(), err)
		}

		want := []problem.FieldError{
			{Loc: "body.resource_id", Msg: "must not be empty"},
			{Loc: "query.limit", Msg: "must be at most 1000"},
		}
		if len(got.Errors) != len(want) {
			t.Fatalf("errors = %v, want %v", got.Errors, want)
		}
		for i := range want {
			if got.Errors[i] != want[i] {
				t.Errorf("errors[%d] = %v, want %v", i, got.Errors[i], want[i])
			}
		}
	})
}

// decode reads the recorded problem back as a generic object, which is how the
// tests assert on the wire shape rather than on a Go struct.
func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the body %q: %v", rec.Body.String(), err)
	}
	return body
}
