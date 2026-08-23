package pricing_test

import (
	"bytes"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/engine/pricing"
)

// examplePath is the model the concept gives as its example, committed at the
// repository root. Parsing it here is what keeps the file and the schema from
// drifting apart.
const examplePath = "../../../pricing/2026-03.yaml"

// oneDimension is the smallest model that prices anything. The spelling tests
// vary the one price it carries.
const oneDimension = `version: "2026-03"
valid_from: "2026-03-01T00:00:00Z"
currency: "EUR"
pricing:
  openstack:
    instance:
      dimensions:
        - metric: "vcpus"
          type: "time_gauge"
          price_per_unit_hour: %s
`

// twoDimensions is the model the equality test varies: one dimension of each
// type and a modifier, so that a reordering has something to reorder.
const twoDimensions = `version: "2026-03"
valid_from: "2026-03-01T00:00:00Z"
currency: "EUR"
pricing:
  openstack:
    instance:
      dimensions:
        - metric: "vcpus"
          type: "time_gauge"
          price_per_unit_hour: "0.02"
        - metric: "egress_gb"
          type: "counter"
          price_per_unit: "0.09"
      state_modifiers:
        shutoff: "0.50"
`

// readFile reads a file the test parses. It takes a testing.TB so that the fuzz
// target can seed itself from the committed example the table tests parse.
func readFile(t testing.TB, path string) []byte {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return data
}

// dec builds the decimal a price is expected to carry. The tests construct
// every price the way the parser does, from text.
func dec(t *testing.T, text string) decimal.Decimal {
	t.Helper()

	value, err := decimal.NewFromString(text)
	if err != nil {
		t.Fatalf("decimal.NewFromString(%q): %v", text, err)
	}
	return value
}

// mustParse parses a model the test expects to hold.
func mustParse(t *testing.T, document string) pricing.Model {
	t.Helper()

	model, _, err := pricing.Parse([]byte(document))
	if err != nil {
		t.Fatalf("Parse() error = %v, want nil", err)
	}
	return model
}

func TestParseExampleModel(t *testing.T) {
	model, doc, err := pricing.Parse(readFile(t, examplePath))
	if err != nil {
		t.Fatalf("Parse() error = %v, want nil", err)
	}
	if len(doc) == 0 {
		t.Error("Parse() document is empty, want the canonical document")
	}

	if model.Version != "2026-03" {
		t.Errorf("Version = %q, want %q", model.Version, "2026-03")
	}
	if want := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC); !model.ValidFrom.Equal(want) {
		t.Errorf("ValidFrom = %s, want %s", model.ValidFrom, want)
	}
	if model.Currency != "EUR" {
		t.Errorf("Currency = %q, want %q", model.Currency, "EUR")
	}

	wantPlatforms := []string{"gardener", "harbor", "hetzner", "ionos", "openstack", "stackit"}
	if got := slices.Sorted(maps.Keys(model.Pricing)); !slices.Equal(got, wantPlatforms) {
		t.Errorf("platforms = %v, want %v", got, wantPlatforms)
	}

	t.Run("the openstack instance is priced in file order", func(t *testing.T) {
		instance := model.Pricing["openstack"]["instance"]

		// The price the dimension's type does not carry stays zero, which is
		// what the pairwise comparison below asserts for both of them.
		want := []pricing.Dimension{
			{Metric: "vcpus", Type: pricing.TypeTimeGauge, PricePerUnitHour: dec(t, "0.02")},
			{Metric: "ram_gb", Type: pricing.TypeTimeGauge, PricePerUnitHour: dec(t, "0.005")},
			{Metric: "disk_gb", Type: pricing.TypeTimeGauge, PricePerUnitHour: dec(t, "0.001")},
			{Metric: "egress_gb", Type: pricing.TypeCounter, PricePerUnit: dec(t, "0.09")},
		}
		if len(instance.Dimensions) != len(want) {
			t.Fatalf("Dimensions = %d, want %d", len(instance.Dimensions), len(want))
		}
		for i, w := range want {
			got := instance.Dimensions[i]
			if got.Metric != w.Metric || got.Type != w.Type {
				t.Errorf("Dimensions[%d] = %s/%s, want %s/%s", i, got.Metric, got.Type, w.Metric, w.Type)
			}
			if !got.PricePerUnitHour.Equal(w.PricePerUnitHour) {
				t.Errorf("Dimensions[%d].PricePerUnitHour = %s, want %s",
					i, got.PricePerUnitHour, w.PricePerUnitHour)
			}
			if !got.PricePerUnit.Equal(w.PricePerUnit) {
				t.Errorf("Dimensions[%d].PricePerUnit = %s, want %s", i, got.PricePerUnit, w.PricePerUnit)
			}
		}

		wantModifiers := map[string]string{"shelved": "0.0", "shutoff": "0.5"}
		if len(instance.StateModifiers) != len(wantModifiers) {
			t.Errorf("StateModifiers = %v, want %v", instance.StateModifiers, wantModifiers)
		}
		for state, text := range wantModifiers {
			got, ok := instance.StateModifiers[state]
			if !ok {
				t.Errorf("StateModifiers has no %q, want %s", state, text)
				continue
			}
			if !got.Equal(dec(t, text)) {
				t.Errorf("StateModifiers[%q] = %s, want %s", state, got, text)
			}
		}
	})
}

