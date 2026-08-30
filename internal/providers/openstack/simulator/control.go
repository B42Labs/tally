package simulator

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
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
	// Held is how many notifications the held-back switch still keeps off the
	// bus. It is 0 for a run without the switch, and 0 once a release let them
	// out.
	Held int
	// Holding is whether the run waits for a release: true from the moment the
	// last regular notification is on the bus until a release lets the held share
	// out. It is the one signal a caller may poll before releasing, because it is
	// the very state a release acts on, while the counts reach their hold values
	// a moment before the run enters the hold.
	Holding bool
}

// clockDocument is what /clock answers. The instants are RFC 3339 in UTC
// because the reader is a person or a script watching a run, and a zoneless
// timestamp would leave both guessing which zone the simulator ran in.
type clockDocument struct {
	VirtualNow string  `json:"virtual_now"`
	Factor     float64 `json:"factor"`
	Published  int     `json:"published"`
	Total      int     `json:"total"`
	Held       int     `json:"held"`
	Holding    bool    `json:"holding"`
	PeriodFrom string  `json:"period_from"`
	PeriodTo   string  `json:"period_to"`
}

// badFactorBody is the single answer every refused PUT /clock gets. One answer
// covers a body that is not JSON, one without the member, and one whose factor
// is negative, because what the caller has to do about it is the same in all
// three.
const badFactorBody = `factor must be a JSON object with a number member "factor" that is zero or positive`

// badReleaseOrigin is what a release a browser sent is refused with, and
// badFactorOrigin a factor change. Both are the answer fromBrowser gives, and
// each names what it refused, so a caller reads which of the two requests was
// turned away.
const (
	badReleaseOrigin = "release does not take a request a browser sent"
	badFactorOrigin  = "the factor does not take a request a browser sent"
)

// badReleaseType is what a POST /release with a body of another media type is
// refused with. It is the second of the two guards: what a page sent is already
// refused by its Origin, and a request without one is held to a body a script
// meant to send. curl -X POST sends no content type and is unaffected.
const badReleaseType = "release takes application/json or no body"

// factorBodyMax bounds the body of PUT /clock. The request carries one number,
// so a kilobyte is far more than it needs, and the bound keeps the endpoint
// from reading whatever an unauthenticated caller sends.
const factorBodyMax = 1 << 10

// The three refusals POST /release answers with. Each of them names the run it
// arrived at, because what a caller does about a release is different in all
// three: turn the switch on, wait, or nothing.
var (
	errNothingHeld     = errors.New("nothing is held back: the run was started without the held-back switch")
	errStillPublishing = errors.New(
		"the month is still publishing; release once /clock reports holding true")
	errAlreadyReleased = errors.New("the held-back notifications were already released")
)

// The phases a holdback runs through. A run is publishing while the regular
// notifications go out, holding once the last of them is on the bus, and
// released from the moment a caller asked for the rest.
const (
	phasePublishing int32 = iota
	phaseHolding
	phaseReleased
)

// holdback is what a run with the held-back switch keeps off the bus and the
// one thing POST /release changes. The handler calls release while broadcast
// waits on the released channel, so the answer to the request is written before
// the held notifications go out.
type holdback struct {
	// count is how many notifications the run holds back, fixed when the run
	// starts.
	count int
	// phase is one of the three constants above, read and written from the
	// handler's goroutine and the publishing one.
	phase atomic.Int32
	// released is closed by the release that succeeds, which is what wakes the
	// run holding on it.
	released chan struct{}
}

// newHoldback holds count notifications back. A count of 0 is a run without the
// switch, which refuses every release.
func newHoldback(count int) *holdback {
	return &holdback{count: count, released: make(chan struct{})}
}

// release lets the held notifications out, or reports why it cannot. The swap
// is what makes the first release the only one: a second caller finds the phase
// already moved on and closes nothing.
func (h *holdback) release() error {
	if h.count == 0 {
		return errNothingHeld
	}
	if h.phase.CompareAndSwap(phaseHolding, phaseReleased) {
		close(h.released)
		return nil
	}
	if h.phase.Load() == phaseReleased {
		return errAlreadyReleased
	}
	return errStillPublishing
}

