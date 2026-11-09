package httpui

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/b42labs/tally/internal/console/signin"
)

// accessLog writes one line per request the sign-in let through, naming the
// operator. The query is on the line because every identifier travels there,
// so the path alone does not say which statement was read.
func accessLog(logger *slog.Logger) func(http.Handler) http.Handler {
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