func TestPriceSpellings(t *testing.T) {
	asString := mustParse(t, fmt.Sprintf(oneDimension, `"0.02"`))
	asNumber := mustParse(t, fmt.Sprintf(oneDimension, "0.02"))

	// Which spelling the file uses is a matter of taste, so the two models have
	// to price the same thing: a re-import that only requotes a price is the
	// same version, not a conflicting one.
	if !asString.Equal(asNumber) {
		t.Error("the string-priced model does not equal the number-priced one, want them equal")
	}

	want := dec(t, "0.02")
	for name, model := range map[string]pricing.Model{"as a string": asString, "as a number": asNumber} {
		t.Run(name, func(t *testing.T) {
			got := model.Pricing["openstack"]["instance"].Dimensions[0].PricePerUnitHour
			if !got.Equal(want) {
				t.Errorf("PricePerUnitHour = %s, want %s", got, want)
			}
		})
	}
}

func TestCanonicalDocumentRoundTrip(t *testing.T) {
	data := readFile(t, examplePath)

	model, doc, err := pricing.Parse(data)
	if err != nil {
		t.Fatalf("Parse() error = %v, want nil", err)
	}

	// A version read back from the database goes through ParseDocument alone,
	// so it has to yield what the file yielded.
	stored, err := pricing.ParseDocument(doc)
	if err != nil {
		t.Fatalf("ParseDocument() error = %v, want nil", err)
	}
	if !stored.Equal(model) {
		t.Error("the model read back from the document does not equal the parsed one, want them equal")
	}

	// The document is what an import compares an existing version against, so
	// the same file may not encode differently from one run to the next.
	_, again, err := pricing.Parse(data)
	if err != nil {
		t.Fatalf("Parse() error = %v, want nil", err)
	}
	if !bytes.Equal(doc, again) {
		t.Errorf("the second document is %s, want the first one %s", again, doc)
	}
}

