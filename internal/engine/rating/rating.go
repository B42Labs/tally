// Package rating turns the usage drafts of a billing period into amounts: one
// per draft and dimension of the pricing entry its resource type carries. Rate
// is a pure function. It reads nothing and writes nothing, so a period is rated
// from the drafts a caller already holds and the model version that caller
// resolved.
//
// The arithmetic follows roadmap/00-conventions.md section 6. Values are
// decimals throughout, the one division is money.Div, and the one rounding is
// money.Round2, applied per dimension per record. Every aggregate a later pass
// builds is a sum of those rounded amounts, so a total equals the sum of the
// line items shown beside it.
//
// A resource type the model does not price is not billed as free. The resource
// is skipped and its type is counted in Result.Unpriced, which reaches an
// operator as a warning in the run's stats. A usage field that holds a value
// nothing can be read as a number is counted in Result.Unreadable the same way:
// it is billed as zero, and a line item short by a whole metric has to be
// visible rather than pass for a resource that consumed nothing.
//
// The normative specification is roadmap/03-phase-3-metering-rating.md, WP 3.6.
package rating

import (
	"cmp"
	"encoding/json"
	"slices"
	"strconv"

	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/core/money"
	"github.com/b42labs/tally/internal/engine/metering"
	"github.com/b42labs/tally/internal/engine/pricing"
	"github.com/b42labs/tally/internal/engine/source"
)

// The two usage fields rating reads by name rather than because a dimension
// prices them: the minutes a time_gauge dimension is billed over, which
// metering writes into every draft under usageMinutes in
// internal/engine/metering/metering.go, and the size field whose value selects
// a type modifier. Renaming either one there renames it here.
const (
	usageMinutes = "minutes"
	usageType    = "type"
)

var (
	// minutesPerHour converts a draft's minutes into the hours a price per unit
	// and hour is quoted in.
	minutesPerHour = decimal.NewFromInt(60)
	// unmodified is what a state or type the pricing entry does not name is
	// billed with.
	unmodified = decimal.NewFromInt(1)
)

// Result is what one rating pass produced: the amounts of every priced
// resource, the resource types no price was found for, and the usage fields
// that held a value no quantity could be read from.
type Result struct {
	// Currency is the currency of the model the amounts were rated with. A
	// model version carries one currency, so every amount here is in it.
	Currency string
	// Resources holds one entry per priced resource, in the order the resources
	// came in. A resource whose type is not priced is not among them.
	Resources []ResourceRating
	// Unpriced counts the skipped resources per platform and resource type,
	// sorted by platform and then by resource type.
	Unpriced []UnpricedResourceType
	// Unreadable counts the drafts whose usage held an unreadable value, per
	// platform, resource type, and field, sorted the same way.
	Unreadable []UnreadableQuantity
}

// ResourceRating is one resource and what its drafts are billed as. Records is
// index-aligned with the drafts it was rated from: Records[i] rates Drafts[i].
type ResourceRating struct {
	Resource source.Resource
	Records  []RecordRating
}

// RecordRating is one usage draft rated: one amount per dimension of the
// resource type's pricing entry, in the order the entry lists them.
type RecordRating struct {
	Amounts []DimensionAmount
	// StateModifier is what the draft's state was billed at, and one where the
	// entry names no modifier for it, rounded to the four places a quantity is
	// rendered at. It is held per record however few dimensions it reached: only
	// a time_gauge dimension is multiplied by it, while a statement shows the
	// modifier of the period it rated.
	StateModifier decimal.Decimal
}

// DimensionAmount is what one dimension of one record costs, rounded with
// money.Round2. A dimension whose metric the record carries no quantity for is
// billed as zero and still emitted, so a record shows every dimension it was
// held against.
type DimensionAmount struct {
	Metric string
	Amount decimal.Decimal
	// Quantity is what the amount was rated from: the value the record carried
	// under Metric rounded to four places, and zero where it carried none, a
	// null, or one no quantity reads from. It is not the amount over the price.
	// Hours and both modifiers stand between the two on a time_gauge dimension.
	Quantity decimal.Decimal
}

// UnpricedResourceType is a resource type the model does not price, and how
// many resources of it were skipped. Count counts resources, not their drafts.
// The run writes the slice into runs.stats.unpriced, which is what the JSON
// tags name the fields for.
type UnpricedResourceType struct {
	Platform     string `json:"platform"`
	ResourceType string `json:"resource_type"`
	Count        int    `json:"count"`
}

