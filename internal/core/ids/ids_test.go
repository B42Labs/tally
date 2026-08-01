package ids_test

import (
	"strings"
	"testing"
	"time"

	"github.com/b42labs/tally/internal/core/ids"
)

// The golden values below pin the hashing scheme. Changing either function
// changes every id it ever produced, so a diff here is a migration, not a fix.
const (
	// sha256("os-prod-eu1:i-1:compute.instance.create.end:2026-03-01T12:00:00Z")
	goldenDeterministicID = "openstack-56e3f54aefd66cdc729b86ddc7ad894b8010095a2edbde2881319b1de7522d4f"
	// sha256("run-1:os-prod-eu1:instance:i-1:delete")
	goldenSyntheticID = "recon-509cb64132d88d07e1f98c046807f1c26396b0b338245e15bc5349cfd0acbc0b"
)

func TestDeterministicEventIDIsGolden(t *testing.T) {
	ts := time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)

	got := ids.DeterministicEventID("openstack", "os-prod-eu1", "i-1", "compute.instance.create.end", ts)
	if got != goldenDeterministicID {
		t.Errorf("DeterministicEventID() = %q, want %q", got, goldenDeterministicID)
	}
}

func TestDeterministicEventIDShape(t *testing.T) {
	ts := time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)

	got := ids.DeterministicEventID("openstack", "os-prod-eu1", "i-1", "compute.instance.create.end", ts)

	prefix, hash, found := strings.Cut(got, "-")
	if !found {
		t.Fatalf("DeterministicEventID() = %q, want a platform prefix", got)
	}
	if prefix != "openstack" {
		t.Errorf("prefix = %q, want %q", prefix, "openstack")
	}
	if len(hash) != 64 {
		t.Errorf("hash length = %d, want 64", len(hash))
	}
}

func TestDeterministicEventIDNormalizesTimezone(t *testing.T) {
	// The same instant written in two zones is one event, not two.
	utc := time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)
	berlin := utc.In(time.FixedZone("CEST", 2*60*60))

	inUTC := ids.DeterministicEventID("openstack", "os-prod-eu1", "i-1", "compute.instance.create.end", utc)
	inBerlin := ids.DeterministicEventID("openstack", "os-prod-eu1", "i-1", "compute.instance.create.end", berlin)

	if inUTC != inBerlin {
		t.Errorf("id differs by timezone: %q (UTC) vs %q (+02:00)", inUTC, inBerlin)
	}
}

func TestDeterministicEventIDIsStableAcrossCalls(t *testing.T) {
	ts := time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)

	first := ids.DeterministicEventID("openstack", "os-prod-eu1", "i-1", "compute.instance.create.end", ts)
	second := ids.DeterministicEventID("openstack", "os-prod-eu1", "i-1", "compute.instance.create.end", ts)

	if first != second {
		t.Errorf("id is not stable: %q then %q", first, second)
	}
}

func TestSyntheticEventIDIsGolden(t *testing.T) {
	got := ids.SyntheticEventID("run-1", "os-prod-eu1", "instance", "i-1", "delete")
	if got != goldenSyntheticID {
		t.Errorf("SyntheticEventID() = %q, want %q", got, goldenSyntheticID)
	}
}

func TestSyntheticEventIDShape(t *testing.T) {
	got := ids.SyntheticEventID("run-1", "os-prod-eu1", "instance", "i-1", "delete")

	hash, found := strings.CutPrefix(got, "recon-")
	if !found {
		t.Fatalf("SyntheticEventID() = %q, want a %q prefix", got, "recon-")
	}
	if len(hash) != 64 {
		t.Errorf("hash length = %d, want 64", len(hash))
	}
}

func TestSyntheticEventIDSeparatesKinds(t *testing.T) {
	// A missed create and a missed delete for one resource in one run are two
	// distinct corrections, so they must not collide.
	create := ids.SyntheticEventID("run-1", "os-prod-eu1", "instance", "i-1", "create")
	del := ids.SyntheticEventID("run-1", "os-prod-eu1", "instance", "i-1", "delete")

	if create == del {
		t.Errorf("ids collide across kinds: %q", create)
	}
}

func TestIDsAcceptEmptyInput(t *testing.T) {
	// Empty strings and the zero time are degenerate but must not panic: they
	// reach these functions from providers with incomplete notifications.
	deterministic := ids.DeterministicEventID("", "", "", "", time.Time{})
	if _, hash, _ := strings.Cut(deterministic, "-"); len(hash) != 64 {
		t.Errorf("DeterministicEventID() = %q, want a 64 character hash", deterministic)
	}

	synthetic := ids.SyntheticEventID("", "", "", "", "")
	if hash, _ := strings.CutPrefix(synthetic, "recon-"); len(hash) != 64 {
		t.Errorf("SyntheticEventID() = %q, want a 64 character hash", synthetic)
	}
}
