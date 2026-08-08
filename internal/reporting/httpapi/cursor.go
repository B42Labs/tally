package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// encodeCursor packs the sort key of the last item on a page into the cursor a
// client sends back to ask for the one after it (roadmap/00-conventions.md
// section 7).
//
// A cursor carries position and nothing else: the parts of the sort key, in the
// order the list is ordered by, as a JSON array of strings, base64url-encoded
// without padding so it travels in a query string as it is. The encoding is what
// makes it opaque, not a secret, so a client has no reason to read it and no
// promise that the next release spells it the same way.
func encodeCursor(keys []string) string {
	// Marshalling a slice of strings cannot fail, so there is no error to carry
	// out of here and no caller that would have to handle one.
	payload, _ := json.Marshal(keys)
	return base64.RawURLEncoding.EncodeToString(payload)
}

// decodeCursor reads a cursor encodeCursor wrote and returns the want key parts
// the query resumes from.
//
// The cursor arrives from a client, so every step back through it is checked:
// only a base64url payload holding a JSON array of exactly want non-empty
// strings gets through, and anything else is refused instead of being handed to
// a query. The element count is part of that because a cursor from a list with
// another sort key would otherwise resume a query with the wrong number of
// bounds.
//
// Callers answer every refusal with the same 400, so the errors here are for the
// log and say which part of the cursor was wrong.
func decodeCursor(cursor string, want int) ([]string, error) {
	payload, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, fmt.Errorf("the cursor is not base64url: %w", err)
	}

	var keys []string
	if err := json.Unmarshal(payload, &keys); err != nil {
		return nil, fmt.Errorf("the cursor does not hold an array of strings: %w", err)
	}
	if len(keys) != want {
		return nil, fmt.Errorf("the cursor holds %d keys, want %d", len(keys), want)
	}
	for i, key := range keys {
		if key == "" {
			return nil, fmt.Errorf("key %d of the cursor is empty", i)
		}
	}
	return keys, nil
}
