package refdoc

import (
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"slices"
)

// Consts renders named constants as a table of the value each one holds and
// what the code says it means, in the order the caller names them.
func Consts(src []byte, names ...string) (string, error) {
	if len(names) == 0 {
		return "", errors.New("refdoc: no constants named")
	}
	file, err := parseSource(src)
	if err != nil {
		return "", err
	}

	rows := make([][]string, 0, len(names))
	for _, name := range names {
		value, meaning, err := findConst(file, name)
		if err != nil {
			return "", err
		}
		rows = append(rows, []string{code(name), code(value), meaning})
	}
	return table([]string{"Name", "Value", "Meaning"}, rows), nil
}

// findConst returns the value the named constant is declared with and the
// comment that documents it. A grouped declaration documents each constant on
// its own spec; a standalone one carries the comment on the declaration, which
// is why the fallback is taken for that shape alone.
func findConst(file *ast.File, name string) (value, meaning string, err error) {
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			valueSpec, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			i := slices.IndexFunc(valueSpec.Names, func(ident *ast.Ident) bool { return ident.Name == name })
			if i < 0 {
				continue
			}
			// A constant that repeats the expression above it, the way an iota
			// group does, has no value the table could show.
			if i >= len(valueSpec.Values) {
				return "", "", fmt.Errorf("refdoc: constant %s has no value of its own", name)
			}
			doc := valueSpec.Doc
			if doc == nil && !gen.Lparen.IsValid() {
				doc = gen.Doc
			}
			return literalText(valueSpec.Values[i]), joinComment(doc), nil
		}
	}
	return "", "", fmt.Errorf("refdoc: no constant %s in the source", name)
}
