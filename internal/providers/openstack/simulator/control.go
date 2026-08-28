package simulator

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// Progress is how far a run has got: the simulated period it publishes and the
// notifications it has already put on the bus. The run hands it to the control
// endpoint through a callback rather than a copy, so a caller reading /clock
// sees the counts of that moment instead of the counts the endpoint was built
// with.
type Progress struct {
	// From and To bound the simulated month, the period the virtual clock runs
	// through.
	From time.Time
	To   time.Time
	// Published is how many notifications the run has published so far, of Total
	// in the whole month.
	Published int
	Total     int
}

// clockDocument is what /clock answers. The instants are RFC 3339 in UTC
// because the reader is a person or a script watching a run, and a zoneless
// timestamp would leave both guessing which zone the simulator ran in.
type clockDocument struct {
	VirtualNow string  `json:"virtual_now"`
	Factor     float64 `json:"factor"`
	Published  int     `json:"published"`
	Total      int     `json:"total"`
	PeriodFrom string  `json:"period_from"`
	PeriodTo   string  `json:"period_to"`
}

// badFactorBody is the single answer every refused PUT /clock gets. One answer
// covers a body that is not JSON, one without the member, and one whose factor
// is negative, because what the caller has to do about it is the same in all
// three.
const badFactorBody = `factor must be a JSON object with a number member "factor" that is zero or positive`

// factorBodyMax bounds the body of PUT /clock. The request carries one number,
// so a kilobyte is far more than it needs, and the bound keeps the endpoint
// from reading whatever an unauthenticated caller sends.
const factorBodyMax = 1 << 10

// NewControlMux builds the routes the simulator serves while it publishes:
// /healthz for the compose health check, and /clock to read and change how fast
// the simulated month runs.
//
// The endpoint changes pacing and nothing else. It cannot alter the month, the
// notifications, or where they go, which is why it carries no credential. What
// keeps it from answering anybody is where it is bound: TALLY_SIM_HTTP_ADDR is
// loopback unless a deployment says otherwise, and the compose stack publishes
// its port on 127.0.0.1. Within that reach the worst a caller does is make
// somebody's demo run at a different speed.
//
// It takes no logger because the command sets the default one, and the only
// line it writes is the factor change.
func NewControlMux(clock *Clock, progress func() Progress) *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeText(w, http.StatusOK, "ok")
	})

	mux.HandleFunc("GET /clock", func(w http.ResponseWriter, _ *http.Request) {
		writeDocument(w, document(clock, progress))
	})

	mux.HandleFunc("PUT /clock", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			// The pointer is what tells a missing member from a factor of 0, and 0
			// is a value the clock accepts: it stops virtual time and lets the run
			// publish as fast as the broker takes it.
			Factor *float64 `json:"factor"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, factorBodyMax)).Decode(&body); err != nil {
			writeText(w, http.StatusBadRequest, badFactorBody)
			return
		}
		if body.Factor == nil {
			writeText(w, http.StatusBadRequest, badFactorBody)
			return
		}
		if err := clock.SetFactor(*body.Factor); err != nil {
			writeText(w, http.StatusBadRequest, badFactorBody)
			return
		}

		slog.Info("the factor changed", "factor", *body.Factor)
		writeDocument(w, document(clock, progress))
	})

	return mux
}

// document reads the state both /clock routes answer with. The clock and the
// progress are read here rather than held, so the answer is of the instant the
// request arrived.
func document(clock *Clock, progress func() Progress) clockDocument {
	p := progress()
	return clockDocument{
		VirtualNow: instant(clock.Now()),
		Factor:     clock.Factor(),
		Published:  p.Published,
		Total:      p.Total,
		PeriodFrom: instant(p.From),
		PeriodTo:   instant(p.To),
	}
}

// instant renders a time the way the document reports it.
func instant(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// writeText answers in plain text, the way the collector's probes answer: what
// reads /healthz is a health check that looks at the status, and a refused PUT
// says in one sentence what a usable body is.
func writeText(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// writeDocument answers with the clock document. The encoding is done before
// the status is written so that a failure can still be reported as one, though
// with two strings, a float, and two ints there is nothing here that fails to
// marshal.
func writeDocument(w http.ResponseWriter, doc clockDocument) {
	encoded, err := json.Marshal(doc)
	if err != nil {
		writeText(w, http.StatusInternalServerError, "the clock could not be encoded")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}