// UnreadableQuantity is a usage field a draft held a value under that no
// quantity could be read from: a size a collector sent as a flavor name, say,
// which the size schema of its resource type lets through. The field is billed
// the way an absent one is, at zero, so the run names it rather than leave an
// invoice short by a whole line with nothing to say so. Count counts drafts,
// not the dimensions the field was read for. The run writes the slice into
// runs.stats.unreadable, which is what the JSON tags name the fields for.
type UnreadableQuantity struct {
	Platform     string `json:"platform"`
	ResourceType string `json:"resource_type"`
	Field        string `json:"field"`
	Count        int    `json:"count"`
}

// unpricedKey is the pair Unpriced counts by while the pass runs.
type unpricedKey struct {
	platform     string
	resourceType string
}

// unreadableKey is the triple Unreadable counts by while the pass runs.
type unreadableKey struct {
	platform     string
	resourceType string
	field        string
}

// Rate rates every resource against the model. It returns no error: a resource
// type the model does not price is reported in Result.Unpriced, a value no
// quantity reads from in Result.Unreadable, and a quantity a draft does not
// carry at all is billed as zero, which are the three things that can still be
// missing once a model has been imported.
//
// State and type modifiers apply to time_gauge dimensions only. A counter
// dimension is the measured quantity times the price of a unit, whatever state
// the record was in: a gigabyte of egress costs what it costs, however the
// resource it left was running.
func Rate(model pricing.Model, resources []metering.ResourceUsage) Result {
	result := Result{Currency: model.Currency}
	unpriced := make(map[unpricedKey]int)
	unreadable := make(map[unreadableKey]int)

	for _, resource := range resources {
		entry, ok := model.Pricing[resource.Resource.Platform][resource.Resource.ResourceType]
		if !ok {
			unpriced[unpricedKey{resource.Resource.Platform, resource.Resource.ResourceType}]++
			continue
		}

		records := make([]RecordRating, 0, len(resource.Drafts))
		for _, draft := range resource.Drafts {
			rated, fields := rateDraft(entry, draft)
			records = append(records, rated)
			for _, field := range fields {
				unreadable[unreadableKey{
					resource.Resource.Platform, resource.Resource.ResourceType, field,
				}]++
			}
		}
		result.Resources = append(result.Resources, ResourceRating{
			Resource: resource.Resource,
			Records:  records,
		})
	}

	for key, count := range unpriced {
		result.Unpriced = append(result.Unpriced, UnpricedResourceType{
			Platform:     key.platform,
			ResourceType: key.resourceType,
			Count:        count,
		})
	}
	slices.SortFunc(result.Unpriced, func(a, b UnpricedResourceType) int {
		return cmp.Or(
			cmp.Compare(a.Platform, b.Platform),
			cmp.Compare(a.ResourceType, b.ResourceType),
		)
	})

	for key, count := range unreadable {
		result.Unreadable = append(result.Unreadable, UnreadableQuantity{
			Platform:     key.platform,
			ResourceType: key.resourceType,
			Field:        key.field,
			Count:        count,
		})
	}
	slices.SortFunc(result.Unreadable, func(a, b UnreadableQuantity) int {
		return cmp.Or(
			cmp.Compare(a.Platform, b.Platform),
			cmp.Compare(a.ResourceType, b.ResourceType),
			cmp.Compare(a.Field, b.Field),
		)
	})

	return result
}

// rateDraft rates one draft against the pricing entry of its resource type, in
// the order the entry lists its dimensions. Beside the rating it returns the
// usage fields it found a value under that it could not read, each named once
// however many dimensions were rated from it.
func rateDraft(entry pricing.ResourcePricing, draft metering.UsageDraft) (RecordRating, []string) {
	var unreadable []string
	note := func(field string) {
		if !slices.Contains(unreadable, field) {
			unreadable = append(unreadable, field)
		}
	}

	// The statement of WP 3.7 shows this modifier for the period, at the four
	// places a quantity is rendered at, so it is rounded to that scale before
	// anything is billed at it: what a customer reads off the line is what the
	// line was computed with. The type modifier reaches no document and stays as
	// the operator wrote it.
	stateModifier := money.RoundQuantity(modifierOr1(entry.StateModifiers, draft.State))
	typeModifier := unmodified
	// A size field named type is what the type modifiers of volumes and ionos
	// servers key on. A resource that carries no type at all has none to be
	// modified by; one that carries a type nothing reads as a name is billed
	// unmodified, at a price the operator wrote for no type, so the run names
	// the field.
	if value, held := draft.Usage[usageType]; held && value != nil {
		if name, isName := value.(string); isName {
			typeModifier = modifierOr1(entry.TypeModifiers, name)
		} else {
			note(usageType)
		}
	}

	amounts := make([]DimensionAmount, 0, len(entry.Dimensions))
	for _, dimension := range entry.Dimensions {
		quantity, readable := QuantityOf(draft.Usage, dimension.Metric)
		if !readable {
			note(dimension.Metric)
		}
		// A usage quantity is a four-place value (roadmap/00-conventions.md
		// section 6) and the statement renders it as one. Rounding it here rather
		// than where it is rendered is what keeps the amount computed from the
		// quantity the document shows: a collector reporting 10.00005 GB is
		// billed for the 10.0001 the line prints, not for a number the line does
		// not carry.
		quantity = money.RoundQuantity(quantity)

		// A dimension type the schema does not allow cannot reach here, and
		// would be billed as zero rather than at a price it does not carry.
		var cost decimal.Decimal
		switch dimension.Type {
		case pricing.TypeTimeGauge:
			minutes, readable := QuantityOf(draft.Usage, usageMinutes)
			if !readable {
				note(usageMinutes)
			}
			hours := money.Div(minutes, minutesPerHour)
			cost = hours.Mul(quantity).
				Mul(dimension.PricePerUnitHour).
				Mul(stateModifier).
				Mul(typeModifier)
		case pricing.TypeCounter:
			cost = quantity.Mul(dimension.PricePerUnit)
		}

		amounts = append(amounts, DimensionAmount{
			Metric:   dimension.Metric,
			Amount:   money.Round2(cost),
			Quantity: quantity,
		})
	}
	return RecordRating{Amounts: amounts, StateModifier: stateModifier}, unreadable
}

