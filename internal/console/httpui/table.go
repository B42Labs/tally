package httpui

import (
	"cmp"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/shopspring/decimal"
)

// A table is sorted and filtered here, on the rows the page already holds. The
// console ships no JavaScript, so a heading the viewer clicks is a link that
// reloads the page with the order in its query string, and the filter box is a
// form that reloads it with the text. Two parameters carry the state of one
// table, named after the table so that the tables of one page keep their own:
// <table>.sort holds the key of a column, prefixed with a hyphen for descending,
// and <table>.q holds the filter text.
//
// A listing the Reporting API pages is sorted and filtered within the page it
// answered, and the link to the next page carries both parameters along.
const (
	sortSuffix  = ".sort"
	querySuffix = ".q"
	descending  = "-"
)

// cell is one value of a table as sorting and filtering see it: the text the
// filter matches, folded to lower case once, and for a numeric column the number
// the order is decided by.
type cell struct {
	text    string
	number  decimal.Decimal
	numeric bool
}

// column is one column of a table: the key its sort parameter names, the
// heading it prints, and the cell a row shows in it. A column without a Cell is
// drawn but neither sorted nor searched, a bar for example.
type column[T any] struct {
	Key     string
	Label   string
	Cell    func(T) cell
	numeric bool
}

// columnKey is the label with its spaces as underscores, which is what the
// sort parameter carries: "resource type" sorts as resource_type.
func columnKey(label string) string {
	return strings.ReplaceAll(label, " ", "_")
}

// textCol declares a column that sorts and filters as text.
func textCol[T any](label string, get func(T) string) column[T] {
	return column[T]{
		Key:   columnKey(label),
		Label: label,
		Cell:  func(row T) cell { return cell{text: strings.ToLower(get(row))} },
	}
}

// numberCol declares a column that sorts as a number. The first click on its
// heading sorts descending, because the largest amount is what a viewer looks
// for first.
func numberCol[T any](label string, get func(T) decimal.Decimal) column[T] {
	return column[T]{
		Key:     columnKey(label),
		Label:   label,
		numeric: true,
		Cell: func(row T) cell {
			value := get(row)
			return cell{text: value.String(), number: value, numeric: true}
		},
	}
}

// countCol declares a numeric column over an integer.
func countCol[T any](label string, get func(T) int64) column[T] {
	return numberCol(label, func(row T) decimal.Decimal { return decimal.NewFromInt(get(row)) })
}

// plainCol declares a column that is drawn and nothing else.
func plainCol[T any](label string) column[T] {
	return column[T]{Label: label}
}

// field is one query parameter the filter form carries through as a hidden
// input, so that filtering a table keeps the page's identifiers, its paging
// cursor, and the state of every other table.
type field struct {
	Name  string
	Value string
}

// header is one heading as the template prints it: the label, the link that
// sorts by the column or nothing for a column that cannot, the direction the
// table is sorted in when this is the column, as aria-sort spells it, and
// whether the column holds numbers, which the heading is aligned over.
type header struct {
	Label   string
	Link    string
	Sort    string
	Numeric bool
}

// tableView is what the template renders around a table: the filter form, the
// headings with their sort links, and how many rows survived the filter.
type tableView struct {
	Path      string
	Query     string
	QueryName string
	Hidden    []field
	ClearLink string
	Headers   []header
	Shown     int
	Total     int
	// Paged is set for a listing that is one page of a longer list, so the
	// row count says which rows it counted.
	Paged bool
}

// Count is the row count the form prints: how many rows the table holds, and
// under a filter how many of them match.
func (v tableView) Count() string {
	scope := ""
	if v.Paged {
		scope = " on this page"
	}
	if v.Query == "" {
		return rowsText(v.Total) + scope
	}
	return fmt.Sprintf("%d of %s%s match", v.Shown, rowsText(v.Total), scope)
}

// rowsText counts rows in words, one row or n rows.
func rowsText(n int) string {
	if n == 1 {
		return "1 row"
	}
	return fmt.Sprintf("%d rows", n)
}

// listing is one table as the page renders it: the rows in the order and under
// the filter the viewer asked for, and the controls that got them there.
type listing[T any] struct {
	Rows  []T
	Table tableView
}

// paged marks a listing as one page of a longer list.
func (l listing[T]) paged() listing[T] {
	l.Table.Paged = true
	return l
}

// tabulate applies what the request says about the table id to rows: the
// filter first, then the order. The rows are copied, so the caller's slice
// stays in the order it was read in, which is the order a picture built from
// the same rows relies on.
func tabulate[T any](r *http.Request, id string, cols []column[T], rows []T) listing[T] {
	return tabulateValues(r.URL.Path, r.URL.Query(), id, cols, rows)
}

