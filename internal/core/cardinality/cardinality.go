// Package cardinality bounds the values a metric label is recorded under, which
// is what keeps a label whose vocabulary the process does not decide from
// minting series for as long as it runs.
//
// A vector keeps every child it is ever asked for for the life of the process,
// so a label whose values arrive off the wire turns into unbounded process
// memory and an unbounded scrape response without a bound of its own. Values
// longer than ValueMax, and values past the limit-th distinct one of their
// label, are recorded under Overflow instead.
//
// The limit is per label and admits on a first-seen basis, so a deployment
// staying inside it sees its real values throughout, and a caller flooding the
// limit with noise costs the remaining series their detail rather than the
// process its memory. Overflow is a value a caller may legitimately carry as
// well; the two then share a series, which is the price of not keeping a
// second, unbounded vocabulary to tell them apart.
package cardinality

import "sync"

const (
	// ValueMax is the longest value recorded as itself. Anything longer is
	// recorded as Overflow whatever the label has room for.
	ValueMax = 128
	// Overflow is the value everything the limit does not admit is counted under.
	Overflow = "other"
)

// Limiter is the vocabulary the bounded labels have been recorded under. It is
// safe for concurrent use: callers record from whatever goroutine handles the
// work the value came with.
type Limiter struct {
	limit    int
	mu       sync.Mutex
	admitted map[labelValue]struct{}
	distinct map[string]int
}

// labelValue is one value of one label, which is what a Limiter admits.
type labelValue struct{ label, value string }

// New builds a limiter that admits limit distinct values per label.
func New(limit int) *Limiter {
	return &Limiter{limit: limit, admitted: map[labelValue]struct{}{}, distinct: map[string]int{}}
}

// Bound returns what value may be recorded as under label: value itself while
// the label has room for it, and Overflow once it has not.
func (l *Limiter) Bound(label, value string) string {
	if len(value) > ValueMax {
		return Overflow
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	admitted := labelValue{label: label, value: value}
	if _, ok := l.admitted[admitted]; ok {
		return value
	}
	if l.distinct[label] >= l.limit {
		return Overflow
	}
	l.admitted[admitted] = struct{}{}
	l.distinct[label]++
	return value
}