// modifierOr1 is the modifier a key carries, or one where the entry names
// neither that key nor any modifier at all.
func modifierOr1(modifiers map[string]decimal.Decimal, key string) decimal.Decimal {
	if modifier, ok := modifiers[key]; ok {
		return modifier
	}
	return unmodified
}

// QuantityOf reads one quantity out of a draft's usage map. The map holds what
// metering put there, which is the quantities the engine derived beside the
// size fields the payload envelope decoded, so a JSON number arrives as a
// float64. A float never reaches decimal.NewFromFloat: it goes through the
// shortest text that round-trips it, which is the text encoding/json writes the
// same value back out as.
//
// A string holding digits is a quantity too. It is the spelling a collector
// reaches for when a size outgrows the range a float64 holds exactly, and a
// size schema that says nothing about the field, which is what the shipped ones
// do, lets it through. A price is accepted in both spellings for the same
// reason, so a quantity spelled that way is read rather than billed at zero.
//
// A metric the map does not hold, a metric it holds a null under, and a nil map
// are zero and readable: nothing was reported, and the dimension is rated at
// 0.00. A value nothing reads a number from, a flavor name or a boolean, is
// zero and unreadable, which is what tells a resource that consumed nothing
// from one whose quantity arrived in a form nothing bills from.
//
// The export reads a stored usage object back through this function, so the
// quantity a CSV shows is the quantity its amount was rated from.
func QuantityOf(usage map[string]any, metric string) (decimal.Decimal, bool) {
	switch value := usage[metric].(type) {
	case money.Quantity:
		return value.Decimal, true
	case decimal.Decimal:
		return value, true
	case int:
		return decimal.NewFromInt(int64(value)), true
	case int64:
		return decimal.NewFromInt(value), true
	case json.Number:
		return fromText(value.String())
	case float64:
		return fromText(strconv.FormatFloat(value, 'g', -1, 64))
	case string:
		return fromText(value)
	case nil:
		return decimal.Decimal{}, true
	default:
		return decimal.Decimal{}, false
	}
}

// The bounds fromText holds a quantity read from text to. Neither is reached by
// a size a collector reports: a byte count of the largest volume anyone sells
// is twenty digits and carries no exponent at all.
//
// A decimal is a big.Int and an exponent, so 1e2000000000 parses cheaply and
// materialises 10^2000000000 the first time it is rounded, and 1e-2147483648
// panics rather than round at all, because multiplying it underflows the int32
// the exponent is held in. Rate returns no error and recovers from nothing, so
// the ingested payload the quantity came from would crash every run over its
// period until someone deleted the event by hand. The bound belongs where the
// text is read rather than where the arithmetic runs.
const (
	maxQuantityExponent = 1000
	maxQuantityText     = 64
)

// fromText parses the text of a quantity. Text neither spelling produces is
// zero and unreadable, and so is text that spells a quantity past the bounds
// above: a quantity that cannot be read is not one to bill at a guess, and not
// one to pass off as absent either.
func fromText(text string) (decimal.Decimal, bool) {
	if len(text) > maxQuantityText {
		return decimal.Decimal{}, false
	}
	value, err := decimal.NewFromString(text)
	if err != nil {
		return decimal.Decimal{}, false
	}
	if exponent := value.Exponent(); exponent > maxQuantityExponent || exponent < -maxQuantityExponent {
		return decimal.Decimal{}, false
	}
	return value, true
}
