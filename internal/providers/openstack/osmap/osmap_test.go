package osmap

import "testing"

// TestVMStateNormalizesWhatNovaReports covers the table both readers of a cloud
// share. A state the table has no entry for is what the cloud reported, because
// substituting something known for an unknown one would hide it.
func TestVMStateNormalizesWhatNovaReports(t *testing.T) {
	tests := []struct {
		name     string
		reported string
		want     string
	}{
		{name: "an active instance stays active", reported: "active", want: "active"},
		{name: "a stopped instance is shut off", reported: "stopped", want: "shutoff"},
		{
			name:     "an offloaded instance is shelved",
			reported: "shelved_offloaded",
			want:     "shelved",
		},
		{name: "a state the table has no entry for passes through", reported: "rescued", want: "rescued"},
		{name: "an absent state stays absent", reported: "", want: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := VMState(tc.reported); got != tc.want {
				t.Errorf("VMState(%q) = %q, want %q", tc.reported, got, tc.want)
			}
		})
	}
}

// TestIPVersionReadsTheAddress covers the one billable property of a floating
// IP. An address that is absent or unreadable counts as IPv4 and comes back
// with the parse error, so a caller that wants to report it can.
func TestIPVersionReadsTheAddress(t *testing.T) {
	tests := []struct {
		name       string
		address    string
		want       int
		wantUnread bool
	}{
		{name: "an IPv4 address is version 4", address: "203.0.113.42", want: 4},
		{name: "an IPv6 address is version 6", address: "2001:db8::1", want: 6},
		{name: "an absent address falls back to version 4", address: "", want: 4, wantUnread: true},
		{
			name:       "an unreadable address falls back to version 4",
			address:    "no-address-at-all",
			want:       4,
			wantUnread: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := IPVersion(tc.address)
			if got != tc.want {
				t.Errorf("IPVersion(%q) = %d, want %d", tc.address, got, tc.want)
			}
			if unread := err != nil; unread != tc.wantUnread {
				t.Errorf("IPVersion(%q) err = %v, want an error: %t", tc.address, err, tc.wantUnread)
			}
		})
	}
}
