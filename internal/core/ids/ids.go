// Package ids derives event identifiers. Both functions are pure and stable
// across processes and releases: the same inputs always produce the same id, so
// re-delivering a notification or re-running a sync is a no-op at ingestion
// rather than a duplicate.
//
// The normative specification is roadmap/00-conventions.md section 4.
package ids

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// DeterministicEventID derives an event id for a provider that exposes no native
// event or action id of its own. The timestamp is normalized to UTC first, so
// the same instant expressed in different zones yields one id.
func DeterministicEventID(platform, cloud, resourceID, eventType string, ts time.Time) string {
	raw := cloud + ":" + resourceID + ":" + eventType + ":" + ts.UTC().Format(time.RFC3339Nano)
	sum := sha256.Sum256([]byte(raw))
	return platform + "-" + hex.EncodeToString(sum[:])
}

// SyntheticEventID derives the event id of a correction emitted by a sync run.
// It is deterministic per run, which makes re-applying that run a no-op.
func SyntheticEventID(syncRunID, cloud, resourceType, resourceID, kind string) string {
	raw := syncRunID + ":" + cloud + ":" + resourceType + ":" + resourceID + ":" + kind
	sum := sha256.Sum256([]byte(raw))
	return "recon-" + hex.EncodeToString(sum[:])
}
