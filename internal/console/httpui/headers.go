package httpui

import "net/http"

// contentSecurityPolicy admits the one stylesheet of the console and nothing
// else: the pages carry no script, no inline style and no external reference.
// The theme form posts to the console itself, which form-action admits.
const contentSecurityPolicy = "default-src 'none'; style-src 'self'; form-action 'self'; " +
	"frame-ancestors 'none'; base-uri 'none'"

// secureHeaders sets the headers every response carries. A response holds what
// an operator may read and nobody else, so nothing stores it, and no page may
// be framed. The headers are set before next runs, so they are there whatever
// next answers.
func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("Cache-Control", "no-store")
		header.Set("Content-Security-Policy", contentSecurityPolicy)
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("X-Frame-Options", "DENY")
		header.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}
