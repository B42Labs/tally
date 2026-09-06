package refdoc

import (
	"go/parser"
	"testing"
)

func TestGoTypeWord(t *testing.T) {
	linked := map[string]bool{"LineItem": true, "Other": true, "dimensionDoc": true}

	cases := map[string]string{
		"string":                  "string",
		"bool":                    "boolean",
		"int":                     "integer",
		"int32":                   "integer",
		"int64":                   "integer",
		"uint64":                  "integer",
		"float64":                 "number",
		"[]string":                "list, comma-separated",
		"time.Time":               "string, RFC 3339 UTC",
		"money.Amount":            "decimal, 2 places",
		"money.Quantity":          "decimal, 4 places",
		"money.Rate":              "decimal, 6 places",
		"jsonMinutes":             "decimal, 4 places",
		"json.RawMessage":         "object",
		"map[string]any":          "object",
		"*string":                 "string or null",
		"*money.Amount":           "decimal, 2 places or null",
		"Other":                   "[Other](#other)",
		"*Other":                  "[Other](#other) or null",
		"[]LineItem":              "array of [LineItem](#lineitem)",
		"map[string]dimensionDoc": "object of [dimensionDoc](#dimensiondoc)",
		"map[string]money.Amount": "object of decimal, 2 places",
		"[]byte":                  "array of `byte`",
		// Three shapes the table does not name, which stay verbatim.
		"decimal.Decimal": "`decimal.Decimal`",
		"[4]byte":         "`[4]byte`",
		"map[int]string":  "`map[int]string`",
	}

	for expr, want := range cases {
		t.Run(expr, func(t *testing.T) {
			parsed, err := parser.ParseExpr(expr)
			if err != nil {
				t.Fatalf("parsing %s: %v", expr, err)
			}
			if got := goTypeWord(parsed, linked); got != want {
				t.Errorf("goTypeWord(%s) = %q, want %q", expr, got, want)
			}
		})
	}
}

func TestGoTypeWordWithoutLinkedTypes(t *testing.T) {
	// A type nothing links to is verbatim rather than a link to a heading that
	// is not on the page.
	parsed, err := parser.ParseExpr("Other")
	if err != nil {
		t.Fatalf("parsing the expression: %v", err)
	}

	if got, want := goTypeWord(parsed, nil), "`Other`"; got != want {
		t.Errorf("goTypeWord(Other) = %q, want %q", got, want)
	}
}

func TestPflagTypeWord(t *testing.T) {
	cases := map[string]string{
		"string":      "string",
		"bool":        "boolean",
		"int":         "integer",
		"int32":       "integer",
		"int64":       "integer",
		"uint":        "integer",
		"uint64":      "integer",
		"float64":     "number",
		"duration":    "duration",
		"stringSlice": "list, comma-separated",
		"stringArray": "string, repeatable",
		"ipNet":       "`ipNet`",
	}

	for typ, want := range cases {
		t.Run(typ, func(t *testing.T) {
			if got := pflagTypeWord(typ); got != want {
				t.Errorf("pflagTypeWord(%q) = %q, want %q", typ, got, want)
			}
		})
	}
}

func TestStdflagTypeWord(t *testing.T) {
	cases := map[string]string{
		"":         "boolean",
		"string":   "string",
		"int":      "integer",
		"uint":     "integer",
		"float":    "number",
		"duration": "duration",
		"value":    "`value`",
	}

	for name, want := range cases {
		t.Run("name "+name, func(t *testing.T) {
			if got := stdflagTypeWord(name); got != want {
				t.Errorf("stdflagTypeWord(%q) = %q, want %q", name, got, want)
			}
		})
	}
}
