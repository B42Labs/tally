package signin

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// The fixture's names are no substrings of the fixed error texts, which is what
// lets a test assert that an error carries none of them.
const (
	testUser     = "alice-operator"
	testPassword = "hunter2-plain"
)

const wantChallenge = `Basic realm="Tally console", charset="UTF-8"`

// hashOf is a bcrypt hash at the cheapest cost the console accepts, so a table
// can afford one per row.
func hashOf(t *testing.T, password string) string {
	t.Helper()

	hash, err := bcrypt.GenerateFromPassword([]byte(password), minBcryptCost)
	if err != nil {
		t.Fatalf("hashing the fixture: %v", err)
	}
	return string(hash)
}

// recorded is a log a test can read back. The handler writes every level, so an
// absent line means the code did not log it.
func recorded() (*slog.Logger, *bytes.Buffer) {
	logged := &bytes.Buffer{}
	return slog.New(slog.NewTextHandler(logged, &slog.HandlerOptions{Level: slog.LevelDebug})), logged
}

// newTestBasic builds the sign-in over the fixture credential.
func newTestBasic(t *testing.T) (*Basic, *bytes.Buffer) {
	t.Helper()

	logger, logged := recorded()
	basic, err := NewBasic(testUser+":"+hashOf(t, testPassword), logger)
	if err != nil {
		t.Fatalf("NewBasic: %v", err)
	}
	return basic, logged
}

// comparisonCounter wraps the real comparison and counts how often it ran.
func comparisonCounter(basic *Basic) *atomic.Int32 {
	count := &atomic.Int32{}
	wrapped := basic.compare
	basic.compare = func(hash, password []byte) error {
		count.Add(1)
		return wrapped(hash, password)
	}
	return count
}

// call sends one request through Require and reports whether it reached next
// and which operator next saw.
func call(basic *Basic, request *http.Request) (*httptest.ResponseRecorder, bool, string) {
	var (
		reached  bool
		operator string
	)
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		reached = true
		operator = OperatorFrom(r.Context())
	})

	recorder := httptest.NewRecorder()
	basic.Require(next).ServeHTTP(recorder, request)
	return recorder, reached, operator
}

// signedIn is a request carrying user and password.
func signedIn(user, password string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/statement?run=1", nil)
	request.SetBasicAuth(user, password)
	return request
}

// assertRefused checks the answer every refusal shares.
func assertRefused(t *testing.T, recorder *httptest.ResponseRecorder, reached bool) {
	t.Helper()

	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}
	if got := recorder.Header().Get("WWW-Authenticate"); got != wantChallenge {
		t.Errorf("WWW-Authenticate = %q, want %q", got, wantChallenge)
	}
	if reached {
		t.Error("the refused request reached the next handler")
	}
}

// assertBusy checks the answer to a request that never got a comparison: a 503
// that asks the client to come back and does not ask for the password again.
func assertBusy(t *testing.T, recorder *httptest.ResponseRecorder, reached bool, logged *bytes.Buffer) {
	t.Helper()

	if recorder.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
	if got := recorder.Header().Get("Retry-After"); got != "5" {
		t.Errorf("Retry-After = %q, want 5", got)
	}
	if got := recorder.Header().Get("WWW-Authenticate"); got != "" {
		t.Errorf("WWW-Authenticate = %q, want none", got)
	}
	if reached {
		t.Error("the refused request reached the next handler")
	}
	if !strings.Contains(logged.String(), `level=WARN msg="sign-in busy"`) {
		t.Errorf("the log does not carry the busy line at WARN:\n%s", logged)
	}
}

func TestNewBasicAccepts(t *testing.T) {
	t.Parallel()

	hash := hashOf(t, testPassword)
	withMinor := func(minor string) string { return hash[:2] + minor + hash[3:] }
	line := testUser + ":" + hash

	cases := []struct {
		name  string
		input string
	}{
		{name: "the 2a prefix", input: testUser + ":" + withMinor("a")},
		{name: "the 2b prefix", input: testUser + ":" + withMinor("b")},
		{name: "the 2y prefix htpasswd writes", input: testUser + ":" + withMinor("y")},
		{name: "the trailing blank line htpasswd -n prints", input: line + "\n\n"},
		{name: "CRLF line endings", input: line + "\r\n\r\n"},
		{name: "comments around the line", input: "# the operator\n" + line + "\n# end\n"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			basic, err := NewBasic(c.input, nil)
			if err != nil {
				t.Fatalf("NewBasic: %v", err)
			}
			_, reached, operator := call(basic, signedIn(testUser, testPassword))
			if !reached || operator != testUser {
				t.Errorf("the correct credential reached next = %v as %q", reached, operator)
			}
		})
	}
}