// tabulateBy is tabulate with the order a table opens in when the viewer names
// none: the key of a column, with the hyphen for descending. The order is put
// into the query the links and the form are built from, so the heading says
// which way the table is sorted and the filter keeps it.
func tabulateBy[T any](r *http.Request, id string, cols []column[T], rows []T, defaultSort string) listing[T] {
	values := r.URL.Query()
	if values.Get(id+sortSuffix) == "" {
		values.Set(id+sortSuffix, defaultSort)
	}
	return tabulateValues(r.URL.Path, values, id, cols, rows)
}

// tabulateValues is tabulate over a query a caller has already read or edited.
func tabulateValues[T any](path string, values url.Values, id string, cols []column[T], rows []T) listing[T] {
	sortName, queryName := id+sortSuffix, id+querySuffix

	query := strings.TrimSpace(values.Get(queryName))
	sortKey, desc := strings.CutPrefix(values.Get(sortName), descending)

	kept := filterRows(cols, rows, query)
	if index := slices.IndexFunc(cols, func(c column[T]) bool { return c.Key == sortKey && c.Cell != nil }); index >= 0 {
		sortRows(kept, cols[index], desc)
	}

	view := tableView{
		Path:      path,
		Query:     query,
		QueryName: queryName,
		Shown:     len(kept),
		Total:     len(rows),
	}
	for _, key := range slices.Sorted(maps.Keys(values)) {
		if key == queryName {
			continue
		}
		for _, value := range values[key] {
			if value != "" {
				view.Hidden = append(view.Hidden, field{Name: key, Value: value})
			}
		}
	}
	if query != "" {
		cleared := maps.Clone(values)
		cleared.Del(queryName)
		view.ClearLink = href(path, cleared)
	}
	for _, col := range cols {
		view.Headers = append(view.Headers, headerFor(path, values, sortName, col, sortKey, desc))
	}
	return listing[T]{Rows: kept, Table: view}
}

// headerFor builds one heading. The link of the sorted column flips its
// direction; the link of any other column sorts by it, ascending for text and
// descending for a number.
func headerFor[T any](
	path string, values url.Values, sortName string, col column[T], sortKey string, desc bool,
) header {
	head := header{Label: col.Label, Numeric: col.numeric}
	if col.Cell == nil {
		return head
	}

	next := col.Key
	switch {
	case col.Key == sortKey && !desc:
		head.Sort = "ascending"
		next = descending + col.Key
	case col.Key == sortKey && desc:
		head.Sort = "descending"
	case col.numeric:
		next = descending + col.Key
	}

	linked := maps.Clone(values)
	linked.Set(sortName, next)
	head.Link = href(path, linked)
	return head
}

// filterRows keeps every row with a cell that contains the query, compared in
// lower case. An empty query keeps every row.
func filterRows[T any](cols []column[T], rows []T, query string) []T {
	if query == "" {
		return slices.Clone(rows)
	}

	needle := strings.ToLower(query)
	kept := make([]T, 0, len(rows))
	for _, row := range rows {
		for _, col := range cols {
			if col.Cell != nil && strings.Contains(col.Cell(row).text, needle) {
				kept = append(kept, row)
				break
			}
		}
	}
	return kept
}

// keyedRow is a row beside the cell it is sorted by, computed once rather than
// on every comparison.
type keyedRow[T any] struct {
	row T
	key cell
}

// sortRows orders rows by one column in place. The sort is stable, so rows
// equal in the column keep the order they were read in.
func sortRows[T any](rows []T, col column[T], desc bool) {
	keyed := make([]keyedRow[T], len(rows))
	for i, row := range rows {
		keyed[i] = keyedRow[T]{row: row, key: col.Cell(row)}
	}

	slices.SortStableFunc(keyed, func(a, b keyedRow[T]) int {
		order := compareCells(a.key, b.key)
		if desc {
			return -order
		}
		return order
	})

	for i := range keyed {
		rows[i] = keyed[i].row
	}
}

// compareCells orders two cells of one column: numbers by value, text by its
// folded form.
func compareCells(a, b cell) int {
	if a.numeric && b.numeric {
		return a.number.Cmp(b.number)
	}
	return cmp.Compare(a.text, b.text)
}

// href encodes an edited query the way link does: every empty value is dropped,
// and a query with nothing left is no query at all.
func href(path string, values url.Values) string {
	clean := url.Values{}
	for key, list := range values {
		for _, value := range list {
			if value != "" {
				clean.Add(key, value)
			}
		}
	}
	if len(clean) == 0 {
		return path
	}
	return path + "?" + clean.Encode()
}

// nextLink is the link to the next page of a listing: the request's own query,
// filters and table state included, with the cursor the API answered.
func nextLink(r *http.Request, cursor string) string {
	values := r.URL.Query()
	values.Set("cursor", cursor)
	return href(r.URL.Path, values)
}
