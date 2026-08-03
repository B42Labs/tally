// Package reporting carries the goose migrations that define the Reporting
// API's database schema. They are embedded rather than read from disk so that
// the binary and the schema it expects travel as one artifact: a container
// image cannot drift from the migration directory it was built against.
package reporting

import "embed"

// FS holds the migration files of this package. Goose applies them in the order
// given by the numeric prefix of each file name.
//
//go:embed *.sql
var FS embed.FS

// Version is the highest version this package embeds, and so the schema a
// binary built from this tree expects. Readiness compares it against what the
// database carries, which is what keeps a pod from serving traffic against a
// schema its code is newer than.
//
// Adding a migration means raising this; a test fails otherwise.
const Version int64 = 1