func TestNewBasicTakesANilLogger(t *testing.T) {
	t.Parallel()

	basic, err := NewBasic(testUser+":"+hashOf(t, testPassword), nil)
	if err != nil {
		t.Fatalf("NewBasic: %v", err)
	}

	recorder, reached, _ := call(basic, httptest.NewRequest(http.MethodGet, "/", nil))

	assertRefused(t, recorder, reached)
}

func TestNewBasicRefuses(t *testing.T) {
	t.Parallel()

	hash := hashOf(t, testPassword)
	valid := testUser + ":" + hash

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{name: "the empty string", input: "", want: "the credential holds no user:hash line"},
		{
			name:  "only comments and blank lines",
			input: "# nothing here\n\n   \n# still nothing\n",
			want:  "the credential holds no user:hash line",
		},
		{name: "a line without a colon", input: testPassword, want: "line 1 is not user:hash"},
		{name: "a line naming no user", input: ":" + hash, want: "line 1 names no user"},
		{name: "a plain password", input: testUser + ":" + testPassword, want: "line 1: the hash is not bcrypt"},
		{
			name:  "an MD5 hash",
			input: testUser + ":$apr1$hunter2p$0123456789012345678901",
			want:  "line 1: the hash is not bcrypt",
		},
		{
			name:  "a SHA1 hash",
			input: testUser + ":{SHA}qUqP5cyxm6YcTAhz05Hph5gvu9M=",
			want:  "line 1: the hash is not bcrypt",
		},
		{
			name:  "a SHA1 hash padded to the length of a bcrypt one",
			input: testUser + ":{SHA}" + strings.Repeat("q", bcryptHashLength-len("{SHA}")),
			want:  "line 1: the hash is not bcrypt",
		},
		{
			name:  "a hash cut by one character",
			input: testUser + ":" + hash[:len(hash)-1],
			want:  "line 1: the hash is not bcrypt",
		},
		{
			name:  "a hash one character too long",
			input: testUser + ":" + hash + "x",
			want:  "line 1: the hash is not bcrypt",
		},
		{
			name:  "a hash below the lowest cost",
			input: testUser + ":" + hash[:4] + "09" + hash[6:],
			want:  "line 1: the bcrypt cost is below 10",
		},
		{
			name:  "a hash with an illegal character in its salt",
			input: testUser + ":" + hash[:8] + "!" + hash[9:],
			want:  "line 1: the hash is not bcrypt",
		},
		{
			name:  "the hash of the empty password",
			input: testUser + ":" + hashOf(t, ""),
			want:  "line 1: the hash is of the empty password",
		},
		{name: "two valid lines", input: valid + "\n" + valid + "\n", want: "the credential holds 2 entries"},
		{
			name:  "a bad line counted by its physical number",
			input: "# the operator\n\r\n" + testPassword,
			want:  "line 3 is not user:hash",
		},
		{
			name:  "a bad second entry counted by its physical number",
			input: valid + "\n\n" + testUser + ":" + testPassword,
			want:  "line 3: the hash is not bcrypt",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			_, err := NewBasic(c.input, nil)
			if err == nil {
				t.Fatalf("NewBasic(%q) error = nil, want one containing %q", c.input, c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("NewBasic error = %q, want it to contain %q", err, c.want)
			}
			secrets := []string{testUser, testPassword, hash}
			if _, rowHash, _ := strings.Cut(c.input, ":"); rowHash != "" {
				secrets = append(secrets, rowHash)
			}
			for _, secret := range secrets {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("NewBasic error = %q, want it to carry none of the input", err)
				}
			}
		})
	}
}

// A hash past the limit has to be refused before the trial comparison, because
// that comparison is what would hang the startup. The check is on the comparison
// not running, since timing it would need the hang.
func TestParseCredentialStopsBeforeAComparisonAboveTheCostLimit(t *testing.T) {
	t.Parallel()

	hash := hashOf(t, testPassword)
	tooDear := hash[:4] + "18" + hash[6:]

	_, _, err := parseCredential(testUser+":"+tooDear, func(_, _ []byte) error {
		t.Error("the comparison ran at a cost above the limit")
		return bcrypt.ErrMismatchedHashAndPassword
	})
	if err == nil || !strings.Contains(err.Error(), "line 1: the bcrypt cost is above 17") {
		t.Fatalf("parseCredential error = %v, want the cost refusal", err)
	}
	for _, secret := range []string{testUser, tooDear} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("parseCredential error = %q, want it to carry none of the input", err)
		}
	}
}

