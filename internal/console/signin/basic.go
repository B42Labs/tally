package signin

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	// minBcryptCost is the lowest cost the console accepts. The cost is what
	// limits guessing, and htpasswd -B without -C writes 5.
	minBcryptCost = 10
	// maxBcryptCost is the highest cost htpasswd -C accepts. It bounds the trial
	// comparison at startup, which an unchecked cost of 31 would hang.
	maxBcryptCost = 17
	// maxComparisons caps the bcrypt comparisons running at once, and with them
	// the CPU that wrong guesses can spend.
	maxComparisons = 2
	// maxSlotWait bounds how long a request waits for a comparison, not the
	// comparison itself. At the documented cost of 12 a queue of guesses stays
	// inside the shutdown budget of the process; at cost 17 one comparison
	// after a full wait does not.
	maxSlotWait = 5 * time.Second
	// maxWaiting caps the requests waiting for or inside a comparison, and with
	// them the memory wrong guesses can hold. This many clear in about three
	// seconds at the documented cost of 12.
	maxWaiting = 32
	// busyLogInterval is the least time between two busy lines. A request past
	// maxWaiting is answered with no comparison to pace it, so a line per answer
	// would let a client write to the log as fast as it can send.
	busyLogInterval = 10 * time.Second
	// bcryptHashLength is the length of every bcrypt hash. One cut short still
	// passes bcrypt.Cost and then never matches anything.
	bcryptHashLength = 60
	// digestKeySize is the number of random bytes that key the remembered digest.
	digestKeySize = 32

	challenge = `Basic realm="Tally console", charset="UTF-8"`
)

// errNotBcrypt is the answer to every form of the hash other than bcrypt. It
// names nothing of the line, because a misconfigured line can hold a plaintext
// password.
var errNotBcrypt = errors.New("the hash is not bcrypt, make one with htpasswd -nBC 12 <user>")

// errBusy means no comparison ran because too many requests were waiting for
// one or the wait ran out.
var errBusy = errors.New("too many requests are waiting for a comparison")

// Basic is the HTTP Basic sign-in against the one operator credential.
//
// HTTP Basic re-sends the credential with every request and a page is two
// requests, so a verified password is remembered and bcrypt runs once per
// process and password. What is remembered is a digest keyed with random bytes
// of this process, never the password.
type Basic struct {
	user    string
	hash    []byte
	key     []byte
	compare func(hash, password []byte) error
	// slots holds one element per comparison running.
	slots chan struct{}
	// waiting counts the requests waiting for or inside a comparison.
	waiting    atomic.Int32
	slotWait   time.Duration
	remembered atomic.Pointer[[sha256.Size]byte]
	logger     *slog.Logger
	// lastBusyLog is when the last busy line was written, in Unix nanoseconds.
	lastBusyLog atomic.Int64
	// busySuppressed counts the busy answers since then that wrote no line.
	busySuppressed atomic.Int64
}

// NewBasic parses the credential: exactly one user:bcrypt-hash line in htpasswd
// format, with blank lines and # comments ignored. The errors name the physical
// line and never the user, the hash or a password. A nil logger falls back to
// slog.Default().
func NewBasic(htpasswd string, logger *slog.Logger) (*Basic, error) {
	if logger == nil {
		logger = slog.Default()
	}

	user, hash, err := parseCredential(htpasswd, bcrypt.CompareHashAndPassword)
	if err != nil {
		return nil, err
	}

	key := make([]byte, digestKeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("drawing the digest key: %w", err)
	}

	return &Basic{
		user:     user,
		hash:     hash,
		key:      key,
		compare:  bcrypt.CompareHashAndPassword,
		slots:    make(chan struct{}, maxComparisons),
		slotWait: maxSlotWait,
		logger:   logger,
	}, nil
}

// parseCredential walks the text line by line and returns its one entry.
// htpasswd -n prints a trailing blank line and the *_FILE convention trims only
// one newline, so a blank line is not an error. The comparison is a parameter
// so a test can prove that a line past the cost limit never reaches one.
func parseCredential(htpasswd string, compare func(hash, password []byte) error) (string, []byte, error) {
	var (
		user    string
		hash    []byte
		entries int
	)
	for index, line := range strings.Split(htpasswd, "\n") {
		number := index + 1
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		name, hashText, found := strings.Cut(line, ":")
		if !found {
			return "", nil, fmt.Errorf("line %d is not user:hash", number)
		}
		if name == "" {
			return "", nil, fmt.Errorf("line %d names no user", number)
		}
		if err := checkHash([]byte(hashText), compare); err != nil {
			return "", nil, fmt.Errorf("line %d: %w", number, err)
		}

		user, hash = name, []byte(hashText)
		entries++
	}

	switch entries {
	case 0:
		return "", nil, errors.New("the credential holds no user:hash line")
	case 1:
		return user, hash, nil
	default:
		return "", nil, fmt.Errorf("the credential holds %d entries, the console takes exactly one", entries)
	}
}

