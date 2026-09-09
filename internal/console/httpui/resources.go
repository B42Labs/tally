package httpui

import (
	"net/http"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/console/reporting"
	"github.com/b42labs/tally/internal/console/store"
	"github.com/b42labs/tally/internal/reporting/httpapi"
)

// resourcesData is one page of the projection: the fleet controls above it,
// the rows the instant left of it, and the link to the next page.
type resourcesData struct {
	Fleet    fleetView
	Items    listing[resourceRow]
	NextLink string
}

// resourceColumns is the projection listing. The payload column is searched
// as the text the page prints it as, so a filter finds a value inside it.
var resourceColumns = []column[resourceRow]{
	textCol("cloud", func(r resourceRow) string { return r.Resource.Cloud }),
	textCol("platform", func(r resourceRow) string { return r.Resource.Platform }),
	textCol("project", func(r resourceRow) string { return r.Resource.ProjectId }),
	textCol("resource type", func(r resourceRow) string { return r.Resource.ResourceType }),
	textCol("resource", func(r resourceRow) string { return r.Resource.ResourceId }),
	textCol("state", func(r resourceRow) string { return r.Resource.State }),
	textCol("created", func(r resourceRow) string { return unknownStamp(r.Resource.CreatedAt) }),
	textCol("deleted", func(r resourceRow) string { return optStamp(r.Resource.DeletedAt) }),
	numberCol("lifetime hours", func(r resourceRow) decimal.Decimal { return r.Lifetime }),
	textCol("last payload", func(r resourceRow) string { return pretty(r.Resource.LastPayload) }),
}

// resourceRow is one resource, its own page, the page of its project, how
// long it has lived, and what its folded payload says.
type resourceRow struct {
	Resource    httpapi.Resource
	Link        string
	ProjectLink string
	// Lifetime is the resource's hours from creation to deletion or to now,
	// and Lived is false for a resource whose history shows no create.
	Lifetime       decimal.Decimal
	Lived          bool
	PayloadSummary string
}

// resourceData is one resource from both sides: what its events made of it, and
// what a run metered and rated it at.
type resourceData struct {
	Cloud       string
	Type        string
	ID          string
	Resource    httpapi.Resource
	ProjectLink string
	// Created and Lifetime are the two lines the head prints for a history
	// with or without a create.
	Created  string
	Lifetime string
	Warnings []string
	Events   listing[httpapi.StoredEvent]
	Timeline timeline
	// Metered is false for a resource no run holds usage for, which every
	// resource is until the first run of its period has passed over it.
	Metered     bool
	Run         store.Run
	Segments    listing[store.Segment]
	CatalogLink string
}

// The columns of a resource page's tables.
var (
	segmentColumns = []column[store.Segment]{
		textCol("state", func(r store.Segment) string { return r.State }),
		textCol("from", func(r store.Segment) string { return stamp(r.From) }),
		textCol("to", func(r store.Segment) string { return stamp(r.To) }),
		countCol("seconds", func(r store.Segment) int64 { return r.Seconds }),
		textCol("dimension", func(r store.Segment) string { return r.Dimension }),
		numberCol("amount", func(r store.Segment) decimal.Decimal { return r.Amount }),
		textCol("currency", func(r store.Segment) string { return r.Currency }),
	}
	storedEventColumns = []column[httpapi.StoredEvent]{
		textCol("timestamp", func(r httpapi.StoredEvent) string { return stamp(r.Timestamp) }),
		textCol("event type", func(r httpapi.StoredEvent) string { return r.EventType }),
		textCol("source", func(r httpapi.StoredEvent) string { return r.Source }),
		textCol("project", func(r httpapi.StoredEvent) string { return r.ProjectId }),
	}
)

// resources lists one page of the projection under the status the viewer
// chose, and of that page the rows the instant or the window it is read at
// keeps. The page is the widest the API serves, so the filters are applied to
// as much of the fleet as one call can hold.
func (h *handlers) resources(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var src sources

	now := h.now()
	fleet, err := readFleet(r, now)
	if err != nil {
		h.failFrom(w, r, err, src)
		return
	}

	parameters := r.URL.Query()
	query := reporting.ResourcesQuery{
		Cloud:        parameters.Get("cloud"),
		ProjectID:    parameters.Get("project_id"),
		ResourceType: parameters.Get("resource_type"),
		State:        parameters.Get("state"),
		Status:       fleet.Status,
		Cursor:       parameters.Get("cursor"),
		Limit:        resourcePageLimit,
	}

	list, request, err := h.api.ListResources(ctx, query)
	src.api(request)
	if err != nil {
		h.failFrom(w, r, apiFailed(err), src)
		return
	}

	rows := make([]resourceRow, 0, len(list.Items))
	unknown := 0
	for _, resource := range list.Items {
		if !keep(resource, fleet) {
			continue
		}
		lifetime, lived := lifetimeHours(resource, now)
		if !lived {
			unknown++
		}
		rows = append(rows, resourceRow{
			Resource: resource,
			Link: link("/resource",
				"cloud", resource.Cloud, "type", resource.ResourceType, "id", resource.ResourceId),
			ProjectLink:    projectLink(resource.Cloud, resource.ProjectId),
			Lifetime:       lifetime,
			Lived:          lived,
			PayloadSummary: payloadSummary(resource.LastPayload),
		})
	}

	// A page is one of several only when the API said so, or when it was
	// reached by a cursor: a fleet that fits one page is the whole fleet.
	paged := list.NextCursor != nil || query.Cursor != ""
	data := resourcesData{
		Fleet: buildFleetView(r, fleet, now, len(rows), len(list.Items), unknown, paged),
		Items: tabulate(r, resourceTable, resourceColumns, rows),
	}
	if paged {
		data.Items = data.Items.paged()
	}
	if list.NextCursor != nil {
		data.NextLink = nextLink(r, *list.NextCursor)
	}
	h.render(w, r, "resources", page{Title: "Resources", Sources: src, Data: data})
}