// holding is whether a release would be granted: the run has put the last
// regular notification on the bus and waits for one. It reads the very field
// release swaps, so a caller that saw it true is not refused by the release it
// sends next; the published count reaches its hold value a moment earlier and
// is no signal to release on.
func (h *holdback) holding() bool {
	return h.phase.Load() == phaseHolding
}

// held is how many notifications are still off the bus, which is none once they
// were released.
func (h *holdback) held() int {
	if h.phase.Load() == phaseReleased {
		return 0
	}
	return h.count
}

// NewControlMux builds the routes the simulator serves while it publishes:
// /healthz for the compose health check, /clock to read and change how fast the
// simulated month runs, and /release to let out the notifications a run with
// the held-back switch keeps back.
//
// The endpoint changes when the month goes out and nothing else. It cannot
// alter the month, the notifications, or where they go, which is why it carries
// no credential: a release publishes the very notifications the run generated,
// each under the timestamp it always carried. What keeps it from answering
// anybody is where it is bound: TALLY_SIM_HTTP_ADDR is loopback unless a
// deployment says otherwise, and the compose stack publishes its port on
// 127.0.0.1. Within that reach the worst a caller does is make somebody's demo
// run at a different speed, or end its hold early. What a page in a browser
// could send there unasked is refused a step earlier: a request a page makes
// carries Origin however the page sent it, and one that carries it answers 403
// on both routes that change something, without reaching the clock or the hold.
// Beyond that a release takes a JSON body or none, and a form-encoded one
// answers 415.
//
// A nil release is a run that holds nothing back, and every request to /release
// is refused as one.
//
// It takes no logger because the command sets the default one, and the only
// lines it writes are the factor change and the release.
func NewControlMux(clock *Clock, progress func() Progress, release func() error) *http.ServeMux {
	if release == nil {
		release = func() error { return errNothingHeld }
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeText(w, http.StatusOK, "ok")
	})

	mux.HandleFunc("GET /clock", func(w http.ResponseWriter, _ *http.Request) {
		writeDocument(w, document(clock, progress))
	})

	mux.HandleFunc("PUT /clock", func(w http.ResponseWriter, r *http.Request) {
		if fromBrowser(r) {
			writeText(w, http.StatusForbidden, badFactorOrigin)
			return
		}
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

	mux.HandleFunc("POST /release", func(w http.ResponseWriter, r *http.Request) {
		if fromBrowser(r) {
			writeText(w, http.StatusForbidden, badReleaseOrigin)
			return
		}
		if contentType := r.Header.Get("Content-Type"); contentType != "" &&
			!strings.HasPrefix(contentType, "application/json") {
			writeText(w, http.StatusUnsupportedMediaType, badReleaseType)
			return
		}
		// The document is read before the release rather than after it, because a
		// release wakes the run at once: read afterwards, the published count would
		// lie somewhere between the month before the release and the month after
		// it, at a different place on every run.
		doc := document(clock, progress)
		if err := release(); err != nil {
			writeText(w, http.StatusConflict, err.Error())
			return
		}

		slog.Info("the held-back notifications were released")
		// What the answer reports is the month one release short, and the release
		// this request granted: the two members the release changed are stated as
		// it left them rather than as they stood a line earlier.
		doc.Held = 0
		doc.Holding = false
		writeDocument(w, doc)
	})

	return mux
}

// fromBrowser is whether a page in a browser sent the request. A browser
// appends Origin to every request whose method is neither GET nor HEAD, the
// same-origin ones included, and Sec-Fetch-Site alongside it, whatever mode the
// page used and whatever content type it managed to set. That is what both
// routes that change a run are guarded with: a form submit is stopped by its
// content type already, while a bodyless fetch sets none of its own and needs
// no preflight. A cross-origin PUT is stopped a step earlier, by the preflight
// the mux answers 405, but a page that resolved the endpoint's address itself
// sends one same-origin, where no preflight stands in the way either. curl and
// a script send neither header and are unaffected.
func fromBrowser(r *http.Request) bool {
	return r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") != ""
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
		Held:       p.Held,
		Holding:    p.Holding,
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
// with two strings, a float, and three ints there is nothing here that fails to
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
