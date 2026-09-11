// Package httpui is the demo console's web layer: the router, the handlers, the
// templates they render, and the one stylesheet those templates load.
//
// Every page is HTML the handler wrote. The console ships no JavaScript, so no
// page carries a script element or an event handler attribute, and it loads
// exactly one asset, /static/console.css. What a viewer sees is what the
// handler rendered, and a page that shows a number read that number itself.
// Sorting and filtering a table are links and a form that reload the page
// with the table's state in the query string, and the theme is a form that
// posts the choice to /theme, which is not a page: it keeps the choice in a
// cookie and sends the viewer back. /statement.json, /run.json and
// /kickbacks.json are not pages either: they serve the files the JSON export
// writes for a statement and for a run, as the export's own bytes rather than
// as HTML.
//
// Every identifier travels in a query parameter rather than in a path segment.
// A cloud name, a resource id and a statement key may each carry a slash, and a
// path segment would either split such an identifier in two or hide it behind
// an escape that a proxy in front of the console is free to normalize away.
//
// Nothing here writes to either side. The pages read the Reporting API for
// projects, resources, lifecycles, stats and refused events, and the engine
// database for everything monetary. A failure of either side reaches the
// viewer as one error page naming what failed, and the same wrapped error goes
// to the log.
package httpui

import (
	"context"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/b42labs/tally/internal/console/reporting"
	"github.com/b42labs/tally/internal/console/store"
	"github.com/b42labs/tally/internal/engine/export"
	"github.com/b42labs/tally/internal/reporting/httpapi"
)

// Options is what the console needs to serve its pages.
type Options struct {
	// Logger takes one line per failed page. A nil logger falls back to
	// slog.Default().
	Logger *slog.Logger
	// API reads the Reporting API and Store the engine database. The console
	// serves no page without both.
	API   API
	Store Store
	// Now is the clock the windowed reads are built from: the overview's event
	// window and the summary month of a project page. A nil Now is time.Now,
	// and a test pins it to a fixed instant.
	Now func() time.Time
}

// API is the part of the Reporting API client the pages call. It is declared
// here rather than taken from the client package so a test can serve a page
// from canned answers, and *reporting.Client satisfies it as it is.
type API interface {
	ListProjects(ctx context.Context, q reporting.ProjectsQuery) (httpapi.ProjectList, reporting.Request, error)
	GetProject(ctx context.Context, id uuid.UUID) (httpapi.Project, reporting.Request, error)
	ListProjectRelations(
		ctx context.Context, id uuid.UUID, q reporting.RelationsQuery,
	) (httpapi.RelationList, reporting.Request, error)
	ListRelatedProjects(ctx context.Context, id uuid.UUID) (httpapi.RelatedProjectList, reporting.Request, error)
	GetProjectSummary(
		ctx context.Context, id uuid.UUID, from, to time.Time,
	) (httpapi.ProjectSummary, reporting.Request, error)
	ListResources(ctx context.Context, q reporting.ResourcesQuery) (httpapi.ResourceList, reporting.Request, error)
	GetLifecycle(
		ctx context.Context, cloud, resourceType, resourceID string,
	) (httpapi.Lifecycle, reporting.Request, error)
	ResourceStats(ctx context.Context, groupBy []string, status string) (httpapi.ResourceStatsList, reporting.Request, error)
	EventStats(
		ctx context.Context, groupBy []string, interval string, from, to time.Time,
	) (httpapi.EventStatsList, reporting.Request, error)
	ListRejectedEvents(ctx context.Context, limit int) (httpapi.DeadLetterList, reporting.Request, error)
}

// Store is the part of the engine read store the pages call, declared here for
// the reason API is. *store.Store satisfies it as it is.
type Store interface {
	ListPeriods(ctx context.Context) ([]store.Period, error)
	GetPeriod(ctx context.Context, from time.Time) (store.Period, error)
	ListRunsForPeriod(ctx context.Context, from time.Time) ([]store.Run, error)
	ListRunTotalsForPeriod(ctx context.Context, from time.Time) ([]store.RunTotal, error)
	ListRuns(ctx context.Context, limit int32) ([]store.Run, error)
	GetRun(ctx context.Context, id uuid.UUID) (store.Run, error)
	ListStatements(ctx context.Context, runID uuid.UUID) ([]store.StatementRow, error)
	GetStatement(ctx context.Context, runID uuid.UUID, key string) (store.Statement, error)
	ListStatementsForProject(ctx context.Context, key string) ([]store.ProjectStatementRow, error)
	ListKickbacksForBeneficiary(ctx context.Context, beneficiary string) ([]store.Kickback, error)
	ListPricingModels(ctx context.Context) ([]store.PricingModel, error)
	GetPricingModel(ctx context.Context, version string) (store.PricingDocument, error)
	LatestRunWithResource(ctx context.Context, cloud, resourceType, resourceID string) (uuid.UUID, bool, error)
	ListResourceSegments(
		ctx context.Context, runID uuid.UUID, cloud, resourceType, resourceID string,
	) ([]store.Segment, error)
	ListCorrectionDeltas(ctx context.Context, runID uuid.UUID) ([]store.Delta, error)
	LoadRunExport(ctx context.Context, runID uuid.UUID) (export.Run, error)
}

