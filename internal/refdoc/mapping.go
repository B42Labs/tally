package refdoc

import (
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"strings"
)

// mappingsVar is the variable the collector keeps its mapping table in.
const mappingsVar = "mappings"

// MappingTable renders the collector's mapping from oslo notification types to
// Tally events: one row per entry of the mappings literal, in the order the
// source declares them. The state, the size and the skip columns name the
// function an entry derives the value with rather than describing it, because
// that name is what a reader looks up in the same file.
func MappingTable(src []byte) (string, error) {
	file, err := parseSource(src)
	if err != nil {
		return "", err
	}
	literal, ok := findMappings(file)
	if !ok {
		return "", errors.New("refdoc: no mappings literal in the source")
	}

	rows := make([][]string, 0, len(literal.Elts))
	for _, element := range literal.Elts {
		entry, ok := element.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		osloType, ok := stringLiteral(entry.Key)
		if !ok {
			return "", fmt.Errorf("refdoc: mappings[%s]: key is not a string literal",
				types.ExprString(entry.Key))
		}
		members := entryMembers(entry.Value)

		resourceID := "none"
		if path, ok := pathText(members, "resourceIDPath"); ok {
			resourceID = code(path)
			if fallback, ok := pathText(members, "resourceIDFallbackPath"); ok {
				resourceID += " or " + code(fallback)
			}
		}
		// An entry without a project path takes the owning project from the
		// request context of the notification.
		projectID := "request context"
		if path, ok := pathText(members, "projectIDPath"); ok {
			projectID = code(path)
		}

		rows = append(rows, []string{
			code(osloType),
			literalCell(members, "eventType"),
			literalCell(members, "resourceType"),
			exprCell(members, "state"),
			exprCell(members, "size"),
			resourceID,
			projectID,
			exprCell(members, "skip"),
		})
	}
	return table([]string{
		"Oslo event type", "Tally event type", "Resource type",
		"State", "Size", "Resource id", "Project id", "Skipped when",
	}, rows), nil
}

// findMappings returns the composite literal the mappings variable is assigned.
func findMappings(file *ast.File) (*ast.CompositeLit, bool) {
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			valueSpec, ok := spec.(*ast.ValueSpec)
			if !ok || len(valueSpec.Names) != 1 || len(valueSpec.Values) != 1 {
				continue
			}
			if valueSpec.Names[0].Name != mappingsVar {
				continue
			}
			if literal, ok := valueSpec.Values[0].(*ast.CompositeLit); ok {
				return literal, true
			}
		}
	}
	return nil, false
}

// entryMembers indexes one mapping entry by the fields it sets. A field the
// entry leaves out is absent from the map, which is what the columns render as
// none.
func entryMembers(value ast.Expr) map[string]ast.Expr {
	members := map[string]ast.Expr{}

	literal, ok := value.(*ast.CompositeLit)
	if !ok {
		return members
	}
	for _, element := range literal.Elts {
		field, ok := element.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if name, ok := field.Key.(*ast.Ident); ok {
			members[name.Name] = field.Value
		}
	}
	return members
}

// literalCell renders the value a member holds.
func literalCell(members map[string]ast.Expr, name string) string {
	expr, ok := members[name]
	if !ok {
		return "none"
	}
	return code(literalText(expr))
}

// exprCell renders a member the way the source writes it.
func exprCell(members map[string]ast.Expr, name string) string {
	expr, ok := members[name]
	if !ok {
		return "none"
	}
	return code(types.ExprString(expr))
}

// pathText joins the elements of a payload path with dots, which is how a page
// spells a member nested inside the notification.
func pathText(members map[string]ast.Expr, name string) (string, bool) {
	expr, ok := members[name]
	if !ok {
		return "", false
	}
	literal, ok := expr.(*ast.CompositeLit)
	if !ok || len(literal.Elts) == 0 {
		return "", false
	}

	parts := make([]string, 0, len(literal.Elts))
	for _, element := range literal.Elts {
		parts = append(parts, literalText(element))
	}
	return strings.Join(parts, "."), true
}
