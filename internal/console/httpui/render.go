package httpui

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"net/http"
)

// htmlContentType is what every rendered page is served as, the error page
// included.
const htmlContentType = "text/html; charset=utf-8"

// stylesheetPath is the embedded stylesheet, and /static/console.css is the
// only route that serves it.
const stylesheetPath = "static/console.css"

// errorPage is the page every failure is rendered with.
const errorPage = "error"

// files carries the templates and the stylesheet inside the binary, so the
// console runs with nothing next to it on disk.
//
//go:embed templates/*.gohtml static/console.css
var files embed.FS

// pageNames is every page template. Each of them defines "content" under that
// one name, so each is parsed into its own clone of the layout: one template
// set cannot hold two definitions of the same name.
var pageNames = []string{
	"overview",
	"projects",
	"project",
	"resources",
	"resource",
	"pricing",
	"catalog",
	"run",
	"statement",
	errorPage,
}

// parsePages parses the layout once and clones it per page.
func parsePages() (map[string]*template.Template, error) {
	layout, err := template.New("layout.gohtml").Funcs(funcMap()).ParseFS(files, "templates/layout.gohtml")
	if err != nil {
		return nil, fmt.Errorf("parsing the console layout: %w", err)
	}

	pages := make(map[string]*template.Template, len(pageNames))
	for _, name := range pageNames {
		clone, err := layout.Clone()
		if err != nil {
			return nil, fmt.Errorf("cloning the console layout for the page %s: %w", name, err)
		}
		parsed, err := clone.ParseFS(files, "templates/"+name+".gohtml")
		if err != nil {
			return nil, fmt.Errorf("parsing the console page %s: %w", name, err)
		}
		pages[name] = parsed
	}
	return pages, nil
}

// page is what a template is executed with: the heading the layout prints,
// where the page's numbers came from, and the page's own data.
type page struct {
	Title   string
	Sources []Source
	Data    any
}

// render writes one page. The template is executed into a buffer first: a
// template failing halfway would otherwise leave a truncated body behind a 200
// status, and a reader cannot tell such a page from a complete one.
//
// The request is needed for the failure path, which logs the path it was
// serving.
func (h *handlers) render(w http.ResponseWriter, r *http.Request, name string, p page) {
	tpl, ok := h.pages[name]
	if !ok {
		h.fail(w, r, http.StatusInternalServerError, "the page could not be rendered",
			fmt.Errorf("no template is parsed for the page %s", name), p.Sources)
		return
	}

	var body bytes.Buffer
	if err := tpl.ExecuteTemplate(&body, "layout", p); err != nil {
		h.fail(w, r, http.StatusServiceUnavailable, "the page could not be rendered",
			fmt.Errorf("rendering the page %s: %w", name, err), p.Sources)
		return
	}

	w.Header().Set("Content-Type", htmlContentType)
	w.WriteHeader(http.StatusOK)
	_, _ = body.WriteTo(w)
}
