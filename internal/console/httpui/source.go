package httpui

import "github.com/b42labs/tally/internal/console/reporting"

// The two kinds of read a page makes. They are the labels the provenance panel
// prints, so a reader sees which numbers came over HTTP and which came out of
// the engine database.
const (
	kindAPI   = "Reporting API"
	kindStore = "engine store"
)

// Source is one read a page made: which side answered it, and what was asked.
type Source struct {
	Kind string
	Text string
}

// sources collects the reads of one page in the order the handler made them.
// The panel is filled as the handler goes, so a page that failed halfway still
// lists what it got to.
type sources []Source

// api records one call to the Reporting API, method and path as the client
// made it, query string included.
func (s *sources) api(req reporting.Request) {
	*s = append(*s, Source{Kind: kindAPI, Text: req.Method + " " + req.Path})
}

// query records one read of the engine database under the name the query
// carries in queries.sql, which is where a reader looks the SQL up.
func (s *sources) query(name string) {
	*s = append(*s, Source{Kind: kindStore, Text: name})
}
