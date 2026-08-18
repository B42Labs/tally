package main

import (
	"fmt"
	"os"

	"github.com/shopspring/decimal"
	"gopkg.in/yaml.v3"
)

// pricingFile is the pricing document as it is written. Prices are strings
// there, so every one of them reaches decimal.NewFromString exactly as it was
// typed: a YAML float would already have lost precision by the time this
// process saw it.
type pricingFile struct {
	Currency   string `yaml:"currency"`
	Dimensions []struct {
		Metric           string `yaml:"metric"`
		PricePerUnitHour string `yaml:"price_per_unit_hour"`
	} `yaml:"dimensions"`
	StateModifiers map[string]string `yaml:"state_modifiers"`
}

// dimension is one priced size metric: what the size object calls it, and what
// one unit of it costs per hour.
type dimension struct {
	Metric           string
	PricePerUnitHour decimal.Decimal
}

// pricing is the parsed pricing document. A state that names no modifier is
// billed at the full price, so the map holds the exceptions alone.
type pricing struct {
	Currency       string
	Dimensions     []dimension
	StateModifiers map[string]decimal.Decimal
}

// loadPricing reads and checks the pricing file at path. Every price is parsed
// here rather than while rating, so a typo in the file stops the run before the
// first request instead of surfacing as a resource that failed halfway through
// the output.
func loadPricing(path string) (pricing, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return pricing{}, fmt.Errorf("reading the pricing file %s: %w", path, err)
	}

	var file pricingFile
	if err := yaml.Unmarshal(data, &file); err != nil {
		return pricing{}, fmt.Errorf("parsing the pricing file %s: %w", path, err)
	}

	if len(file.Dimensions) == 0 {
		return pricing{}, fmt.Errorf("the pricing file %s lists no dimensions", path)
	}
	// The currency is what the document's amounts are denominated in, and it is
	// carried straight into the output: an absent one ships a total with no unit
	// on it, which is not a figure anybody can reconcile against an invoice.
	if file.Currency == "" {
		return pricing{}, fmt.Errorf("the pricing file %s names no currency", path)
	}

	parsed := pricing{
		Currency:       file.Currency,
		Dimensions:     make([]dimension, 0, len(file.Dimensions)),
		StateModifiers: make(map[string]decimal.Decimal, len(file.StateModifiers)),
	}

	seen := make(map[string]bool, len(file.Dimensions))
	for _, dim := range file.Dimensions {
		// Two entries for one metric would both be rated, so the metric would be
		// billed twice without anything in the output saying so.
		if seen[dim.Metric] {
			return pricing{}, fmt.Errorf("the pricing file %s prices the metric %q twice", path, dim.Metric)
		}
		seen[dim.Metric] = true

		price, err := decimal.NewFromString(dim.PricePerUnitHour)
		if err != nil {
			return pricing{}, fmt.Errorf("reading the price of the metric %q in %s: %w", dim.Metric, path, err)
		}
		// A stray minus parses cleanly and would credit the customer for usage.
		if price.IsNegative() {
			return pricing{}, fmt.Errorf("the price of the metric %q in %s is negative: %s", dim.Metric, path, price)
		}
		parsed.Dimensions = append(parsed.Dimensions, dimension{Metric: dim.Metric, PricePerUnitHour: price})
	}

	for state, modifier := range file.StateModifiers {
		factor, err := decimal.NewFromString(modifier)
		if err != nil {
			return pricing{}, fmt.Errorf("reading the modifier of the state %q in %s: %w", state, path, err)
		}
		if factor.IsNegative() {
			return pricing{}, fmt.Errorf("the modifier of the state %q in %s is negative: %s", state, path, factor)
		}
		parsed.StateModifiers[state] = factor
	}

	return parsed, nil
}
