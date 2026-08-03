package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// The prefixes that say what a token opens. A prefix is part of the token and
// therefore part of its hash, and it lets an operator who finds a leaked token
// tell an ingest credential from a query token without looking it up.
const (
	ingestTokenPrefix = "tly_i_"
	apiTokenPrefix    = "tly_a_"
)

// tokenBytes is how much entropy a token carries. Hex encoding turns it into
// the 64 characters that follow the prefix.
const tokenBytes = 32

// GenerateIngestToken returns a new ingest token: tly_i_ followed by 32 random
// bytes in lower-case hex. The caller shows the token to the operator once and
// stores nothing but HashToken's digest.
func GenerateIngestToken() (string, error) {
	return generateToken(ingestTokenPrefix)
}

// GenerateAPIToken returns a new query token: tly_a_ followed by 32 random
// bytes in lower-case hex. The caller shows the token to the operator once and
// stores nothing but HashToken's digest.
func GenerateAPIToken() (string, error) {
	return generateToken(apiTokenPrefix)
}

// generateToken draws tokenBytes from the system entropy source and renders
// them behind prefix.
func generateToken(prefix string) (string, error) {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("reading random bytes: %w", err)
	}
	return prefix + hex.EncodeToString(raw), nil
}

// HashToken returns the sha256 digest of the whole token, prefix included, in
// lower-case hex. This is what the token_hash columns hold: the database never
// stores a usable token, so a dump of it hands out no access.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
