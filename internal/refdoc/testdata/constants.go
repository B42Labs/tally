package testdata

// Source names the pipeline an event came from.
type Source string

// The problem types the fixture API answers with. A client matches on these
// URNs rather than on the status code.
const (
	// TypeValidation marks a request the contract rejects.
	TypeValidation = "urn:tally:test:validation"
	// TypeInternal marks a failure the caller cannot do anything about.
	TypeInternal = "urn:tally:test:internal"
	// SourceCollector marks an event a provider-side collector pushed.
	SourceCollector Source = "collector"
)

// identifierMaxLen bounds the fields that identify a resource. They are indexed
// columns, and a value past the btree limit fails the insert.
const identifierMaxLen = 512

// The counters are declared with iota, so only the first carries a value.
const (
	counterFirst = iota
	counterSecond
)

// notAConstant is declared as a var, which is what a page must not render as a
// constant.
var notAConstant = "value"
