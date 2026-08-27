// Package adjustment holds the pricing adjustments a project relation carries
// to the rules they have to match: the surcharges, discounts, project discounts
// and kickbacks a rated amount passes through. They live in the
// "pricing_adjustments" member of a relation's metadata, and this package holds
// the JSON Schema that member is held to.
//
// It is the one place the Reporting API reads pricing_adjustments from, so the
// API spells neither the member name nor the schema out again. The API holds a
// written relation to the schema and answers 422 for a document it refuses
// (decision D2).
//
// The package does no I/O.
//
// The normative specification is roadmap/05-phase-5-commercial-pricing.md,
// WP 5.2.
package adjustment

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// MetadataKey is the member of a relation's metadata that carries the
// adjustments. A relation whose metadata does not hold it adjusts nothing.
const MetadataKey = "pricing_adjustments"

// MaxAdjustments is how many adjustments one relation may carry. The array
// arrives from an HTTP request body, so its length has to be bounded somewhere:
// without a bound, a body of nothing but elements the schema refuses costs one
// violation per element, and everything reading them afterwards grows with it.
// The cap sits here rather than as a maxItems beside the items of the schema
// because both keywords are evaluated: the walk would report every element of
// an array past the cap before the length was ever named.
const MaxAdjustments = 64

// Violation is one place the schema refused, located inside the array.
type Violation struct {
	Location string // the JSON pointer below the array; empty for the array itself
	Message  string // the validator's own sentence, for example "got number, want string"
}

// InvalidError is what Validate returns for a document the schema refuses. It
// carries every violation the validator found, so a caller answering 422 names
// all of them at once instead of one per round trip.
type InvalidError struct {
	Violations []Violation
}

// Error names every violation with the place it sits in, so the element that
// has to change can be read off the message.
func (e *InvalidError) Error() string {
	places := make([]string, 0, len(e.Violations))
	for _, violation := range e.Violations {
		places = append(places, MetadataKey+violation.Location+": "+violation.Message)
	}
	return "the pricing adjustments do not match the schema: " + strings.Join(places, "; ")
}

// schemaDocument is the JSON Schema every pricing_adjustments member is checked
// against. It is embedded rather than read from disk so that a binary and the
// rules it enforces travel as one artifact.
//
//go:embed adjustments_schema.json
var schemaDocument []byte

// schemaURL is the location the embedded schema is compiled under. Nothing
// loads it: it only gives the document an identity. The scheme is one no loader
// serves, so a $ref that resolves against it fails to compile instead of
// reaching for a file on the importing host's filesystem.
const schemaURL = "tally:pricing-adjustments"

// schema compiles the embedded document once. Compiling costs more than
// validating against the result, and the document is the same for the life of
// the process. A compile error is the package's own bug and reaches every
// caller of Validate.
var schema = sync.OnceValues(func() (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaDocument))
	if err != nil {
		return nil, fmt.Errorf("decoding the adjustments schema: %w", err)
	}

	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	if err := compiler.AddResource(schemaURL, doc); err != nil {
		return nil, fmt.Errorf("adding the adjustments schema: %w", err)
	}
	compiled, err := compiler.Compile(schemaURL)
	if err != nil {
		return nil, fmt.Errorf("compiling the adjustments schema: %w", err)
	}
	return compiled, nil
})

// Validate holds the JSON text of the adjustments array alone to the schema. A
// document that comes back without an error carries at most MaxAdjustments
// elements, and nothing but elements with a type the enum names, a rate between
// 0 and 1 and a scope the grammar of decision D5 admits. A document the schema
// refuses comes back as an *InvalidError; text that is not JSON at all comes
// back as a decoding error.
func Validate(doc []byte) error {
	compiled, err := schema()
	if err != nil {
		return err
	}

	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		return fmt.Errorf("decoding the pricing adjustments: %w", err)
	}
	// The length is read before the schema walks the array, so an array past
	// the cap is one violation naming the length rather than one per element.
	if items, ok := value.([]any); ok && len(items) > MaxAdjustments {
		return &InvalidError{Violations: []Violation{{
			Message: fmt.Sprintf("maxItems: got %d, want %d", len(items), MaxAdjustments),
		}}}
	}
	if err := compiled.Validate(value); err != nil {
		var invalid *jsonschema.ValidationError
		if errors.As(err, &invalid) {
			return invalidFrom(invalid)
		}
		return fmt.Errorf("validating the pricing adjustments: %w", err)
	}
	return nil
}

// ValidateMetadata holds the adjustments of a whole relation metadata document
// to the schema. Metadata without the member is not an error: the relation
// adjusts nothing, and the caller writes it as it stands. A member that is
// there is validated, so a caller that has to refuse a malformed one gets the
// *InvalidError.
func ValidateMetadata(metadata []byte) error {
	if len(metadata) == 0 {
		return nil
	}

	var members map[string]json.RawMessage
	if err := json.Unmarshal(metadata, &members); err != nil {
		return fmt.Errorf("decoding the relation metadata: %w", err)
	}

	member, ok := members[MetadataKey]
	if !ok {
		return nil
	}
	return Validate(member)
}

// invalidFrom turns the validator's error into the violations a caller reports.
// The detailed output nests one unit per keyword that refused, and only the
// leaves name a defect: a unit with children says no more than that something
// below it is wrong. The walk therefore descends and emits the leaves in the
// order the validator produced them, whatever depth they sit at.
func invalidFrom(err *jsonschema.ValidationError) *InvalidError {
	return &InvalidError{Violations: appendLeaves(nil, err.DetailedOutput())}
}

// appendLeaves appends the leaves of one output unit to the violations.
func appendLeaves(violations []Violation, unit *jsonschema.OutputUnit) []Violation {
	if len(unit.Errors) == 0 {
		return append(violations, Violation{
			Location: unit.InstanceLocation,
			Message:  unit.Error.String(),
		})
	}
	for i := range unit.Errors {
		violations = appendLeaves(violations, &unit.Errors[i])
	}
	return violations
}