func TestParseCredentialAcceptsTheCostLimit(t *testing.T) {
	t.Parallel()

	hash := hashOf(t, testPassword)
	atLimit := hash[:4] + "17" + hash[6:]

	_, _, err := parseCredential(testUser+":"+atLimit, func(_, _ []byte) error {
		return bcrypt.ErrMismatchedHashAndPassword
	})
	if err != nil {
		t.Errorf("parseCredential at the cost limit: %v", err)
	}
}

func TestOperatorFromAContextNoSignInTouched(t *testing.T) {
	t.Parallel()

	if got := OperatorFrom(context.Background()); got != "" {
		t.Errorf("OperatorFrom = %q, want the empty string", got)
	}
}

func TestRequireServesTheCorrectCredential(t *testing.T) {
	t.Parallel()

	basic, _ := newTestBasic(t)

	recorder, reached, operator := call(basic, signedIn(testUser, testPassword))

	if !reached {
		t.Fatal("the correct credential did not reach the next handler")
	}
	if recorder.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if operator != testUser {
		t.Errorf("OperatorFrom = %q, want %q", operator, testUser)
	}
}

func TestRequireSignsInAPasswordContainingAColon(t *testing.T) {
	t.Parallel()

	const password = "pass:with:colons"
	basic, err := NewBasic(testUser+":"+hashOf(t, password), nil)
	if err != nil {
		t.Fatalf("NewBasic: %v", err)
	}

	if _, reached, _ := call(basic, signedIn(testUser, password)); !reached {
		t.Error("a password containing a colon did not sign in")
	}
}

func TestRequireRefusesWithoutACredential(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		header string
	}{
		{name: "no header"},
		{name: "another scheme", header: "Bearer x"},
		{name: "undecodable base64", header: "Basic !!!"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			basic, logged := newTestBasic(t)
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			if c.header != "" {
				request.Header.Set("Authorization", c.header)
			}

			recorder, reached, _ := call(basic, request)

			assertRefused(t, recorder, reached)
			if strings.Contains(logged.String(), "level=WARN") {
				t.Errorf("the log carries a WARN line:\n%s", logged)
			}
			if !strings.Contains(logged.String(), `level=DEBUG msg="sign-in refused"`) {
				t.Errorf("the log does not carry the refusal at DEBUG:\n%s", logged)
			}
		})
	}
}

func TestRequireRefusesAWrongCredential(t *testing.T) {
	t.Parallel()

	const (
		sentUser     = "mallory-typed-this"
		sentPassword = "guess-number-seven"
	)

	cases := []struct {
		name           string
		user, password string
	}{
		{name: "a wrong password", user: testUser, password: sentPassword},
		{name: "a wrong user with the right password", user: sentUser, password: testPassword},
		{name: "an empty password", user: testUser, password: ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			basic, logged := newTestBasic(t)

			recorder, reached, _ := call(basic, signedIn(c.user, c.password))

			assertRefused(t, recorder, reached)
			line := logged.String()
			if !strings.Contains(line, `level=WARN msg="sign-in refused"`) {
				t.Errorf("the log does not carry the refusal at WARN:\n%s", line)
			}
			for _, want := range []string{"method=GET", "path=/statement", "remote="} {
				if !strings.Contains(line, want) {
					t.Errorf("the refusal does not carry %q:\n%s", want, line)
				}
			}
			for _, sent := range []string{sentUser, sentPassword, testUser, testPassword} {
				if strings.Contains(line, sent) {
					t.Errorf("the refusal carries what was sent, %q:\n%s", sent, line)
				}
			}
		})
	}
}

func TestRequireRefusesWhenTheComparisonFails(t *testing.T) {
	t.Parallel()

	basic, logged := newTestBasic(t)
	basic.compare = func(_, _ []byte) error { return errors.New("the hash went bad") }

	recorder, reached, _ := call(basic, signedIn(testUser, testPassword))

	assertRefused(t, recorder, reached)
	if !strings.Contains(logged.String(), `level=WARN msg="sign-in refused"`) {
		t.Errorf("the log does not carry the refusal at WARN:\n%s", logged)
	}
}

