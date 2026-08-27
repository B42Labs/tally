// Package project names the registry conventions of decision D1
// (roadmap/05-phase-5-commercial-pricing.md). A meta-project (platform "meta")
// and a partner (platform "partner") are virtual projects: ordinary projects
// rows that own no resources and carry their platform as their cloud. The
// relation types "member_of" and "managed_by" are the ones that reach them, and
// neither ever attributes cost.
//
// The package does no I/O. It is the one place the Reporting API, the engine and
// the CLI read these literals from, so none of them spells them out again.
package project

const (
	// PlatformMeta is the platform of a meta-project, the virtual project that
	// groups real projects under one customer.
	PlatformMeta = "meta"
	// PlatformPartner is the platform of a partner, the virtual project a reseller
	// relation points at.
	PlatformPartner = "partner"
	// RelationMemberOf points a project at the meta-project it belongs to.
	RelationMemberOf = "member_of"
	// RelationManagedBy points a project at the partner that manages it.
	RelationManagedBy = "managed_by"
)

// IsVirtualPlatform reports whether the platform is one whose projects own no
// resources. The comparison is exact and case-sensitive, matching the literals
// the registry stores.
func IsVirtualPlatform(platform string) bool {
	return platform == PlatformMeta || platform == PlatformPartner
}

// IsVirtualRelationType reports whether the relation type is one that reaches a
// virtual project; none of them attributes cost. The comparison is exact and
// case-sensitive, matching the literals the registry stores.
func IsVirtualRelationType(relationType string) bool {
	return relationType == RelationMemberOf || relationType == RelationManagedBy
}