func TestParseFailures(t *testing.T) {
	// A schema failure carries the compiler's own message, which names the
	// keyword and the place in the document that failed. The operator who wrote
	// the file has to be able to find the price at fault from it.
	const schemaFailed = "validating the pricing model"

	tests := []struct {
		name     string
		file     string
		document string
		want     []string
	}{
		{
			name:     "a file with no content at all",
			document: "",
			want:     []string{schemaFailed, "got null, want object"},
		},
		{
			name: "a model that is not a mapping",
			file: "non-mapping-root.yaml",
			want: []string{schemaFailed, "got array, want object"},
		},
		{
			name: "a second document",
			file: "second-document.yaml",
			want: []string{"the file holds more than one document, the model belongs in the first"},
		},
		{
			name: "a model without a version",
			file: "missing-version.yaml",
			want: []string{schemaFailed, "missing property 'version'"},
		},
		{
			name: "a model without valid_from",
			file: "missing-valid-from.yaml",
			want: []string{schemaFailed, "missing property 'valid_from'"},
		},
		{
			name: "a model without a currency",
			file: "missing-currency.yaml",
			want: []string{schemaFailed, "missing property 'currency'"},
		},
		{
			name: "a model that prices nothing",
			file: "missing-pricing.yaml",
			want: []string{schemaFailed, "missing property 'pricing'"},
		},
		{
			name: "a currency that is not ISO 4217",
			file: "bad-currency.yaml",
			want: []string{schemaFailed, "at '/currency'", "does not match pattern"},
		},
		{
			name: "a pricing mapping without a platform",
			file: "empty-pricing.yaml",
			want: []string{schemaFailed, "at '/pricing'", "minProperties: got 0, want 1"},
		},
		{
			name: "a resource type without dimensions",
			file: "no-dimensions.yaml",
			want: []string{schemaFailed, "missing property 'dimensions'"},
		},
		{
			name: "a resource type with an empty list of dimensions",
			file: "empty-dimensions.yaml",
			want: []string{schemaFailed, "minItems: got 0, want 1"},
		},
		{
			name: "a dimension type the rating pass does not know",
			file: "unknown-dimension-type.yaml",
			want: []string{schemaFailed, "value must be one of 'time_gauge', 'counter'"},
		},
		{
			name: "a time_gauge that carries both prices",
			file: "time-gauge-with-price-per-unit.yaml",
			want: []string{schemaFailed, "at '/pricing/openstack/instance/dimensions/0': 'oneOf' failed"},
		},
		{
			name: "a time_gauge that carries neither price",
			file: "time-gauge-missing-price.yaml",
			want: []string{schemaFailed, "missing property 'price_per_unit_hour'"},
		},
		{
			name: "a counter that carries both prices",
			file: "counter-with-price-per-unit-hour.yaml",
			want: []string{schemaFailed, "at '/pricing/openstack/instance/dimensions/0': 'oneOf' failed"},
		},
		{
			name: "a counter that carries neither price",
			file: "counter-missing-price.yaml",
			want: []string{schemaFailed, "missing property 'price_per_unit'"},
		},
		{
			name: "a negative price spelled as a number",
			file: "negative-price-number.yaml",
			want: []string{schemaFailed, "minimum: got -0.02, want 0"},
		},
		{
			name: "a negative price spelled as a string",
			file: "negative-price-string.yaml",
			want: []string{schemaFailed, "'-0.02' does not match pattern"},
		},
		{
			name: "a negative modifier",
			file: "negative-modifier.yaml",
			want: []string{schemaFailed, "at '/pricing/openstack/instance/state_modifiers/shutoff'", "minimum: got -0.5"},
		},
		{
			name: "a key the model form does not know",
			file: "unknown-key.yaml",
			want: []string{schemaFailed, "additional properties 'valid_until' not allowed"},
		},
		{
			name: "a key inside a dimension that the model form does not know",
			file: "unknown-dimension-key.yaml",
			want: []string{schemaFailed, "additional properties 'price_per_unit_minute' not allowed"},
		},
		{
			name: "one metric priced twice",
			file: "duplicate-metric.yaml",
			want: []string{"openstack", "instance", "vcpus"},
		},
		{
			name: "a valid_from that is no timestamp",
			file: "bad-valid-from.yaml",
			want: []string{"valid_from", "March 2026"},
		},
		{
			name: "a key set twice",
			file: "repeated-key.yaml",
			want: []string{`the key "currency" is set twice`},
		},
		{
			// An unquoted timestamp is the mistake the format invites, so the
			// refusal names the tag the scalar resolved to rather than leaving
			// the operator to guess which of them to quote.
			name: "an unquoted timestamp",
			document: `version: "2026-03"
valid_from: 2026-03-01T00:00:00Z
currency: "EUR"
pricing:
  openstack:
    instance:
      dimensions:
        - metric: "vcpus"
          type: "time_gauge"
          price_per_unit_hour: "0.02"
`,
			want: []string{"!!timestamp", "quote it"},
		},
		{
			// A tab where YAML wants spaces is the other mistake the format
			// invites, and the decoder refuses it before there is a node to
			// convert. What reaches the operator is the decoder's own message,
			// under the line that says which step failed.
			name:     "a file YAML cannot decode",
			document: "version: \"2026-03\"\n\tcurrency: \"EUR\"\n",
			want:     []string{"parsing the pricing model"},
		},
		{
			name: "a mapping key that is not a string",
			document: `version: "2026-03"
valid_from: "2026-03-01T00:00:00Z"
currency: "EUR"
pricing:
  openstack:
    1: {}
`,
			want: []string{"a mapping key is a !!int", "every key of the model is a string"},
		},
		{
			// yaml.v3 registers an anchor before it parses the node it names,
			// so an anchor can name a node that holds the alias itself. The
			// expansion has to be refused rather than followed.
			name: "an anchor that holds itself",
			document: `version: "2026-03"
valid_from: "2026-03-01T00:00:00Z"
currency: "EUR"
pricing: &a
  openstack: *a
`,
			want: []string{`the anchor "a" holds itself`},
		},
		{
			name:     "anchors that expand to more values than a model holds",
			document: nestedAnchors(),
			want:     []string{"expands to more than", "values, which is what nested anchors do"},
		},
		{
			name:     "anchors that expand to more bytes than a model is",
			document: aliasedScalar(),
			want:     []string{"expands to more than", "bytes, which is what nested anchors do"},
		},
		{
			name:     "anchors whose text is short and whose document is not",
			document: escapedScalar(),
			want:     []string{"expands to more than", "bytes, which is what nested anchors do"},
		},
		{
			name:     "a model nested deeper than any model nests",
			document: "root: " + strings.Repeat("[", 70) + strings.Repeat("]", 70) + "\n",
			want:     []string{"nests more than", "no pricing model does"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data := []byte(tc.document)
			if tc.file != "" {
				data = readFile(t, filepath.Join("testdata", tc.file))
			}

			_, doc, err := pricing.Parse(data)
			if err == nil {
				t.Fatal("Parse() error = nil, want the refusal")
			}
			if doc != nil {
				t.Errorf("Parse() document = %s, want nil", doc)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Parse() error = %q, want it to contain %q", err, want)
				}
			}
		})
	}
}