// bcrypt at the cost an operator picks takes a noticeable share of a core, so
// how often it runs is the behaviour here and not a detail of the code.
func TestRequireRemembersAVerifiedPassword(t *testing.T) {
	t.Parallel()

	t.Run("two correct requests run one comparison", func(t *testing.T) {
		t.Parallel()

		basic, _ := newTestBasic(t)
		count := comparisonCounter(basic)

		for range 2 {
			if _, reached, _ := call(basic, signedIn(testUser, testPassword)); !reached {
				t.Fatal("the correct credential did not reach the next handler")
			}
		}
		if got := count.Load(); got != 1 {
			t.Errorf("comparisons = %d, want 1", got)
		}
	})

	t.Run("a wrong password after a sign-in runs one and forgets nothing", func(t *testing.T) {
		t.Parallel()

		basic, _ := newTestBasic(t)
		call(basic, signedIn(testUser, testPassword))
		count := comparisonCounter(basic)

		recorder, reached, _ := call(basic, signedIn(testUser, "wrong"))
		assertRefused(t, recorder, reached)
		if got := count.Load(); got != 1 {
			t.Errorf("comparisons for the wrong password = %d, want 1", got)
		}

		if _, reached, _ := call(basic, signedIn(testUser, testPassword)); !reached {
			t.Fatal("the correct credential no longer signs in")
		}
		if got := count.Load(); got != 1 {
			t.Errorf("comparisons after the correct credential = %d, want still 1", got)
		}
	})

	// The comparison runs for a wrong user name as well, so the response time
	// does not reveal whether the name was right.
	wrongUser := []struct {
		name     string
		signedIn bool
	}{
		{name: "a wrong user runs one comparison before any sign-in"},
		{name: "the remembered password does not sign in another user", signedIn: true},
	}
	for _, c := range wrongUser {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			basic, _ := newTestBasic(t)
			if c.signedIn {
				call(basic, signedIn(testUser, testPassword))
			}
			count := comparisonCounter(basic)

			recorder, reached, _ := call(basic, signedIn("someone-else", testPassword))

			assertRefused(t, recorder, reached)
			if got := count.Load(); got != 1 {
				t.Errorf("comparisons for a wrong user = %d, want 1", got)
			}
		})
	}
}

// blockingCompare replaces the comparison with one that waits on release, and
// reports how many ran at once at most and how many have started.
func blockingCompare(basic *Basic) (release chan struct{}, started chan struct{}, peak *atomic.Int32) {
	release = make(chan struct{})
	started = make(chan struct{}, 64)
	peak = &atomic.Int32{}
	running := &atomic.Int32{}

	basic.compare = func(_, _ []byte) error {
		now := running.Add(1)
		for {
			seen := peak.Load()
			if now <= seen || peak.CompareAndSwap(seen, now) {
				break
			}
		}
		started <- struct{}{}
		<-release
		running.Add(-1)
		return bcrypt.ErrMismatchedHashAndPassword
	}
	return release, started, peak
}

