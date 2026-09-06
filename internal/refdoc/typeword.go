package refdoc

import (
	"fmt"
	"go/ast"
	"go/types"
	"strings"
)

// The words a page uses for the shapes several types share.
const (
	wordList       = "list, comma-separated"
	wordRepeatable = "string, repeatable"
	wordInteger    = "integer"
	wordNumber     = "number"
)

// typeWords maps a type, spelled the way the source spells it, to the word the
// pages name it by. It holds the Go types the documents and the settings carry
// and the type names pflag reports, which overlap in the scalars.
var typeWords = map[string]string{
	"string":          "string",
	"bool":            "boolean",
	"int":             wordInteger,
	"int32":           wordInteger,
	"int64":           wordInteger,
	"uint64":          wordInteger,
	"float64":         wordNumber,
	"duration":        "duration",
	"[]string":        wordList,
	"map[string]any":  "object",
	"json.RawMessage": "object",
	"time.Time":       "string, RFC 3339 UTC",
	"money.Amount":    "decimal, 2 places",
	"money.Quantity":  "decimal, 4 places",
	"money.Rate":      "decimal, 6 places",
	"jsonMinutes":     "decimal, 4 places",
	"jsonQuantity":    "decimal, 4 places",
}

// goTypeWord is the word for a Go type as a member table names it. A type in
// linked is rendered as a link to its own heading, which is how a document
// whose members carry other documents is read; every other type the table above
// does not name is rendered verbatim in a code span.
func goTypeWord(expr ast.Expr, linked map[string]bool) string {
	text := types.ExprString(expr)
	if word, ok := typeWords[text]; ok {
		return word
	}
	if ident, ok := expr.(*ast.Ident); ok && linked[ident.Name] {
		return fmt.Sprintf("[%s](#%s)", ident.Name, strings.ToLower(ident.Name))
	}

	switch t := expr.(type) {
	case *ast.StarExpr:
		return goTypeWord(t.X, linked) + " or null"
	case *ast.ArrayType:
		if t.Len == nil {
			return "array of " + goTypeWord(t.Elt, linked)
		}
	case *ast.MapType:
		if types.ExprString(t.Key) == "string" {
			return "object of " + goTypeWord(t.Value, linked)
		}
	}
	return code(text)
}

// pflagTypeWord is the word for the type a cobra flag reports, the string
// pflag.Value.Type returns.
func pflagTypeWord(typ string) string {
	switch typ {
	case "uint":
		return wordInteger
	case "stringSlice":
		return wordList
	case "stringArray":
		return wordRepeatable
	}
	if word, ok := typeWords[typ]; ok {
		return word
	}
	return code(typ)
}

// stdflagTypeWord is the word for the type name flag.UnquoteUsage returns for a
// flag of the standard library's flag package. That name is empty for a boolean
// and abbreviated for the numbers, which is why the pflag words do not fit it.
func stdflagTypeWord(name string) string {
	switch name {
	case "":
		return "boolean"
	case "int", "uint":
		return wordInteger
	case "float":
		return wordNumber
	}
	if word, ok := typeWords[name]; ok {
		return word
	}
	return code(name)
}
