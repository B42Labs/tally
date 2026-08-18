package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
)

// writePricing writes a pricing document and returns its path.
func writePricing(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "pricing.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// TestLoadPricingReadsThePrototype holds the loader to the file the slice is
// run with. The prices are the concept's section 3.4 numbers, and the golden
// output is derived from exactly these, so a changed file has to fail here
// rather than shift the rated total.
func TestLoadPricingReadsThePrototype(t *testing.T) {
	pr, err := loadPricing(filepath.Join("..", "..", "pricing", "prototype.yaml"))
	if err != nil {
		t.Fatalf("loadPricing() error = %v, want nil", err)
	}

	if pr.Currency != "EUR" {
		t.Errorf("Currency = %q, want %q", pr.Currency, "EUR")
	}

	wantPrices := []struct{ metric, price string }{
		{"vcpus", "0.02"},
		{"ram_gb", "0.005"},
		{"disk_gb", "0.001"},
	}
	if len(pr.Dimensions) != len(wantPrices) {
		t.Fatalf("Dimensions = %d entries, want %d", len(pr.Dimensions), len(wantPrices))
	}
	for i, want := range wantPrices {
		got := pr.Dimensions[i]
		if got.Metric != want.metric {
			t.Errorf("Dimensions[%d].Metric = %q, want %q", i, got.Metric, want.metric)
		}
		if !got.PricePerUnitHour.Equal(decimal.RequireFromString(want.price)) {
			t.Errorf("Dimensions[%d].PricePerUnitHour = %s, want %s", i, got.PricePerUnitHour, want.price)
		}
	}

	wantModifiers := map[string]string{"shelved": "0.0", "shutoff": "0.5"}
	if len(pr.StateModifiers) != len(wantModifiers) {
		t.Fatalf("StateModifiers = %v, want %d entries", pr.StateModifiers, len(wantModifiers))
	}
	for state, want := range wantModifiers {
		got, ok := pr.StateModifiers[state]
		if !ok {
			t.Errorf("StateModifiers has no %q", state)
			continue
		}
		if !got.Equal(decimal.RequireFromString(want)) {
			t.Errorf("StateModifiers[%q] = %s, want %s", state, got, want)
		}
	}
}

// TestLoadPricingRefusesABrokenFile checks that every way the file can be wrong
// stops the run before the first request, and names what is wrong with it. A
// price the loader let through would be discovered as a wrong invoice.
func TestLoadPricingRefusesABrokenFile(t *testing.T) {
	for name, tc := range map[string]struct {
		content string
		want    string
	}{
		"no dimensions": {
			content: "currency: EUR\ndimensions: []\n",
			want:    "lists no dimensions",
		},
		"a metric priced twice": {
			content: `currency: EUR
dimensions:
  - metric: vcpus
    price_per_unit_hour: "0.02"
  - metric: vcpus
    price_per_unit_hour: "0.03"
`,
			want: `"vcpus"`,
		},
		"a price that is not a number": {
			content: `currency: EUR
dimensions:
  - metric: ram_gb
    price_per_unit_hour: "abc"
`,
			want: `"ram_gb"`,
		},
		"a modifier that is not a number": {
			content: `currency: EUR
dimensions:
  - metric: vcpus
    price_per_unit_hour: "0.02"
state_modifiers:
  shutoff: "half"
`,
			want: `"shutoff"`,
		},
		"no currency": {
			content: `dimensions:
  - metric: vcpus
    price_per_unit_hour: "0.02"
`,
			want: "names no currency",
		},
		"a negative price": {
			content: `currency: EUR
dimensions:
  - metric: vcpus
    price_per_unit_hour: "-0.02"
`,
			want: "is negative",
		},
		"a negative modifier": {
			content: `currency: EUR
dimensions:
  - metric: vcpus
    price_per_unit_hour: "0.02"
state_modifiers:
  shutoff: "-0.5"
`,
			want: "is negative",
		},
		"a document that is not YAML": {
			content: "currency: EUR\ndimensions: [\n",
			want:    "parsing the pricing file",
		},
	} {
		t.Run(name, func(t *testing.T) {
			path := writePricing(t, tc.content)

			_, err := loadPricing(path)
			if err == nil {
				t.Fatalf("loadPricing() error = nil, want an error naming %s", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("loadPricing() error = %v, want it to name %s", err, tc.want)
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("loadPricing() error = %v, want it to name the file %s", err, path)
			}
		})
	}
}

// TestLoadPricingReportsAMissingFile keeps the read error wrapped rather than
// replaced, so a caller can still tell a missing file from an unreadable one.
func TestLoadPricingReportsAMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.yaml")

	_, err := loadPricing(path)
	if err == nil {
		t.Fatalf("loadPricing() error = nil, want an error")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("loadPricing() error = %v, want it to wrap fs.ErrNotExist", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("loadPricing() error = %v, want it to name the file %s", err, path)
	}
}
