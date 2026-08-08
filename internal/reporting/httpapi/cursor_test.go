package httpapi

import (
	"encoding/base64"
	"slices"
	"testing"
)

// TestCursorRoundTrip pins the property the paginated lists stand on: the cursor
// handed out for the last row of a page comes back as exactly the key parts the
// next query resumes from, whatever the sort key is made of.
func TestCursorRoundTrip(t *testing.T) {
	tests := map[string][]string{
		"one key": {"2026-03-01T00:00:00Z"},
		"two keys": {
			"2026-03-01T00:00:00Z",
			"9a0e6b2c-4d1f-4f7a-8b3e-2c5d6e7f8a90",
		},
		"three keys": {
			"openstack",
			"instance",
			"9a0e6b2c-4d1f-4f7a-8b3e-2c5d6e7f8a90",
		},
	}

	for name, keys := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := decodeCursor(encodeCursor(keys), len(keys))
			if err != nil {
				t.Fatalf("decodeCursor() error = %v, want nil", err)
			}
			if !slices.Equal(got, keys) {
				t.Errorf("keys = %q, want %q", got, keys)
			}
		})
	}
}

// TestCursorRefusesAMalformedCursor walks the ways a cursor can arrive broken. A
// client types this value, so each of them ends in an error rather than in a key
// set a query would be run with.
func TestCursorRefusesAMalformedCursor(t *testing.T) {
	tests := map[string]struct {
		cursor string
		want   int
	}{
		"not base64url at all": {
			cursor: "not base64!",
			want:   1,
		},
		"base64url over bytes that are not JSON": {
			cursor: base64.RawURLEncoding.EncodeToString([]byte("{oops")),
			want:   1,
		},
		"a JSON object rather than an array": {
			cursor: base64.RawURLEncoding.EncodeToString([]byte(`{"after":"2026-03-01T00:00:00Z"}`)),
			want:   1,
		},
		"a JSON array of numbers": {
			cursor: base64.RawURLEncoding.EncodeToString([]byte(`[1,2]`)),
			want:   2,
		},
		"more keys than the list orders by": {
			cursor: encodeCursor([]string{"2026-03-01T00:00:00Z", "9a0e6b2c-4d1f-4f7a-8b3e-2c5d6e7f8a90"}),
			want:   1,
		},
		"an empty key": {
			cursor: encodeCursor([]string{"2026-03-01T00:00:00Z", ""}),
			want:   2,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := decodeCursor(tc.cursor, tc.want)
			if err == nil {
				t.Fatalf("decodeCursor(%q, %d) error = nil, want an error (keys %q)",
					tc.cursor, tc.want, got)
			}
		})
	}
}
