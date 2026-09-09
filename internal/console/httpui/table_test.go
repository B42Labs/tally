package httpui

import (
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// tableRow is what the table tests sort and filter.
type tableRow struct {
	Name  string
	Count int64
}

// tableColumns is a text column, a number column and a plain one, which is
// every kind a page declares.
var tableColumns = []column[tableRow]{
	textCol("name", func(r tableRow) string { return r.Name }),
	countCol("row count", func(r tableRow) int64 { return r.Count }),
	plainCol[tableRow]("share"),
}

// tableRows is deliberately out of order in both columns, and mixed in case.
func tableRows() []tableRow {
	return []tableRow{{Name: "beta", Count: 2}, {Name: "Alpha", Count: 10}, {Name: "gamma", Count: 1}}
}

// names is the order the rows came out in.
func names(rows []tableRow) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.Name)
	}
	return out
}

// tabulateAt runs the table under one request URL.
func tabulateAt(target string, rows []tableRow) listing[tableRow] {
	return tabulate(httptest.NewRequest("GET", target, nil), "t", tableColumns, rows)
}

func TestTabulate(t *testing.T) {
	t.Parallel()

	t.Run("no state keeps the order as read", func(t *testing.T) {
		t.Parallel()

		got := tabulateAt("/page?id=7", tableRows())
		if want := []string{"beta", "Alpha", "gamma"}; !slices.Equal(names(got.Rows), want) {
			t.Errorf("rows = %v, want %v", names(got.Rows), want)
		}
		if got.Table.Count() != "3 rows" {
			t.Errorf("count = %q, want 3 rows", got.Table.Count())
		}
		for _, head := range got.Table.Headers {
			if head.Sort != "" {
				t.Errorf("the heading %s says it is sorted %s", head.Label, head.Sort)
			}
		}
		if got.Table.ClearLink != "" {
			t.Error("a table without a filter offers to clear one")
		}
	})

	t.Run("text sorts ascending without regard to case", func(t *testing.T) {
		t.Parallel()

		got := tabulateAt("/page?t.sort=name", tableRows())
		if want := []string{"Alpha", "beta", "gamma"}; !slices.Equal(names(got.Rows), want) {
			t.Errorf("rows = %v, want %v", names(got.Rows), want)
		}
	})

	t.Run("a hyphen sorts descending", func(t *testing.T) {
		t.Parallel()

		got := tabulateAt("/page?t.sort=-row_count", tableRows())
		if want := []string{"Alpha", "beta", "gamma"}; !slices.Equal(names(got.Rows), want) {
			t.Errorf("rows = %v, want %v", names(got.Rows), want)
		}
	})

	t.Run("a number sorts as a number", func(t *testing.T) {
		t.Parallel()

		got := tabulateAt("/page?t.sort=row_count", tableRows())
		// As text, 10 would come before 2.
		if want := []string{"gamma", "beta", "Alpha"}; !slices.Equal(names(got.Rows), want) {
			t.Errorf("rows = %v, want %v", names(got.Rows), want)
		}
	})

	t.Run("the sorted heading says its direction and flips it", func(t *testing.T) {
		t.Parallel()

		got := tabulateAt("/page?id=7&t.sort=name", tableRows())
		name := got.Table.Headers[0]
		if name.Sort != "ascending" {
			t.Errorf("aria-sort = %q, want ascending", name.Sort)
		}
		if name.Link != "/page?id=7&t.sort=-name" {
			t.Errorf("link = %q, want the descending sort beside the id", name.Link)
		}

		got = tabulateAt("/page?t.sort=-name", tableRows())
		name = got.Table.Headers[0]
		if name.Sort != "descending" {
			t.Errorf("aria-sort = %q, want descending", name.Sort)
		}
		if name.Link != "/page?t.sort=name" {
			t.Errorf("link = %q, want the ascending sort", name.Link)
		}
	})

	t.Run("a first click sorts text ascending and a number descending", func(t *testing.T) {
		t.Parallel()

		got := tabulateAt("/page", tableRows())
		if link := got.Table.Headers[0].Link; link != "/page?t.sort=name" {
			t.Errorf("text link = %q, want ascending", link)
		}
		if link := got.Table.Headers[1].Link; link != "/page?t.sort=-row_count" {
			t.Errorf("number link = %q, want descending", link)
		}
	})

	t.Run("a plain column has no link", func(t *testing.T) {
		t.Parallel()

		got := tabulateAt("/page", tableRows())
		share := got.Table.Headers[2]
		if share.Label != "share" || share.Link != "" {
			t.Errorf("plain heading = %+v, want the label and no link", share)
		}
	})

	t.Run("only a number heading is numeric", func(t *testing.T) {
		t.Parallel()

		got := tabulateAt("/page", tableRows())
		if got.Table.Headers[0].Numeric || !got.Table.Headers[1].Numeric || got.Table.Headers[2].Numeric {
			t.Errorf("headings = %+v, want the count alone numeric", got.Table.Headers)
		}
	})

	t.Run("a key no column carries sorts nothing", func(t *testing.T) {
		t.Parallel()

		got := tabulateAt("/page?t.sort=share", tableRows())
		if want := []string{"beta", "Alpha", "gamma"}; !slices.Equal(names(got.Rows), want) {
			t.Errorf("rows = %v, want the order as read", names(got.Rows))
		}
	})

	t.Run("the filter matches any cell without regard to case", func(t *testing.T) {
		t.Parallel()

		got := tabulateAt("/page?id=7&t.q=ALPHA", tableRows())
		if want := []string{"Alpha"}; !slices.Equal(names(got.Rows), want) {
			t.Errorf("rows = %v, want %v", names(got.Rows), want)
		}
		if got.Table.Count() != "1 of 3 rows match" {
			t.Errorf("count = %q", got.Table.Count())
		}
		if got.Table.ClearLink != "/page?id=7" {
			t.Errorf("clear link = %q, want the page without the filter", got.Table.ClearLink)
		}
		if got.Table.Query != "ALPHA" {
			t.Errorf("the form shows %q, want the text as typed", got.Table.Query)
		}
	})

	t.Run("a number is matched by its text", func(t *testing.T) {
		t.Parallel()

		got := tabulateAt("/page?t.q=10", tableRows())
		if want := []string{"Alpha"}; !slices.Equal(names(got.Rows), want) {
			t.Errorf("rows = %v, want %v", names(got.Rows), want)
		}
	})

	t.Run("the filter is trimmed and then sorted", func(t *testing.T) {
		t.Parallel()

		got := tabulateAt("/page?t.q=%20a%20&t.sort=-name", tableRows())
		if want := []string{"gamma", "beta", "Alpha"}; !slices.Equal(names(got.Rows), want) {
			t.Errorf("rows = %v, want %v", names(got.Rows), want)
		}
	})

	t.Run("the form carries every other parameter and not its own", func(t *testing.T) {
		t.Parallel()

		got := tabulateAt("/page?id=7&t.sort=name&t.q=a&u.q=z&empty=", tableRows())
		want := []field{{"id", "7"}, {"t.sort", "name"}, {"u.q", "z"}}
		if !slices.Equal(got.Table.Hidden, want) {
			t.Errorf("hidden fields = %v, want %v", got.Table.Hidden, want)
		}
		if got.Table.QueryName != "t.q" || got.Table.Path != "/page" {
			t.Errorf("the form posts %s to %s", got.Table.QueryName, got.Table.Path)
		}
	})

	t.Run("a paged listing counts the page", func(t *testing.T) {
		t.Parallel()

		got := tabulateAt("/page", tableRows()).paged()
		if got.Table.Count() != "3 rows on this page" {
			t.Errorf("count = %q", got.Table.Count())
		}
		got = tabulateAt("/page?t.q=a", tableRows()).paged()
		if got.Table.Count() != "3 of 3 rows on this page match" {
			t.Errorf("count = %q", got.Table.Count())
		}
	})

	t.Run("one row is counted in the singular", func(t *testing.T) {
		t.Parallel()

		got := tabulateAt("/page", tableRows()[:1])
		if got.Table.Count() != "1 row" {
			t.Errorf("count = %q", got.Table.Count())
		}
	})

	t.Run("the caller's rows keep their order", func(t *testing.T) {
		t.Parallel()

		rows := tableRows()
		tabulateAt("/page?t.sort=name", rows)
		if want := []string{"beta", "Alpha", "gamma"}; !slices.Equal(names(rows), want) {
			t.Errorf("the caller's rows = %v, want them untouched", names(rows))
		}
	})
}

