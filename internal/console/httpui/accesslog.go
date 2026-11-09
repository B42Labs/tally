package httpui

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/b42labs/tally/internal/console/metrics"
	"github.com/b42labs/tally/internal/console/signin"
)

// accessLog counts every request the sign-in let through and writes one line
// for it, naming the operator. The query is on the line because every
// identifier travels there, so the path alone does not say which statement was
// read. The counter takes the route pattern instead, which carries no
// identifier and is known only once next has returned.
func accessLog(logger *slog.Logger, m *metrics.Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			wrapped := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

			next.ServeHTTP(wrapped, r)

			// A handler that writes nothing leaves the status at zero, and the
			// server answers such a request 200.
			status := wrapped.Status()
			if status == 0 {
				status = http.StatusOK
			}
			m.Request(chi.RouteContext(r.Context()).RoutePattern(), status)
			logger.Info("request",
				"operator", signin.OperatorFrom(r.Context()),
				"method", r.Method,
				"path", r.URL.Path,
				"query", r.URL.RawQuery,
				"status", status,
				"duration_ms", time.Since(start).Milliseconds(),
			)
		})
	}
}
