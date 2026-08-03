package httpapi

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/b42labs/tally/internal/reporting/httpapi/problem"
)

// requestIDHeader carries the correlation id in and out. A caller that already
// has one, such as the Gateway, passes it so that its logs and ours line up.
const requestIDHeader = "X-Request-ID"

// requestIDPattern is what an incoming id has to look like to be adopted. It
// keeps a hostile caller from smuggling newlines or unbounded strings into
// every log line the request produces.
var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// contextKey is the private type of this package's context keys, so no other
// package can collide with them.
type contextKey int

const (
	contextKeyRequestID contextKey = iota
	contextKeyLogger
)

// Logger returns the logger of the request ctx belongs to, which carries the
// request id. It falls back to the default logger outside a request, so a
// caller never has to nil-check it.
func Logger(ctx context.Context) *slog.Logger {
	if logger, ok := ctx.Value(contextKeyLogger).(*slog.Logger); ok {
		return logger
	}
	return slog.Default()
}

// RequestID returns the correlation id of the request ctx belongs to, or the
// empty string outside a request. It is the same id the response echoes in its
// X-Request-ID header.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(contextKeyRequestID).(string)
	return id
}

// newRequestID builds the middleware that gives every request a correlation id
// and a logger tagged with it. The id is echoed to the client so that a caller
// reporting a problem can name the exact request in our logs.
func newRequestID(base *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(requestIDHeader)
			if !requestIDPattern.MatchString(id) {
				id = uuid.NewString()
			}
			w.Header().Set(requestIDHeader, id)

			ctx := context.WithValue(r.Context(), contextKeyRequestID, id)
			ctx = context.WithValue(ctx, contextKeyLogger, base.With("request_id", id))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// requestLogging logs one line per request once it is served. It reports the
// method, the path, the status, and how long the request took, and it never
// touches headers: the Authorization header carries a service token, and a log
// pipeline is not where tokens belong.
func requestLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

		next.ServeHTTP(recorder, r)

		Logger(r.Context()).Info("served request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", recorder.Status(),
			"duration", time.Since(start),
		)
	})
}

// recoverer turns a panicking handler into a 500 problem response so that one
// bad request does not take the process down with it.
func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			panicked := recover()
			if panicked == nil {
				return
			}

			Logger(r.Context()).Error("handler panicked",
				"panic", fmt.Sprint(panicked),
				"stack", string(debug.Stack()),
			)

			// A handler that panicked halfway through its response has already
			// spent the status line, so there is nothing left to render. The
			// request logging middleware sits directly outside this one, which is
			// what makes the recorder available here.
			if recorder, ok := w.(middleware.WrapResponseWriter); ok && recorder.Status() != 0 {
				return
			}
			problem.Write(w, http.StatusInternalServerError, problem.TypeInternal,
				"Internal error", "the request could not be served")
		}()

		next.ServeHTTP(w, r)
	})
}
