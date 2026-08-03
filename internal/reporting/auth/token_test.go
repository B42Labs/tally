package auth_test

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"testing"

	"github.com/b42labs/tally/internal/reporting/auth"
)

func TestGenerate(t *testing.T) {
	for name, tc := range map[string]struct {
		generate func() (string, error)
		format   *regexp.Regexp
	}{
		"an ingest token": {generate: auth.GenerateIngestToken, format: regexp.MustCompile(`^tly_i_[0-9a-f]{64}$`)},
		"an api token":    {generate: auth.GenerateAPIToken, format: regexp.MustCompile(`^tly_a_[0-9a-f]{64}$`)},
	} {
		t.Run(name+" carries its prefix and 32 bytes in lower-case hex", func(t *testing.T) {
			token, err := tc.generate()
			if err != nil {
				t.Fatalf("generating the token: %v", err)
			}
			if !tc.format.MatchString(token) {
				t.Errorf("token does not match %s", tc.format)
			}
		})

		t.Run(name+" differs from the one generated before it", func(t *testing.T) {
			first, err := tc.generate()
			if err != nil {
				t.Fatalf("generating the first token: %v", err)
			}
			second, err := tc.generate()
			if err != nil {
				t.Fatalf("generating the second token: %v", err)
			}
			if first == second {
				t.Error("two successive tokens are identical")
			}
		})
	}
}

func TestHashToken(t *testing.T) {
	t.Run("digests the token as it stands", func(t *testing.T) {
		token, err := auth.GenerateIngestToken()
		if err != nil {
			t.Fatalf("generating the token: %v", err)
		}

		sum := sha256.Sum256([]byte(token))
		if got, want := auth.HashToken(token), hex.EncodeToString(sum[:]); got != want {
			t.Errorf("HashToken() = %q, want %q", got, want)
		}
	})

	t.Run("covers the prefix, so the two token kinds never collide", func(t *testing.T) {
		const random = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

		if auth.HashToken("tly_i_"+random) == auth.HashToken("tly_a_"+random) {
			t.Error("an ingest token and an api token with the same random part hash alike")
		}
	})
}