// resource shows one resource over time: the lifecycle the API folded from its
// events above what a run metered for it. The run is the newest one holding
// usage for the resource unless the viewer named another one, which is how the
// page of an older run is reached.
func (h *handlers) resource(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var src sources

	cloud, err := stringParameter(r, "cloud")
	if err != nil {
		h.failFrom(w, r, err, src)
		return
	}
	resourceType, err := stringParameter(r, "type")
	if err != nil {
		h.failFrom(w, r, err, src)
		return
	}
	resourceID, err := stringParameter(r, "id")
	if err != nil {
		h.failFrom(w, r, err, src)
		return
	}

	runID := uuid.Nil
	metered := false
	if asked := r.URL.Query().Get("run"); asked != "" {
		runID, err = uuid.Parse(asked)
		if err != nil {
			h.failFrom(w, r, unreadableUUID("run", err), src)
			return
		}
		metered = true
	}

	lifecycle, request, err := h.api.GetLifecycle(ctx, cloud, resourceType, resourceID)
	src.api(request)
	if err != nil {
		h.failFrom(w, r, apiFailed(err), src)
		return
	}

	if !metered {
		runID, metered, err = h.store.LatestRunWithResource(ctx, cloud, resourceType, resourceID)
		src.query("LatestRunWithResource")
		if err != nil {
			h.failFrom(w, r, storeFailed(err), src)
			return
		}
	}

	data := resourceData{
		Cloud:       cloud,
		Type:        resourceType,
		ID:          resourceID,
		Resource:    lifecycle.Resource,
		ProjectLink: projectLink(lifecycle.Resource.Cloud, lifecycle.Resource.ProjectId),
		Warnings:    lifecycle.Warnings,
		Events:      tabulate(r, "events", storedEventColumns, lifecycle.Events),
		Metered:     metered,
	}
	data.Created, data.Lifetime = lifecycleTimes(lifecycle, h.now())

	var groups []segmentGroup
	if metered {
		run, runErr := h.store.GetRun(ctx, runID)
		src.query("GetRun")
		if runErr != nil {
			h.failFrom(w, r, storeFailed(runErr), src)
			return
		}

		segments, segmentErr := h.store.ListResourceSegments(ctx, runID, cloud, resourceType, resourceID)
		src.query("ListResourceSegments")
		if segmentErr != nil {
			h.failFrom(w, r, storeFailed(segmentErr), src)
			return
		}

		groups = groupSegments(segments)
		data.Run = run
		data.Segments = tabulate(r, "segments", segmentColumns, segments)
		if run.PricingVersion != "" {
			data.CatalogLink = link("/catalog", "version", run.PricingVersion)
		}
	}

	data.Timeline = buildTimeline(lifecycle.Intervals, groups, h.now())
	h.render(w, r, "resource", page{
		Title:   "Resource " + resourceID,
		Sources: src,
		Data:    data,
	})
}

// groupSegments folds the rated rows of one resource into one rectangle per
// metered interval. Rating writes one row per dimension, so a resource rated on
// four dimensions carries four rows of the same interval, and the picture draws
// what they add up to.
//
// The rows are grouped by the instant an interval starts, keyed by its
// nanosecond so that two equal instants group whatever location they carry.
func groupSegments(segments []store.Segment) []segmentGroup {
	groups := make([]segmentGroup, 0, len(segments))
	at := make(map[int64]int, len(segments))

	for _, segment := range segments {
		key := segment.From.UnixNano()
		index, ok := at[key]
		if !ok {
			groups = append(groups, segmentGroup{State: segment.State, From: segment.From, To: segment.To})
			index = len(groups) - 1
			at[key] = index
		}
		groups[index].Amount = groups[index].Amount.Add(segment.Amount)
	}
	return groups
}
