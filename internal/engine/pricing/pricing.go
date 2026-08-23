// Package pricing reads a pricing model: the versioned document that says what
// a unit of every priced metric costs, per platform and resource type. It takes
// the YAML file an operator writes, checks it against the JSON Schema embedded
// here, and returns the typed model the rating pass multiplies usage by.
//
// A file becomes a canonical JSON document on the way in, and that document is
// what a version is stored from. Parse hands it back beside the model and
// ParseDocument reads it again, so a model that came from a file and one that
// came back from the database go through one code path: a stored version is
// held to the schema its file was held to. Every scalar keeps the literal text
// the file spells it with and every mapping key is written in sorted order, so
// the same file yields the same bytes however often it is imported. What the
// database holds is JSONB, which Postgres parses on the way in and writes back
// with its own key order and number formatting, so a stored version is never
// the bytes Parse returned: a re-import is compared as a parsed model rather
// than byte for byte.
//
// Prices never touch float64. A scalar reaches decimal.NewFromString as text,
// whether the file spells a price as a number or as a string, because a price
// that went through a float would carry its rounding error into every amount
// rated from it. The forbidigo rules in .golangci.yml keep it that way.
//
// The normative specification is roadmap/03-phase-3-metering-rating.md, WP 3.5.
package pricing

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/shopspring/decimal"
)

const (
	// TypeTimeGauge is billed over the time a quantity was held, so the
	// dimension carries a price per unit and hour.
	TypeTimeGauge = "time_gauge"
	// TypeCounter is billed over a quantity the counters pass measured, so the
	// dimension carries a price per unit.
	TypeCounter = "counter"
)

// Model is one version of the pricing model. Pricing is keyed by platform and
// then by resource type; a resource type the map does not hold is not priced.
type Model struct {
	Version   string
	ValidFrom time.Time
	Currency  string
	Pricing   map[string]map[string]ResourcePricing
}

// ResourcePricing is what one resource type of one platform costs. Dimensions
// are in the order the file lists them, which is the order the rated records of
// a resource are written in. The modifier maps are empty where the file sets
// none, and a state or type the map does not hold is billed unmodified.
type ResourcePricing struct {
	Dimensions     []Dimension
	StateModifiers map[string]decimal.Decimal
	TypeModifiers  map[string]decimal.Decimal
}

// Dimension is one priced metric of a resource type. Type decides which of the
// two prices carries the value: PricePerUnitHour for TypeTimeGauge,
// PricePerUnit for TypeCounter. The other one is zero.
type Dimension struct {
	Metric           string
	Type             string
	PricePerUnitHour decimal.Decimal
	PricePerUnit     decimal.Decimal
}

// Equal reports whether both models price the same thing. Decimals are compared
// by value, so a price the file respells as 0.50 rather than "0.5" leaves the
// model equal, which is what a re-import of an unchanged version has to be. The
// dimensions of a resource type are compared in order: reordering them reorders
// the rated records they produce, so it is a different model.
func (m Model) Equal(other Model) bool {
	if m.Version != other.Version || m.Currency != other.Currency || !m.ValidFrom.Equal(other.ValidFrom) {
		return false
	}
	if len(m.Pricing) != len(other.Pricing) {
		return false
	}

	for platform, types := range m.Pricing {
		otherTypes, ok := other.Pricing[platform]
		if !ok || len(types) != len(otherTypes) {
			return false
		}
		for resourceType, entry := range types {
			otherEntry, ok := otherTypes[resourceType]
			if !ok || !entry.equal(otherEntry) {
				return false
			}
		}
	}
	return true
}

// equal reports whether both entries price a resource type the same way.
func (r ResourcePricing) equal(other ResourcePricing) bool {
	if len(r.Dimensions) != len(other.Dimensions) {
		return false
	}
	for i, d := range r.Dimensions {
		o := other.Dimensions[i]
		if d.Metric != o.Metric || d.Type != o.Type ||
			!d.PricePerUnitHour.Equal(o.PricePerUnitHour) || !d.PricePerUnit.Equal(o.PricePerUnit) {
			return false
		}
	}
	return equalModifiers(r.StateModifiers, other.StateModifiers) &&
		equalModifiers(r.TypeModifiers, other.TypeModifiers)
}

// equalModifiers compares two modifier maps by value. A nil map and an empty one
// are the same thing: neither modifies anything.
func equalModifiers(a, b map[string]decimal.Decimal) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		other, ok := b[key]
		if !ok || !value.Equal(other) {
			return false
		}
	}
	return true
}