func TestModelEqual(t *testing.T) {
	tests := []struct {
		name  string
		other string
		equal bool
	}{
		{
			name:  "a modifier respelled as a number",
			other: strings.Replace(twoDimensions, `shutoff: "0.50"`, "shutoff: 0.5", 1),
			equal: true,
		},
		{
			name: "the mapping keys written in another order",
			other: `currency: "EUR"
pricing:
  openstack:
    instance:
      state_modifiers:
        shutoff: "0.50"
      dimensions:
        - type: "time_gauge"
          price_per_unit_hour: "0.02"
          metric: "vcpus"
        - price_per_unit: "0.09"
          metric: "egress_gb"
          type: "counter"
valid_from: "2026-03-01T00:00:00Z"
version: "2026-03"
`,
			equal: true,
		},
		{
			name:  "a changed price",
			other: strings.Replace(twoDimensions, `price_per_unit_hour: "0.02"`, `price_per_unit_hour: "0.03"`, 1),
			equal: false,
		},
		{
			// What a state costs is as much of the model as what a metric
			// costs: a corrected modifier is a corrected price.
			name:  "a changed state modifier",
			other: strings.Replace(twoDimensions, `shutoff: "0.50"`, `shutoff: "0.25"`, 1),
			equal: false,
		},
		{
			name: "a state modifier the other model does not name",
			other: strings.Replace(twoDimensions, `        shutoff: "0.50"`,
				`        shutoff: "0.50"
        shelved: "0.0"`, 1),
			equal: false,
		},
		{
			name: "type modifiers where the other model has none",
			other: twoDimensions + `      type_modifiers:
        hdd: "0.5"
`,
			equal: false,
		},
		{
			name:  "another currency",
			other: strings.Replace(twoDimensions, `currency: "EUR"`, `currency: "USD"`, 1),
			equal: false,
		},
		{
			name:  "another valid_from",
			other: strings.Replace(twoDimensions, `"2026-03-01T00:00:00Z"`, `"2026-03-02T00:00:00Z"`, 1),
			equal: false,
		},
		{
			name: "a platform the other model does not price",
			other: twoDimensions + `  hetzner:
    server:
      dimensions:
        - metric: "vcpus"
          type: "time_gauge"
          price_per_unit_hour: "0.015"
`,
			equal: false,
		},
		{
			name: "a resource type the other model does not price",
			other: twoDimensions + `    volume:
      dimensions:
        - metric: "size_gb"
          type: "time_gauge"
          price_per_unit_hour: "0.0001"
`,
			equal: false,
		},
		{
			name: "one dimension more",
			other: strings.Replace(twoDimensions, `      state_modifiers:`,
				`        - metric: "ram_gb"
          type: "time_gauge"
          price_per_unit_hour: "0.005"
      state_modifiers:`, 1),
			equal: false,
		},
		{
			// The dimensions decide the order of the rated records a resource
			// produces, so a reordering is a different model even though it
			// prices the same metrics.
			name: "the dimensions reordered",
			other: `version: "2026-03"
valid_from: "2026-03-01T00:00:00Z"
currency: "EUR"
pricing:
  openstack:
    instance:
      dimensions:
        - metric: "egress_gb"
          type: "counter"
          price_per_unit: "0.09"
        - metric: "vcpus"
          type: "time_gauge"
          price_per_unit_hour: "0.02"
      state_modifiers:
        shutoff: "0.50"
`,
			equal: false,
		},
	}

	base := mustParse(t, twoDimensions)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			other := mustParse(t, tc.other)

			if got := base.Equal(other); got != tc.equal {
				t.Errorf("Equal() = %t, want %t", got, tc.equal)
			}
			// Equality does not depend on which model is asked.
			if got := other.Equal(base); got != tc.equal {
				t.Errorf("Equal() the other way round = %t, want %t", got, tc.equal)
			}
		})
	}
}

