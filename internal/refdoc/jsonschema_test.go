package refdoc

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// realSchemas are the JSON Schema documents of this repository, both of which
// the renderer has to get through.
var realSchemas = []string{
	"../engine/pricing/pricing.schema.json",
	"../core/adjustment/adjustments_schema.json",
}

// assertOrder checks that the parts appear in the rendering in the order they
// are named. A renderer whose output is read top to bottom is asserted this way
// rather than by its lines alone, because a section that moved still carries
// every line it did before.
func assertOrder(t *testing.T, got string, parts []string) {
	t.Helper()

	at := 0
	for _, part := range parts {
		i := strings.Index(got[at:], part)
		if i < 0 {
			t.Fatalf("%q does not follow what stands above it:\n%s", part, got)
		}
		at += i + len(part)
	}
}

func TestJSONSchema(t *testing.T) {
	got, err := JSONSchema(readFixture(t, "schema.json"))
	if err != nil {
		t.Fatalf("JSONSchema() error = %v, want nil", err)
	}

	assertWant(t, "schema.want.md", got)
}

func TestJSONSchemaRendersAnArrayRoot(t *testing.T) {
	got, err := JSONSchema(readFixture(t, "schema_array.json"))
	if err != nil {
		t.Fatalf("JSONSchema() error = %v, want nil", err)
	}

	assertWant(t, "schema_array.want.md", got)
}

func TestJSONSchemaKeepsTheOrderOfTheDocument(t *testing.T) {
	got, err := JSONSchema(readFixture(t, "schema.json"))
	if err != nil {
		t.Fatalf("JSONSchema() error = %v, want nil", err)
	}

	// The definitions and the properties are neither sorted nor grouped: a
	// reader follows the file, so the sections and the rows follow it too.
	assertOrder(t, got, []string{
		"#### `root`", "`version`", "`kind`", "`tags`", "`entries`",
		"#### `entry`", "#### `measure`", "#### `labels`", "#### `window`", "#### `rate`",
	})
}

func TestJSONSchemaRendersEachSchemaShape(t *testing.T) {
	got, err := JSONSchema(readFixture(t, "schema.json"))
	if err != nil {
		t.Fatalf("JSONSchema() error = %v, want nil", err)
	}

	for _, want := range []string{
		// A reference is a link, an array names what it holds, a property with
		// an enum alone is an enum, and one with a oneOf alone is alternatives.
		"| `measures` | array of [measure](#measure) | yes | minItems 1 |",
		"| `kind` | enum | no | `draft`, `final` |",
		"| `either` | alternatives | no | none |",
		// A nested additionalProperties states the constraint of every level.
		"| `entries` | object | yes | minProperties 1; values object (minProperties 1; values [entry](#entry)) |",
		// The three shapes of additionalProperties.
		"No other property is allowed.",
		"Other properties are allowed.",
		"Other properties are `string`.",
		"Other properties are [rate](#rate).",
		// A oneOf on an object opens with what the bullets under it state, and
		// each alternative names what it requires, what it forbids, and what it
		// pins.
		"Exactly one of these alternatives holds:\n\n" +
			"- `hourly` is required, `each` is absent, `kind` is `gauge`",
		// A definition without properties is one sentence.
		"A number at least 0, or a string matching `^[0-9]+(\\.[0-9]+)?$`.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the rendering does not carry %q:\n%s", want, got)
		}
	}
}

func TestJSONSchemaRendersTheSchemasOfTheRepository(t *testing.T) {
	for _, path := range realSchemas {
		t.Run(path, func(t *testing.T) {
			assertRendersRealSource(t, path, JSONSchema)
		})
	}
}

func TestJSONSchemaReportsASyntaxErrorWithItsOffset(t *testing.T) {
	_, err := JSONSchema([]byte(`{"type": "object",}`))
	if err == nil {
		t.Fatal("JSONSchema() error = nil, want a syntax error")
	}

	// The offset is what a reader needs to find the character, so the cause is
	// kept rather than flattened into a sentence of this package's own.
	var syntax *json.SyntaxError
	if !errors.As(err, &syntax) {
		t.Fatalf("JSONSchema() error = %v, want a *json.SyntaxError", err)
	}
	if !strings.Contains(err.Error(), "at offset 19") {
		t.Errorf("JSONSchema() error = %q, want it to carry the offset", err)
	}
}

