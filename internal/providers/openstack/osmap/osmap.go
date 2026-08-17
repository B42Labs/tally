// Package osmap holds the vocabulary every OpenStack reader in Tally
// normalizes with: the divisors the reported quantities are converted by, the
// nova vm_state table, and which protocol a floating IP is an address of.
//
// The collector (internal/providers/openstack) and the reconciliation adapter
// (internal/reporting/reconciliation/adapters) both read it. The two have to
// say the same thing: a sync that normalized a state or a quantity differently
// from the collector would report drift on every resource the collector had
// already booked correctly. Saying it once is what keeps them from drifting
// apart unnoticed.
//
// It is a package of its own, and one that imports nothing but decimal
// arithmetic, because the adapter runs inside the Reporting API: importing the
// collector's own package would pull its AMQP consumer and its SQLite outbox
// into that binary.
package osmap

import (
	"net/netip"

	"github.com/shopspring/decimal"
)

// MebibytesPerGibibyte and BytesPerGibibyte are the divisors the reported
// quantities are converted with. Nova reports memory in MiB and glance reports
// image sizes in bytes, while the canonical size objects are in GiB.
var (
	MebibytesPerGibibyte = decimal.NewFromInt(1024)
	BytesPerGibibyte     = decimal.NewFromInt(1 << 30)
)

// vmStates maps a nova vm_state to the state Tally records.
var vmStates = map[string]string{
	"active":            "active",
	"stopped":           "shutoff",
	"shelved_offloaded": "shelved",
	"paused":            "paused",
	"suspended":         "suspended",
	"error":             "error",
}

// VMState normalizes the vm_state nova reported. A state outside the table
// passes through unchanged, because a state is an opaque provider string and
// substituting something known for an unknown one would hide what the cloud
// actually reported.
func VMState(reported string) string {
	if state, ok := vmStates[reported]; ok {
		return state
	}
	return reported
}

// IPVersion reports which protocol a floating IP is an address of. An address
// that is absent or unreadable counts as IPv4, since that is what a deployment
// allocates unless it says otherwise, and it comes back together with the parse
// error, so a caller that wants to report the address it could not read can.
func IPVersion(address string) (int, error) {
	parsed, err := netip.ParseAddr(address)
	switch {
	case err != nil:
		return 4, err
	case parsed.Is4():
		return 4, nil
	default:
		return 6, nil
	}
}