func TestEmptyText(t *testing.T) {
	t.Parallel()

	if got := emptyText(tableView{}, "nothing here"); got != "nothing here" {
		t.Errorf("without a filter = %q", got)
	}
	if got := emptyText(tableView{Query: "x", Total: 0}, "nothing here"); got != "nothing here" {
		t.Errorf("with a filter over nothing = %q", got)
	}
	if got := emptyText(tableView{Query: "x", Total: 3}, "nothing here"); got != "no row matches the filter" {
		t.Errorf("with a filter that emptied the table = %q", got)
	}
}

func TestHref(t *testing.T) {
	t.Parallel()

	if got := href("/p", url.Values{}); got != "/p" {
		t.Errorf("no query = %q", got)
	}
	if got := href("/p", url.Values{"b": {""}, "a": {"x y"}}); got != "/p?a=x+y" {
		t.Errorf("an empty value and an encoded one = %q", got)
	}

	next := nextLink(httptest.NewRequest("GET", "/projects?platform=openstack&projects.q=p", nil), "abc")
	for _, part := range []string{"platform=openstack", "projects.q=p", "cursor=abc"} {
		if !strings.Contains(next, part) {
			t.Errorf("the next link %q drops %s", next, part)
		}
	}
}

func TestBackPath(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"/projects?platform=openstack": "/projects?platform=openstack",
		"/":                            "/",
		"":                             "/",
		"projects":                     "/",
		"https://example.com/":         "/",
		"//example.com/":               "/",
		`/\example.com/`:               "/",
	}
	for back, want := range cases {
		if got := backPath(back); got != want {
			t.Errorf("backPath(%q) = %q, want %q", back, got, want)
		}
	}
}