// nestedAnchors builds the file the expansion budget exists for: nine anchors,
// each holding nine references to the one before it. It stays a few hundred
// bytes on disk and names more values than a machine holds.
func nestedAnchors() string {
	var file strings.Builder
	file.WriteString(`a0: &a0 "x"` + "\n")
	for level := 1; level < 9; level++ {
		fmt.Fprintf(&file, "a%d: &a%d [", level, level)
		for i := range 9 {
			if i > 0 {
				file.WriteString(", ")
			}
			fmt.Fprintf(&file, "*a%d", level-1)
		}
		file.WriteString("]\n")
	}
	file.WriteString("pricing: *a8\n")
	return file.String()
}

// aliasedScalar builds the file the byte budget exists for: one scalar of 64
// KiB, named by an anchor three levels of sequences reference eight times each.
// It visits a thousandth of the values the node budget allows, so counting
// nodes never refuses it, and it marshals into more bytes than a model is.
func aliasedScalar() string {
	var file strings.Builder
	fmt.Fprintf(&file, "a0: &a0 %q\n", strings.Repeat("x", 64<<10))
	for level := 1; level < 4; level++ {
		fmt.Fprintf(&file, "a%d: &a%d [", level, level)
		for i := range 8 {
			if i > 0 {
				file.WriteString(", ")
			}
			fmt.Fprintf(&file, "*a%d", level-1)
		}
		file.WriteString("]\n")
	}
	file.WriteString("pricing: *a3\n")
	return file.String()
}

// escapedScalar builds the file the byte budget is charged in escaped bytes
// for: a scalar of 64 KiB of <, named by an anchor three levels of sequences
// reference four times each. The text it writes is under 10 MiB, well inside
// the budget, and encoding/json escapes every < into a six-byte \u003c, so the
// document it marshals into is nearly 60 MiB.
func escapedScalar() string {
	var file strings.Builder
	fmt.Fprintf(&file, "a0: &a0 %q\n", strings.Repeat("<", 64<<10))
	for level := 1; level < 4; level++ {
		fmt.Fprintf(&file, "a%d: &a%d [", level, level)
		for i := range 4 {
			if i > 0 {
				file.WriteString(", ")
			}
			fmt.Fprintf(&file, "*a%d", level-1)
		}
		file.WriteString("]\n")
	}
	file.WriteString("pricing: *a3\n")
	return file.String()
}

