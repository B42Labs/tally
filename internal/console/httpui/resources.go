package httpui

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/b42labs/tally/internal/console/reporting"
	"github.com/b42labs/tally/internal/console/store"
	"github.com/b42labs/tally/internal/reporting/httpapi"
)

// resourcesData is one page of the projection with the link to the next one.
type resourcesData struct {
	Items    []resourceRow
	NextLink string
}

// resourceRow is one resource and its own page.
type resourceRow struct {
	Resource httpapi.Resource
	Link     string
}

// resourceData is one resource from both sides: what its events made of it, and
// what a run metered and rated it at.
type resourceData struct {
	Cloud    string
	Type     string
	ID       string
	Resource httpapi.Resource
	Warnings []string
	Events   []httpapi.StoredEvent
	Timeline timeline
	// Metered is false for a resource no run holds usage for, which every
	// resource is until the first run of its period has passed over it.
	Metered     bool
	Run         store.Run
	Segments    []store.Segment
	CatalogLink string
}

// resources lists one page of the projection.
func (h *handlers) resources(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var src sources

	parameters := r.URL.Query()
	query := reporting.ResourcesQuery{
		Cloud:        parameters.Get("cloud"),
		ProjectID:    parameters.Get("project_id"),
		ResourceType: parameters.Get("resource_type"),
		State:        parameters.Get("state"),
		Status:       parameters.Get("status"),
		Cursor:       parameters.Get("cursor"),
	}

	list, request, err := h.api.ListResources(ctx, query)
	src.api(request)
	if err != nil {
		h.failFrom(w, r, apiFailed(err), src)
		return
	}

	rows := make([]resourceRow, 0, len(list.Items))
	for _, resource := range list.Items {
		rows = append(rows, resourceRow{
			Resource: resource,
			Link: link("/resource",
				"cloud", resource.Cloud, "type", resource.ResourceType, "id", resource.ResourceId),
		})
	}

	data := resourcesData{Items: rows}
	if list.NextCursor != nil {
		data.NextLink = link("/resources",
			"cloud", query.Cloud,
			"project_id", query.ProjectID,
			"resource_type", query.ResourceType,
			"state", query.State,
			"status", query.Status,
			"cursor", *list.NextCursor)
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
		Cloud:    cloud,
		Type:     resourceType,
		ID:       resourceID,
		Resource: lifecycle.Resource,
		Warnings: lifecycle.Warnings,
		Events:   lifecycle.Events,
		Metered:  metered,
	}

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
		data.Segments = segments
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
