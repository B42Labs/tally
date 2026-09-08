package httpui

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/console/store"
	"github.com/b42labs/tally/internal/engine/pricing"
)

// pricingData is the list of imported catalog versions.
type pricingData struct {
	Models []pricingRow
}

// pricingRow is one version and the catalog it opens.
type pricingRow struct {
	Model store.PricingModel
	Link  string
}

// catalogData is one catalog version, one row per priced dimension.
type catalogData struct {
	Version    string
	ValidFrom  time.Time
	Currency   string
	ImportedAt time.Time
	Rows       []dimensionRow
}

// dimensionRow is one priced metric of one resource type with the modifiers of
// the entry it belongs to.
type dimensionRow struct {
	Platform       string
	ResourceType   string
	Metric         string
	Type           string
	Price          decimal.Decimal
	StateModifiers []modifier
	TypeModifiers  []modifier
}

// modifier is one factor a state or a type is billed with.
type modifier struct {
	Key   string
	Value decimal.Decimal
}

// pricing lists the imported catalog versions. The page reads the engine
// database alone: the Reporting API knows nothing about what anything costs.
func (h *handlers) pricing(w http.ResponseWriter, r *http.Request) {
	var src sources

	models, err := h.store.ListPricingModels(r.Context())
	src.query("ListPricingModels")
	if err != nil {
		h.failFrom(w, r, storeFailed(err), src)
		return
	}

	rows := make([]pricingRow, 0, len(models))
	for _, model := range models {
		rows = append(rows, pricingRow{Model: model, Link: link("/catalog", "version", model.Version)})
	}
	h.render(w, r, "pricing", page{
		Title:   "Pricing",
		Sources: src,
		Data:    pricingData{Models: rows},
	})
}

// catalog shows what one version prices. The stored document is parsed by the
// engine's own parser rather than read as free JSON, so the page shows the
// model a run would have rated with.
func (h *handlers) catalog(w http.ResponseWriter, r *http.Request) {
	var src sources

	version, err := stringParameter(r, "version")
	if err != nil {
		h.failFrom(w, r, err, src)
		return
	}

	stored, err := h.store.GetPricingModel(r.Context(), version)
	src.query("GetPricingModel")
	if err != nil {
		h.failFrom(w, r, storeFailed(err), src)
		return
	}

	model, err := pricing.ParseDocument(stored.Document)
	if err != nil {
		h.failFrom(w, r, documentFailed(fmt.Errorf("reading the pricing catalog %s: %w", version, err)), src)
		return
	}

	data := catalogData{
		Version:    stored.Version,
		ValidFrom:  stored.ValidFrom,
		Currency:   stored.Currency,
		ImportedAt: stored.ImportedAt,
		Rows:       dimensionRows(model),
	}
	h.render(w, r, "catalog", page{
		Title:   "Catalog " + stored.Version,
		Sources: src,
		Data:    data,
	})
}

// dimensionRows flattens a model into the table the page prints. Platforms and
// resource types are walked in sorted order, because a map hands them over in
// none and a catalog that reorders itself between two loads is unreadable.
func dimensionRows(model pricing.Model) []dimensionRow {
	var rows []dimensionRow

	for _, platform := range slices.Sorted(maps.Keys(model.Pricing)) {
		types := model.Pricing[platform]
		for _, resourceType := range slices.Sorted(maps.Keys(types)) {
			entry := types[resourceType]
			for _, dimension := range entry.Dimensions {
				rows = append(rows, dimensionRow{
					Platform:       platform,
					ResourceType:   resourceType,
					Metric:         dimension.Metric,
					Type:           dimension.Type,
					Price:          dimensionPrice(dimension),
					StateModifiers: modifiers(entry.StateModifiers),
					TypeModifiers:  modifiers(entry.TypeModifiers),
				})
			}
		}
	}
	return rows
}

// dimensionPrice picks the price the dimension's type carries. The other one is
// zero, and printing it would put a price of nothing next to every counter.
func dimensionPrice(dimension pricing.Dimension) decimal.Decimal {
	if dimension.Type == pricing.TypeCounter {
		return dimension.PricePerUnit
	}
	return dimension.PricePerUnitHour
}

// modifiers sorts a modifier map by key. An entry that sets none renders as
// nothing at all, which is what the empty slice makes the template print.
func modifiers(values map[string]decimal.Decimal) []modifier {
	list := make([]modifier, 0, len(values))
	for _, key := range slices.Sorted(maps.Keys(values)) {
		list = append(list, modifier{Key: key, Value: values[key]})
	}
	return list
}