// handlers is what every route is served from: the two read seams, the clock,
// and the parsed pages.
type handlers struct {
	logger *slog.Logger
	api    API
	store  Store
	now    func() time.Time
	pages  map[string]*template.Template
}

// NewRouter parses the pages and assembles the console. It fails when a
// template does not parse, which keeps a broken build from starting a server
// whose pages only fail once someone opens them.
func NewRouter(opts Options) (http.Handler, error) {
	pages, err := parsePages()
	if err != nil {
		return nil, err
	}

	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	h := &handlers{logger: logger, api: opts.API, store: opts.Store, now: now, pages: pages}

	r := chi.NewRouter()
	// A panic is the one failure that does not reach fail, and a demo console
	// answering nothing at all is worse than one answering 500.
	r.Use(middleware.Recoverer)

	r.Get("/", h.overview)
	r.Get("/projects", h.projects)
	r.Get("/project", h.project)
	r.Get("/resources", h.resources)
	r.Get("/resource", h.resource)
	r.Get("/pricing", h.pricing)
	r.Get("/catalog", h.catalog)
	r.Get("/period", h.billingPeriod)
	r.Get("/run", h.run)
	r.Get("/statement", h.statement)
	r.Get(exportRoute, h.statementExport)
	r.Get(runFileRoute, h.runFile)
	r.Get(kickbacksFileRoute, h.kickbacksFile)
	r.Get("/static/console.css", h.stylesheet)
	r.Post(themeRoute, h.theme)
	r.NotFound(h.notFound)
	r.MethodNotAllowed(h.methodNotAllowed)

	return r, nil
}

// stylesheet serves the console's only asset out of the binary.
func (h *handlers) stylesheet(w http.ResponseWriter, r *http.Request) {
	css, err := files.ReadFile(stylesheetPath)
	if err != nil {
		h.fail(w, r, http.StatusInternalServerError, "the stylesheet could not be read", err, nil)
		return
	}

	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	_, _ = w.Write(css)
}

// notFound answers a path the console has no page for. It answers as an error
// page rather than as bare text so that a mistyped URL still lands on the
// console's own navigation.
func (h *handlers) notFound(w http.ResponseWriter, r *http.Request) {
	h.fail(w, r, http.StatusNotFound, "no such page",
		fmt.Errorf("the path %s is not part of the console", r.URL.Path), nil)
}

// methodNotAllowed answers a route asked with the wrong method, which is the
// theme route fetched rather than posted. It lands on the error page for the
// reason notFound does.
func (h *handlers) methodNotAllowed(w http.ResponseWriter, r *http.Request) {
	h.fail(w, r, http.StatusMethodNotAllowed, "the method is not allowed",
		fmt.Errorf("the path %s does not answer %s", r.URL.Path, r.Method), nil)
}

// link builds one href. The pairs are parameter names and values, encoded once
// here rather than by every template that prints a link, so a value carrying a
// slash, a space or an ampersand reaches the console as it was stored. A pair
// whose value is empty is left out: a filter nobody set does not travel.
func link(path string, pairs ...string) string {
	query := url.Values{}
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i+1] != "" {
			query.Set(pairs[i], pairs[i+1])
		}
	}
	if len(query) == 0 {
		return path
	}
	return path + "?" + query.Encode()
}

// stringParameter reads a query parameter a page cannot be built without.
func stringParameter(r *http.Request, name string) (string, error) {
	value := r.URL.Query().Get(name)
	if value == "" {
		return "", missingParameter(name)
	}
	return value, nil
}

// uuidParameter reads an id a page cannot be built without.
func uuidParameter(r *http.Request, name string) (uuid.UUID, error) {
	value, err := stringParameter(r, name)
	if err != nil {
		return uuid.Nil, err
	}
	id, err := uuid.Parse(value)
	if err != nil {
		return uuid.Nil, unreadableUUID(name, err)
	}
	return id, nil
}

// idText renders an id the store reads a NULL column as, the corrected run of a
// regular run for example.
func idText(id uuid.UUID) string {
	if id == uuid.Nil {
		return absent
	}
	return id.String()
}
