// This file is in the package rather than beside it because the verifier is
// unexported: EnsureConsoleRole draws a fresh salt on every call, so a known
// answer is reachable only through the function it calls.
package store

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"regexp"
	"strings"
	"testing"
)

// The exchange of RFC 7677 section 3, the SCRAM-SHA-256 test vector. The salt
// and the iteration count are the server's, the proof and the signature are what
// a verifier derived from "pencil" under them has to reproduce.
const (
	rfc7677Password        = "pencil"
	rfc7677Salt            = "W22ZaJ0SNY7soEsUEjb6gQ=="
	rfc7677ClientFirstBare = "n=user,r=rOprNGfwEbeRWgbNEkqO"
	rfc7677ServerFirst     = "r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096"
	rfc7677ClientFinal     = "c=biws,r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0"
	rfc7677ClientProof     = "dHzbZapWIk4jUhN+Ute9ytag9zjfMHgsqmmiz7AndVQ="
	rfc7677ServerSignature = "6rriTRBi23WpRR/wtup+mMhUZUn/dB5nLTJRsjl95G4="
)

// verifierPattern is the form PostgreSQL stores a SCRAM-SHA-256 verifier in.
// Its alphabet is what lets EnsureConsoleRole write the verifier into a
// statement as a literal: it holds no quote and no backslash.
var verifierPattern = regexp.MustCompile(`^SCRAM-SHA-256\$4096:[A-Za-z0-9+/=]+\$[A-Za-z0-9+/=]+:[A-Za-z0-9+/=]+$`)

func TestScramVerifier(t *testing.T) {
	t.Run("derives the verifier of the RFC 7677 test vector", func(t *testing.T) {
		got, err := scramVerifier(rfc7677Password, decode(t, rfc7677Salt))
		if err != nil {
			t.Fatalf("scramVerifier() error = %v, want nil", err)
		}

		want := "SCRAM-SHA-256$4096:W22ZaJ0SNY7soEsUEjb6gQ==$" +
			"WG5d8oPm3OtcPnkdi4Uo7BkeZkBFzpcXkuLmtbsT4qY=:" +
			"wfPLwcE6nTWhTAmQ7tl2KeoiWGPlZqQxSrmfPwDl2dU="
		if got != want {
			t.Errorf("scramVerifier() = %q, want %q", got, want)
		}
	})

	t.Run("lets the server and the client of the RFC exchange authenticate", func(t *testing.T) {
		// The constant above is anchored to the RFC itself: the server key signs
		// the exchange the way the RFC's server did, and the client key the RFC's
		// proof carries hashes to the stored key.
		got, err := scramVerifier(rfc7677Password, decode(t, rfc7677Salt))
		if err != nil {
			t.Fatalf("scramVerifier() error = %v, want nil", err)
		}
		_, keys, _ := strings.Cut(strings.TrimPrefix(got, "SCRAM-SHA-256$4096:"), "$")
		storedB64, serverB64, found := strings.Cut(keys, ":")
		if !found {
			t.Fatalf("scramVerifier() = %q, want StoredKey:ServerKey after the salt", got)
		}
		storedKey, serverKey := decode(t, storedB64), decode(t, serverB64)

		authMessage := rfc7677ClientFirstBare + "," + rfc7677ServerFirst + "," + rfc7677ClientFinal

		serverSignature := base64.StdEncoding.EncodeToString(hmacSHA256(serverKey, authMessage))
		if serverSignature != rfc7677ServerSignature {
			t.Errorf("ServerSignature = %s, want the RFC's %s", serverSignature, rfc7677ServerSignature)
		}

		clientSignature := hmacSHA256(storedKey, authMessage)
		clientKey := decode(t, rfc7677ClientProof)
		for i := range clientKey {
			clientKey[i] ^= clientSignature[i]
		}
		if hashed := sha256.Sum256(clientKey); !bytes.Equal(hashed[:], storedKey) {
			t.Error("the client key the RFC's proof carries does not hash to StoredKey")
		}
	})

	t.Run("writes only base64 after the prefix whatever the salt", func(t *testing.T) {
		for range 64 {
			salt := make([]byte, scramSaltLen)
			if _, err := rand.Read(salt); err != nil {
				t.Fatalf("drawing a salt: %v", err)
			}

			got, err := scramVerifier("!~"+rfc7677Password, salt)
			if err != nil {
				t.Fatalf("scramVerifier() error = %v, want nil", err)
			}
			if !verifierPattern.MatchString(got) {
				t.Fatalf("scramVerifier() = %q, want it to match %s", got, verifierPattern)
			}
		}
	})
}

// decode is the standard base64 value s spells.
func decode(t *testing.T, s string) []byte {
	t.Helper()

	decoded, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("decoding %q: %v", s, err)
	}
	return decoded
}