func TestJSONSchemaRendersAnArrayWithoutItems(t *testing.T) {
	cases := map[string]struct {
		schema string
		want   string
	}{
		// An array the document holds to nothing at all.
		"no items and no bound": {
			schema: `{"type": "array"}`,
			want:   "The array is unconstrained.",
		},
		// One whose items are unconstrained still carries the bound on the
		// array itself, which the item sentence must not swallow.
		"a bound without items": {
			schema: `{"type": "array", "minItems": 1}`,
			want:   "Each item is unconstrained. The array holds at least 1 item.",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := JSONSchema([]byte(tc.schema))
			if err != nil {
				t.Fatalf("JSONSchema() error = %v, want nil", err)
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("the rendering does not carry %q:\n%s", tc.want, got)
			}
		})
	}
}

func TestJSONSchemaRejectsWhatItCannotRender(t *testing.T) {
	cases := map[string]struct {
		schema string
		want   string
	}{
		"no type on the root": {
			schema: `{"title": "Nameless", "properties": {"a": {"type": "string"}}}`,
			want:   "refdoc: schema has no type",
		},
		"a reference out of the document": {
			schema: `{"type": "object", "properties": {"a": {"$ref": "#/components/x"}}}`,
			want:   `refdoc: unsupported $ref "#/components/x"`,
		},
		"a reference under a definition": {
			schema: `{"type": "object", "$defs": {"a": {"items": {"$ref": "other.json#/x"}}}}`,
			want:   `refdoc: unsupported $ref "other.json#/x"`,
		},
		// The draft asks for an object here, and the decoder names the package
		// and the document once rather than twice.
		"a property set that is not an object": {
			schema: `{"type": "object", "properties": []}`,
			want:   "refdoc: schema: a set of named schemas is not an object",
		},
		// A keyword this renderer does not read would drop out of the page and
		// leave it stating a weaker contract than the validator enforces.
		"a keyword no page renders": {
			schema: `{"type": "object", "properties": {"a": {"type": "integer", "maximum": 10}}}`,
			want:   `refdoc: schema: keyword "maximum" is not rendered`,
		},
		// A not is read by the bullet of a oneOf alternative and nowhere else,
		// so one standing on the object itself is dropped by the section that
		// renders the object.
		"a not outside an alternative": {
			schema: `{"type": "object", "properties": {"a": {"type": "string"}},` +
				` "not": {"required": ["a"]}}`,
			want: "refdoc: a not outside a stated oneOf alternative is not rendered",
		},
		"a not under a definition": {
			schema: `{"type": "object", "$defs": {"a": {"not": {"required": ["b"]}}}}`,
			want:   "refdoc: a not outside a stated oneOf alternative is not rendered",
		},
		// Only an object naming properties is rendered as the table and the
		// bullets; one naming none is a single sentence stating what the value
		// is, which says the type of each alternative and nothing else.
		"a not in an alternative of an object without properties": {
			schema: `{"type": "object", "$defs": {"a": {"oneOf": [{"required": ["b"],` +
				` "not": {"required": ["c"]}}]}}}`,
			want: "refdoc: a not outside a stated oneOf alternative is not rendered",
		},
		"a not in an alternative of an item without properties": {
			schema: `{"type": "array", "items": {"oneOf": [{"required": ["a"],` +
				` "not": {"required": ["b"]}}]}}`,
			want: "refdoc: a not outside a stated oneOf alternative is not rendered",
		},
		// A section is written for the root and for a definition, so a property
		// carrying alternatives of its own is rendered as its type word alone.
		"a not in an alternative of a property": {
			schema: `{"type": "object", "properties": {"a": {"type": "object",` +
				` "properties": {"b": {"type": "string"}}, "oneOf": [{"required": ["b"],` +
				` "not": {"required": ["c"]}}]}}}`,
			want: "refdoc: a not outside a stated oneOf alternative is not rendered",
		},
		// The bullet states the required members of a not and nothing else, so
		// a not pinning a property down states a condition the page leaves out.
		"a not beyond required": {
			schema: `{"type": "object", "properties": {"a": {"type": "string"}},` +
				` "oneOf": [{"required": ["a"], "not": {"required": ["b"],` +
				` "properties": {"b": {"const": "x"}}}}]}`,
			want: "refdoc: a not beyond required is not rendered",
		},
		"a not requiring nothing": {
			schema: `{"type": "object", "properties": {"a": {"type": "string"}},` +
				` "oneOf": [{"required": ["a"], "not": {"minProperties": 1}}]}`,
			want: "refdoc: a not beyond required is not rendered",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := JSONSchema([]byte(tc.schema))
			if err == nil {
				t.Fatalf("JSONSchema() error = nil, want %q", tc.want)
			}
			if err.Error() != tc.want {
				t.Errorf("JSONSchema() error = %q, want %q", err, tc.want)
			}
		})
	}
}
