package refdoc

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"reflect"
	"strconv"
	"strings"
)

// sourceName is what a parse error names the position in. The renderers take
// the bytes of a source file rather than its path, so the caller reads the file
// and this stands in for its name.
const sourceName = "source.go"

// parseSource parses one Go file, comments included, because the comments are
// what the pages say about the code. A parse error is returned as it is: it
// carries the position of what the parser stumbled over.
func parseSource(src []byte) (*ast.File, error) {
	return parser.ParseFile(token.NewFileSet(), sourceName, src, parser.ParseComments)
}

// typeDecl is a type declaration with the comment that documents it. A
// standalone declaration carries the comment on the GenDecl and a grouped one
// on the TypeSpec, so both are looked at and whichever is set is kept.
type typeDecl struct {
	spec *ast.TypeSpec
	doc  *ast.CommentGroup
}

// findType returns the declaration of the named type, or false when the file
// declares no such type.
func findType(file *ast.File, name string) (typeDecl, bool) {
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok || typeSpec.Name.Name != name {
				continue
			}
			doc := typeSpec.Doc
			if doc == nil {
				doc = gen.Doc
			}
			return typeDecl{spec: typeSpec, doc: doc}, true
		}
	}
	return typeDecl{}, false
}

// tagValue returns the value the field's struct tag holds for key, and whether
// the tag holds it at all.
func tagValue(field *ast.Field, key string) (string, bool) {
	if field.Tag == nil {
		return "", false
	}
	unquoted, err := strconv.Unquote(field.Tag.Value)
	if err != nil {
		return "", false
	}
	return reflect.StructTag(unquoted).Lookup(key)
}

// joinComment renders a doc comment as one line, its lines trimmed and joined
// by a single space, which is what a table cell takes. A comment that is not
// there is the empty string.
func joinComment(group *ast.CommentGroup) string {
	if group == nil {
		return ""
	}

	var lines []string
	for line := range strings.SplitSeq(group.Text(), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	return strings.Join(lines, " ")
}

// stringLiteral returns the text a string literal holds, without its quotes.
func stringLiteral(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	unquoted, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return unquoted, true
}

// literalText renders a value the way a page shows it: a string without the
// quotes the source spells it with, and everything else as it is written.
func literalText(expr ast.Expr) string {
	if text, ok := stringLiteral(expr); ok {
		return text
	}
	return types.ExprString(expr)
}