// schemaDocument is the JSON Schema every pricing model is checked against. It
// is embedded rather than read from disk so that a binary and the rules it
// enforces travel as one artifact.
//
//go:embed pricing.schema.json
var schemaDocument []byte

// schemaURL is the location the embedded schema is compiled under. Nothing
// loads it: it only gives the document an identity. The scheme is one no loader
// serves, so a $ref that resolves against it fails to compile instead of
// reaching for a file on the importing host's filesystem.
const schemaURL = "tally:pricing-model"

// schema compiles the embedded document once. Compiling costs more than
// validating against the result, and the document is the same for the life of
// the process. A compile error is the package's own bug and reaches every
// caller of Parse and ParseDocument.
var schema = sync.OnceValues(func() (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaDocument))
	if err != nil {
		return nil, fmt.Errorf("decoding the pricing schema: %w", err)
	}

	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	if err := compiler.AddResource(schemaURL, doc); err != nil {
		return nil, fmt.Errorf("adding the pricing schema: %w", err)
	}
	compiled, err := compiler.Compile(schemaURL)
	if err != nil {
		return nil, fmt.Errorf("compiling the pricing schema: %w", err)
	}
	return compiled, nil
})

// modelFile is a pricing model as the canonical document spells it.
type modelFile struct {
	Version   string                             `json:"version"`
	ValidFrom string                             `json:"valid_from"`
	Currency  string                             `json:"currency"`
	Pricing   map[string]map[string]resourceFile `json:"pricing"`
}

// resourceFile is one resource type of that document.
type resourceFile struct {
	Dimensions     []dimensionFile            `json:"dimensions"`
	StateModifiers map[string]decimal.Decimal `json:"state_modifiers"`
	TypeModifiers  map[string]decimal.Decimal `json:"type_modifiers"`
}

// dimensionFile is one dimension of that document. The price the dimension's
// type does not carry is absent from the document and stays zero here.
type dimensionFile struct {
	Metric           string          `json:"metric"`
	Type             string          `json:"type"`
	PricePerUnitHour decimal.Decimal `json:"price_per_unit_hour"`
	PricePerUnit     decimal.Decimal `json:"price_per_unit"`
}

// ParseDocument reads a canonical JSON document into a Model. It is the path a
// document read back from the database takes, and the path Parse takes once it
// has turned a file into one.
//
// The document is checked against the embedded schema first, so what the model
// is built from is a document the schema accepts. Two rules the schema cannot
// state are checked afterwards: valid_from has to be an RFC 3339 timestamp, and
// no resource type may price one metric twice.
func ParseDocument(doc []byte) (Model, error) {
	compiled, err := schema()
	if err != nil {
		return Model{}, err
	}

	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		return Model{}, fmt.Errorf("decoding the pricing model: %w", err)
	}
	if err := compiled.Validate(value); err != nil {
		return Model{}, fmt.Errorf("validating the pricing model: %w", err)
	}

	var file modelFile
	if err := json.Unmarshal(doc, &file); err != nil {
		return Model{}, fmt.Errorf("reading the pricing model: %w", err)
	}

	// The schema only says valid_from is a string: a model that names no
	// instant would otherwise be stored and then never selected for a period.
	validFrom, err := time.Parse(time.RFC3339, file.ValidFrom)
	if err != nil {
		return Model{}, fmt.Errorf("reading valid_from %q: %w", file.ValidFrom, err)
	}

	pricing := make(map[string]map[string]ResourcePricing, len(file.Pricing))
	for platform, types := range file.Pricing {
		byType := make(map[string]ResourcePricing, len(types))
		for resourceType, entry := range types {
			dimensions := make([]Dimension, 0, len(entry.Dimensions))
			priced := make(map[string]bool, len(entry.Dimensions))

			for _, d := range entry.Dimensions {
				// Every dimension is rated on its own, so a metric priced
				// twice is billed twice under one name.
				if priced[d.Metric] {
					return Model{}, fmt.Errorf(
						"%s/%s prices the metric %s twice, and every dimension is rated on its own",
						platform, resourceType, d.Metric)
				}
				priced[d.Metric] = true

				dimensions = append(dimensions, Dimension(d))
			}

			byType[resourceType] = ResourcePricing{
				Dimensions:     dimensions,
				StateModifiers: entry.StateModifiers,
				TypeModifiers:  entry.TypeModifiers,
			}
		}
		pricing[platform] = byType
	}

	return Model{
		Version: file.Version,
		// Stored and compared as UTC, so that a version whose file wrote the
		// offset out selects the same periods as one that wrote Z.
		ValidFrom: validFrom.UTC(),
		Currency:  file.Currency,
		Pricing:   pricing,
	}, nil
}