// anchoredModifiers writes the state modifiers of two resource types once, as
// an anchor the second one reaches through an alias. It is how an operator
// factors a modifier block their platforms share.
const anchoredModifiers = `version: "2026-03"
valid_from: "2026-03-01T00:00:00Z"
currency: "EUR"
pricing:
  openstack:
    instance:
      dimensions:
        - metric: "vcpus"
          type: "time_gauge"
          price_per_unit_hour: "0.02"
      state_modifiers: &states
        shutoff: "0.50"
  hetzner:
    server:
      dimensions:
        - metric: "vcpus"
          type: "time_gauge"
          price_per_unit_hour: "0.015"
      state_modifiers: *states
`

// expandedModifiers prices what anchoredModifiers prices, with the shared block
// written out under both resource types.
const expandedModifiers = `version: "2026-03"
valid_from: "2026-03-01T00:00:00Z"
currency: "EUR"
pricing:
  openstack:
    instance:
      dimensions:
        - metric: "vcpus"
          type: "time_gauge"
          price_per_unit_hour: "0.02"
      state_modifiers:
        shutoff: "0.50"
  hetzner:
    server:
      dimensions:
        - metric: "vcpus"
          type: "time_gauge"
          price_per_unit_hour: "0.015"
      state_modifiers:
        shutoff: "0.50"
`

func TestAliasesExpand(t *testing.T) {
	anchored, anchoredDoc, err := pricing.Parse([]byte(anchoredModifiers))
	if err != nil {
		t.Fatalf("Parse() of the anchored file error = %v, want nil", err)
	}
	expanded, expandedDoc, err := pricing.Parse([]byte(expandedModifiers))
	if err != nil {
		t.Fatalf("Parse() of the expanded file error = %v, want nil", err)
	}

	// An alias is a spelling, not a price. Whichever of the two files an
	// operator keeps, importing the other one has to be a replay of the same
	// version rather than a conflict, and that is decided on the document.
	if !bytes.Equal(anchoredDoc, expandedDoc) {
		t.Errorf("the anchored document is %s, want the expanded one %s", anchoredDoc, expandedDoc)
	}
	if !anchored.Equal(expanded) {
		t.Error("the anchored model does not equal the expanded one, want them equal")
	}

	modifier, ok := anchored.Pricing["hetzner"]["server"].StateModifiers["shutoff"]
	if !ok {
		t.Fatal("the aliased resource type carries no shutoff modifier, want the anchor expanded")
	}
	if want := dec(t, "0.50"); !modifier.Equal(want) {
		t.Errorf("StateModifiers[shutoff] = %s, want %s", modifier, want)
	}
}

// FuzzParse drives the parser's trust boundary. What reaches it is whatever
// file an operator pointed the import at, and the invariant is that such a file
// either parses or is refused: a parse that panicked, ran out of memory, or ran
// off the stack takes the import command down instead of naming the line that
// is wrong. A model that parsed carries the document it was built from, and
// that document has to yield the same model again, because an import of a
// version the database already holds is decided on that round trip.
func FuzzParse(f *testing.F) {
	f.Add(readFile(f, examplePath))
	f.Add([]byte(twoDimensions))
	f.Add([]byte(anchoredModifiers))
	f.Add([]byte(fmt.Sprintf(oneDimension, `"0.02"`)))
	f.Add([]byte(""))
	f.Add([]byte("not yaml: ["))
	f.Add([]byte("a: &x\n  b: *x\n"))
	f.Add([]byte(nestedAnchors()))

	f.Fuzz(func(t *testing.T, data []byte) {
		model, doc, err := pricing.Parse(data)
		if err != nil {
			if doc != nil {
				t.Errorf("Parse() document = %s beside an error, want nil", doc)
			}
			return
		}

		again, err := pricing.ParseDocument(doc)
		if err != nil {
			t.Fatalf("ParseDocument() of a document Parse produced: %v", err)
		}
		if !again.Equal(model) {
			t.Error("the model read back from the document does not equal the parsed one, want them equal")
		}
	})
}