func TestRequireCapsTheComparisons(t *testing.T) {
	t.Parallel()

	t.Run("ten concurrent wrong requests never run more than two", func(t *testing.T) {
		t.Parallel()

		basic, _ := newTestBasic(t)
		release, started, peak := blockingCompare(basic)

		var requests sync.WaitGroup
		for range 10 {
			requests.Go(func() { call(basic, signedIn(testUser, "wrong")) })
		}

		// Two comparisons running is the state the other eight are held out of.
		for range maxComparisons {
			<-started
		}
		select {
		case <-started:
			t.Error("a third comparison started while two were running")
		case <-time.After(100 * time.Millisecond):
		}
		close(release)
		requests.Wait()

		if got := peak.Load(); got != maxComparisons {
			t.Errorf("peak concurrent comparisons = %d, want %d", got, maxComparisons)
		}
	})

	// holdBothSlots starts two wrong requests and returns once both are inside
	// a comparison. The returned function lets them finish.
	holdBothSlots := func(t *testing.T, basic *Basic) func() {
		t.Helper()

		release, started, _ := blockingCompare(basic)
		var requests sync.WaitGroup
		for range maxComparisons {
			requests.Go(func() { call(basic, signedIn(testUser, "wrong")) })
		}
		for range maxComparisons {
			<-started
		}
		return func() {
			close(release)
			requests.Wait()
		}
	}

	t.Run("a remembered credential is served while both slots are held", func(t *testing.T) {
		t.Parallel()

		basic, _ := newTestBasic(t)
		call(basic, signedIn(testUser, testPassword))

		finish := holdBothSlots(t, basic)
		t.Cleanup(finish)

		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()

		if _, reached, _ := call(basic, signedIn(testUser, testPassword).WithContext(ctx)); !reached {
			t.Error("a signed-in operator waited behind the wrong guesses")
		}
	})

	t.Run("a request whose context ends while both slots are held runs no comparison", func(t *testing.T) {
		t.Parallel()

		basic, logged := newTestBasic(t)
		finish := holdBothSlots(t, basic)
		t.Cleanup(finish)

		// Every comparison from here on would block, and none may start.
		count := comparisonCounter(basic)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		recorder, reached, _ := call(basic, signedIn(testUser, "wrong").WithContext(ctx))

		assertRefused(t, recorder, reached)
		if got := count.Load(); got != 0 {
			t.Errorf("comparisons = %d, want 0", got)
		}
		if strings.Contains(logged.String(), "level=WARN") {
			t.Errorf("the log carries a WARN line:\n%s", logged)
		}
	})

	t.Run("a request whose wait runs out while both slots are held is answered busy", func(t *testing.T) {
		t.Parallel()

		basic, logged := newTestBasic(t)
		basic.slotWait = 10 * time.Millisecond
		finish := holdBothSlots(t, basic)
		t.Cleanup(finish)

		// Every comparison from here on would block, and none may start. The
		// context turns a wait that never runs out into a failure and not a hang.
		count := comparisonCounter(basic)
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()

		recorder, reached, _ := call(basic, signedIn(testUser, "wrong").WithContext(ctx))

		assertBusy(t, recorder, reached, logged)
		if got := count.Load(); got != 0 {
			t.Errorf("comparisons = %d, want 0", got)
		}
	})

	t.Run("a request past the queue limit is answered busy at once", func(t *testing.T) {
		t.Parallel()

		basic, logged := newTestBasic(t)
		count := comparisonCounter(basic)
		basic.waiting.Store(maxWaiting)

		recorder, reached, _ := call(basic, signedIn(testUser, "wrong"))

		assertBusy(t, recorder, reached, logged)
		if got := count.Load(); got != 0 {
			t.Errorf("comparisons = %d, want 0", got)
		}
		if got := basic.waiting.Load(); got != maxWaiting {
			t.Errorf("waiting after the refusal = %d, want %d", got, maxWaiting)
		}
	})

	t.Run("a finished comparison gives its place in the queue back", func(t *testing.T) {
		t.Parallel()

		basic, _ := newTestBasic(t)

		for range maxWaiting + 1 {
			recorder, reached, _ := call(basic, signedIn(testUser, "wrong"))
			assertRefused(t, recorder, reached)
		}
	})

	t.Run("a burst of busy answers writes one line and counts the rest", func(t *testing.T) {
		t.Parallel()

		const burst = 20

		basic, logged := newTestBasic(t)
		basic.waiting.Store(maxWaiting)

		var requests sync.WaitGroup
		for range burst {
			requests.Go(func() { call(basic, signedIn(testUser, "wrong")) })
		}
		requests.Wait()

		if got := strings.Count(logged.String(), `msg="sign-in busy"`); got != 1 {
			t.Fatalf("busy lines after the burst = %d, want 1:\n%s", got, logged)
		}

		// A zero timestamp is an interval that has run out.
		basic.lastBusyLog.Store(0)
		recorder, reached, _ := call(basic, signedIn(testUser, "wrong"))

		assertBusy(t, recorder, reached, logged)
		if got := strings.Count(logged.String(), `msg="sign-in busy"`); got != 2 {
			t.Errorf("busy lines after the interval = %d, want 2:\n%s", got, logged)
		}

		// An answer counted between the winning CompareAndSwap and its Swap is on
		// the first line, so only the sum over both lines is fixed.
		total := 0
		for _, match := range regexp.MustCompile(`suppressed=(\d+)`).FindAllStringSubmatch(logged.String(), -1) {
			count, err := strconv.Atoi(match[1])
			if err != nil {
				t.Fatalf("parsing %q: %v", match[0], err)
			}
			total += count
		}
		if total != burst-1 {
			t.Errorf("suppressed answers across the lines = %d, want %d:\n%s", total, burst-1, logged)
		}
	})

	t.Run("a clock stepped back does not silence the busy line", func(t *testing.T) {
		t.Parallel()

		basic, logged := newTestBasic(t)
		basic.waiting.Store(maxWaiting)
		// A last line in the future is what a clock stepped back leaves behind.
		basic.lastBusyLog.Store(time.Now().Add(time.Hour).UnixNano())

		recorder, reached, _ := call(basic, signedIn(testUser, "wrong"))
		assertBusy(t, recorder, reached, logged)

		// The step restarts the interval, so the next answer writes no line.
		call(basic, signedIn(testUser, "wrong"))
		if got := strings.Count(logged.String(), `msg="sign-in busy"`); got != 1 {
			t.Errorf("busy lines after the step = %d, want 1:\n%s", got, logged)
		}
	})
}