// checkHash refuses a hash that could not verify a password, and the hash of
// the empty password. The trial comparison is what finds a character bcrypt.Cost
// does not look at, an illegal one in the salt for example.
func checkHash(hash []byte, compare func(hash, password []byte) error) error {
	if len(hash) != bcryptHashLength {
		return errNotBcrypt
	}
	cost, err := bcrypt.Cost(hash)
	if err != nil {
		return errNotBcrypt
	}
	if cost < minBcryptCost {
		return fmt.Errorf("the bcrypt cost is below %d, make one with htpasswd -nBC 12 <user>", minBcryptCost)
	}
	if cost > maxBcryptCost {
		return fmt.Errorf("the bcrypt cost is above %d", maxBcryptCost)
	}

	switch err := compare(hash, nil); {
	case err == nil:
		return errors.New("the hash is of the empty password")
	case errors.Is(err, bcrypt.ErrMismatchedHashAndPassword):
		return nil
	default:
		return errNotBcrypt
	}
}

// Require serves next only for a request carrying the credential. next sees the
// operator through OperatorFrom.
func (b *Basic) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok {
			// Every browser's first request carries no credential, so this is
			// no sign of anything.
			b.refuse(w, r, slog.LevelDebug)
			return
		}

		digest := b.digest(password)
		userOK := subtle.ConstantTimeCompare([]byte(user), []byte(b.user)) == 1

		if !userOK || !b.isRemembered(digest) {
			// The comparison runs for a wrong user name as well, so the response
			// time does not reveal whether the name was right.
			compared, err := b.compareInSlot(r.Context(), password)
			if errors.Is(err, errBusy) {
				b.busy(w, r)
				return
			}
			if !compared {
				b.refuse(w, r, slog.LevelDebug)
				return
			}
			if err != nil || !userOK {
				b.refuse(w, r, slog.LevelWarn)
				return
			}
			b.remembered.Store(&digest)
		}

		next.ServeHTTP(w, r.WithContext(WithOperator(r.Context(), b.user)))
	})
}

// digest is the HMAC-SHA-256 of the password under this process's random key.
func (b *Basic) digest(password string) [sha256.Size]byte {
	mac := hmac.New(sha256.New, b.key)
	mac.Write([]byte(password))
	return [sha256.Size]byte(mac.Sum(nil))
}

// isRemembered reports whether digest is the one of the last verified password.
// Before the first successful sign-in nothing is remembered.
func (b *Basic) isRemembered(digest [sha256.Size]byte) bool {
	remembered := b.remembered.Load()
	return remembered != nil && subtle.ConstantTimeCompare(digest[:], remembered[:]) == 1
}

// compareInSlot runs the bcrypt comparison once a slot is free. compared is
// false when no comparison ran: err is errBusy when too many requests are
// waiting or the wait ran out, and nil when the request's context ended first.
// A remembered credential never comes here, so a flood of wrong guesses does
// not hold up an operator who has already signed in. Nothing is remembered
// after a restart, so a first sign-in during a flood is answered errBusy.
func (b *Basic) compareInSlot(ctx context.Context, password string) (compared bool, err error) {
	defer b.waiting.Add(-1)
	if b.waiting.Add(1) > maxWaiting {
		return false, errBusy
	}

	select {
	case b.slots <- struct{}{}:
		defer func() { <-b.slots }()
	case <-ctx.Done():
		return false, nil
	case <-time.After(b.slotWait):
		return false, errBusy
	}
	return true, b.compare(b.hash, []byte(password))
}

// refuse answers 401 and logs the request. A caller who has not signed in gets
// no navigation, so the answer is plain text rather than the error page. The
// line never carries the sent user name, because people type passwords into
// that field.
func (b *Basic) refuse(w http.ResponseWriter, r *http.Request, level slog.Level) {
	b.logger.Log(r.Context(), level, "sign-in refused",
		"method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr)
	w.Header().Set("WWW-Authenticate", challenge)
	http.Error(w, "sign-in required", http.StatusUnauthorized)
}

// busy answers 503 and logs the request, at most one line per busyLogInterval.
// Each line counts the busy answers that wrote no line since the one before it.
// A 401 would have a browser ask again for a password nobody has checked, so
// this answer carries no challenge.
func (b *Basic) busy(w http.ResponseWriter, r *http.Request) {
	// UnixNano carries no monotonic reading, so a clock stepped back makes elapsed
	// negative. That counts as run out, or the step would silence the line for
	// its own length. last is read before the clock so that a line another
	// request has just written cannot make elapsed negative.
	last := b.lastBusyLog.Load()
	now := time.Now().UnixNano()
	elapsed := now - last
	if (elapsed < 0 || elapsed >= int64(busyLogInterval)) && b.lastBusyLog.CompareAndSwap(last, now) {
		b.logger.WarnContext(r.Context(), "sign-in busy",
			"method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr,
			"suppressed", b.busySuppressed.Swap(0))
	} else {
		b.busySuppressed.Add(1)
	}
	w.Header().Set("Retry-After", strconv.Itoa(int(maxSlotWait/time.Second)))
	http.Error(w, "the sign-in is busy, try again shortly", http.StatusServiceUnavailable)
}
