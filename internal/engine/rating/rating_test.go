package rating_test

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/core/money"
	"github.com/b42labs/tally/internal/engine/metering"
	"github.com/b42labs/tally/internal/engine/pricing"
	"github.com/b42labs/tally/internal/engine/rating"
	"github.com/b42labs/tally/internal/engine/source"
)

// conceptModel is the openstack part of the model the concept gives as its
// example. The cases below parse it rather than building a pricing.Model by
// hand, so what is rated is what an operator's file yields.
const conceptModel = `version: "2026-03"
valid_from: "2026-03-01T00:00:00Z"
currency: "EUR"
pricing:
  openstack:
    instance:
      dimensions:
        - metric: "vcpus"
          type: "time_gauge"
          price_per_unit_hour: "0.02"
        - metric: "ram_gb"
          type: "time_gauge"
          price_per_unit_hour: "0.005"
        - metric: "disk_gb"
          type: "time_gauge"
          price_per_unit_hour: "0.001"
        - metric: "egress_gb"
          type: "counter"
          price_per_unit: "0.09"
      state_modifiers:
        shelved: "0.0"
        shutoff: "0.5"
    volume:
      dimensions:
        - metric: "size_gb"
          type: "time_gauge"
          price_per_unit_hour: "0.0001"
      type_modifiers:
        ssd: "1.0"
        hdd: "0.5"
    floating_ip:
      dimensions:
        - metric: "count"
          type: "time_gauge"
          price_per_unit_hour: "0.005"
`

// counterModel prices one counter metric at one unit of currency, so a rated
// amount is the quantity the usage map was read as.
const counterModel = `version: "2026-03"
valid_from: "2026-03-01T00:00:00Z"
currency: "EUR"
pricing:
  openstack:
    bucket:
      dimensions:
        - metric: "egress_gb"
          type: "counter"
          price_per_unit: "1"
`

// The resources the cases rate. Only the platform and the resource type select
// a pricing entry; the rest travels through the pass untouched.
var (
	instance   = source.Resource{Cloud: "prod", Platform: "openstack", ResourceType: "instance", ResourceID: "abc-123"}
	volume     = source.Resource{Cloud: "prod", Platform: "openstack", ResourceType: "volume", ResourceID: "vol-1"}
	floatingIP = source.Resource{Cloud: "prod", Platform: "openstack", ResourceType: "floating_ip", ResourceID: "fip-1"}
	bucket     = source.Resource{Cloud: "prod", Platform: "openstack", ResourceType: "bucket", ResourceID: "buck-1"}
)

// mustParse parses a model the case expects to hold.
func mustParse(t *testing.T, document string) pricing.Model {
	t.Helper()

	model, _, err := pricing.Parse([]byte(document))
	if err != nil {
		t.Fatalf("Parse() error = %v, want nil", err)
	}
	return model
}

// draft builds a usage draft the way metering builds one: the size fields of
// the interval, plus the minutes its seconds are worth and the count of one
// that prices a resource by its existence.
func draft(state string, minutes int64, size map[string]any) metering.UsageDraft {
	seconds := minutes * 60
	usage := map[string]any{
		"minutes": money.NewQuantity(money.Minutes(seconds)),
		"count":   1,
	}
	maps.Copy(usage, size)

	return metering.UsageDraft{
		State:     state,
		ProjectID: "proj-456",
		Seconds:   seconds,
		Usage:     usage,
	}
}

// instanceSize is the m1.large of the concept's example, spelled the way the
// payload envelope decodes a size: every JSON number is a float64.
func instanceSize(egressGB float64) map[string]any {
	return map[string]any{
		"vcpus":     float64(4),
		"ram_gb":    float64(8),
		"disk_gb":   float64(80),
		"egress_gb": egressGB,
	}
}

// wantAmount is one expected amount: the metric it is rated under, and the text
// the expected decimal is built from.
type wantAmount struct {
	metric string
	amount string
}

// assertAmounts holds a record's amounts against the dimensions and values it
// is expected to carry, in order. Decimals are compared by value, so 19.20 and
// 19.2 are the same amount.
func assertAmounts(t *testing.T, got []rating.DimensionAmount, want []wantAmount) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("amounts = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Metric != w.metric {
			t.Errorf("amount %d is the metric %q, want %q", i, got[i].Metric, w.metric)
			continue
		}
		if expected := decimal.RequireFromString(w.amount); !got[i].Amount.Equal(expected) {
			t.Errorf("%s = %s, want %s", w.metric, got[i].Amount, w.amount)
		}
	}
}

// rateOne rates the drafts of a single resource and returns its records. It
// fails the case where the resource type was not priced.
func rateOne(t *testing.T, model pricing.Model, resource source.Resource, drafts ...metering.UsageDraft) []rating.RecordRating {
	t.Helper()

	result := rating.Rate(model, []metering.ResourceUsage{{Resource: resource, Drafts: drafts}})
	if len(result.Resources) != 1 {
		t.Fatalf("Resources = %d, want 1 (unpriced: %v)", len(result.Resources), result.Unpriced)
	}
	if got := len(result.Resources[0].Records); got != len(drafts) {
		t.Fatalf("Records = %d, want %d", got, len(drafts))
	}
	return result.Resources[0].Records
}

// TestRateEndToEndExample rates the worked example of the concept: one instance
// that ran for ten days, was shut off for ten, and ran for the rest of March.
func TestRateEndToEndExample(t *testing.T) {
	model := mustParse(t, conceptModel)

	result := rating.Rate(model, []metering.ResourceUsage{{
		Resource: instance,
		Drafts: []metering.UsageDraft{
			draft("active", 14400, instanceSize(18.0)),
			draft("shutoff", 14400, instanceSize(0)),
			draft("active", 15840, instanceSize(22.5)),
		},
	}})

	if result.Currency != "EUR" {
		t.Errorf("Currency = %q, want %q", result.Currency, "EUR")
	}
	if len(result.Unpriced) != 0 {
		t.Errorf("Unpriced = %v, want none: the instance is priced", result.Unpriced)
	}
	if len(result.Resources) != 1 {
		t.Fatalf("Resources = %d, want 1", len(result.Resources))
	}

	rated := result.Resources[0]
	if rated.Resource != instance {
		t.Errorf("Resource = %+v, want %+v", rated.Resource, instance)
	}
	if len(rated.Records) != 3 {
		t.Fatalf("Records = %d, want 3, one per draft", len(rated.Records))
	}

	records := []struct {
		name string
		want []wantAmount
	}{
		{
			name: "active for 240 hours",
			want: []wantAmount{
				{"vcpus", "19.20"}, {"ram_gb", "9.60"}, {"disk_gb", "19.20"}, {"egress_gb", "1.62"},
			},
		},
		{
			name: "shut off for 240 hours, at half the time_gauge price",
			want: []wantAmount{
				{"vcpus", "9.60"}, {"ram_gb", "4.80"}, {"disk_gb", "9.60"}, {"egress_gb", "0.00"},
			},
		},
		{
			name: "active for 264 hours, the egress rounded half away from zero",
			want: []wantAmount{
				{"vcpus", "21.12"}, {"ram_gb", "10.56"}, {"disk_gb", "21.12"}, {"egress_gb", "2.03"},
			},
		},
	}
	for i, record := range records {
		t.Run(record.name, func(t *testing.T) {
			assertAmounts(t, rated.Records[i].Amounts, record.want)
		})
	}
}

func TestRateModifiers(t *testing.T) {
	model := mustParse(t, conceptModel)

	t.Run("a type modifier halves the volume it names", func(t *testing.T) {
		records := rateOne(t, model, volume,
			draft("in-use", 17280, map[string]any{"size_gb": float64(200), "type": "hdd"}))

		assertAmounts(t, records[0].Amounts, []wantAmount{{"size_gb", "2.88"}})
	})

	t.Run("a state modifier of zero rates every time_gauge dimension as zero", func(t *testing.T) {
		records := rateOne(t, model, instance, draft("shelved", 14400, instanceSize(4.0)))

		// The counter keeps its price: the state modifier is not applied to it.
		assertAmounts(t, records[0].Amounts, []wantAmount{
			{"vcpus", "0.00"}, {"ram_gb", "0.00"}, {"disk_gb", "0.00"}, {"egress_gb", "0.36"},
		})
	})

	t.Run("a floating ip is priced by the count of one", func(t *testing.T) {
		records := rateOne(t, model, floatingIP, draft("active", 44640, nil))

		assertAmounts(t, records[0].Amounts, []wantAmount{{"count", "3.72"}})
	})

	t.Run("a counter on a modified state is rated unmodified", func(t *testing.T) {
		records := rateOne(t, model, instance, draft("shutoff", 14400, instanceSize(18.0)))

		// 18.0 × 0.09 is 1.62, not the 0.81 the state modifier would make of it.
		assertAmounts(t, records[0].Amounts, []wantAmount{
			{"vcpus", "9.60"}, {"ram_gb", "4.80"}, {"disk_gb", "9.60"}, {"egress_gb", "1.62"},
		})
	})

	t.Run("a type nothing names is billed unmodified", func(t *testing.T) {
		records := rateOne(t, model, volume,
			draft("in-use", 17280, map[string]any{"size_gb": float64(200), "type": "nvme"}))

		assertAmounts(t, records[0].Amounts, []wantAmount{{"size_gb", "5.76"}})
	})
}

// TestRateEmitsEveryDimension rates records that carry no quantity a dimension
// can be billed from. Every dimension is still emitted, at 0.00 and in the
// order the pricing entry lists it, so a record shows what it was held against.
func TestRateEmitsEveryDimension(t *testing.T) {
	model := mustParse(t, conceptModel)

	free := []wantAmount{
		{"vcpus", "0.00"}, {"ram_gb", "0.00"}, {"disk_gb", "0.00"}, {"egress_gb", "0.00"},
	}

	cases := []struct {
		name  string
		draft metering.UsageDraft
	}{
		{
			name:  "no priced metric is in the usage map",
			draft: draft("active", 14400, nil),
		},
		{
			name:  "a metric carries a string",
			draft: draft("active", 14400, map[string]any{"vcpus": "m1.small"}),
		},
		{
			name:  "the usage map is nil",
			draft: metering.UsageDraft{State: "active"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			records := rateOne(t, model, instance, tc.draft)

			assertAmounts(t, records[0].Amounts, free)
		})
	}
}

// TestQuantityTypes rates one counter dimension priced at one, over every type
// a usage map holds a quantity as.
func TestQuantityTypes(t *testing.T) {
	model := mustParse(t, counterModel)

	cases := []struct {
		name  string
		value any
		want  string
	}{
		{"a money quantity", money.NewQuantity(decimal.RequireFromString("2.5")), "2.50"},
		{"a decimal", decimal.RequireFromString("2.5"), "2.50"},
		{"an int", 3, "3.00"},
		{"an int64", int64(4), "4.00"},
		{"a json number", json.Number("2.5"), "2.50"},
		{"a float64", float64(2.5), "2.50"},
		{"a float64 whose shortest text is exponent notation", float64(1e21), "1e21"},
		{"a string holding digits", "2.5", "2.50"},
		{"a string nothing reads a number from", "m1.small", "0.00"},
		// A decimal is digits and an exponent, so text is cheap to parse and
		// expensive to compute with: 1e2000000000 is four bytes of exponent and
		// two billion digits the moment the cost is rounded. The bound is held
		// at the text, and what it refuses is read the way a flavor name is.
		{"a string whose exponent no quantity carries", "1e100000", "0.00"},
		{"a string of more digits than a quantity is spelled with", strings.Repeat("9", 65), "0.00"},
		{"a type nothing computes with", true, "0.00"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			records := rateOne(t, model, bucket, metering.UsageDraft{
				State: "active",
				Usage: map[string]any{"egress_gb": tc.value},
			})

			assertAmounts(t, records[0].Amounts, []wantAmount{{"egress_gb", tc.want}})
		})
	}
}

// TestUnpriced rates resources of types the model does not price, interleaved
// with one it does.
func TestUnpriced(t *testing.T) {
	model := mustParse(t, conceptModel)

	resource := func(platform, resourceType, id string) metering.ResourceUsage {
		return metering.ResourceUsage{
			Resource: source.Resource{Cloud: "prod", Platform: platform, ResourceType: resourceType, ResourceID: id},
			Drafts:   []metering.UsageDraft{draft("active", 14400, instanceSize(1.0))},
		}
	}

	result := rating.Rate(model, []metering.ResourceUsage{
		resource("openstack", "router", "rtr-1"),
		{Resource: instance, Drafts: []metering.UsageDraft{draft("active", 14400, instanceSize(18.0))}},
		resource("hetzner", "server", "srv-1"),
		resource("openstack", "router", "rtr-2"),
		resource("openstack", "loadbalancer", "lb-1"),
	})

	if len(result.Resources) != 1 {
		t.Fatalf("Resources = %d, want 1: only the instance is priced", len(result.Resources))
	}
	if result.Resources[0].Resource != instance {
		t.Errorf("Resource = %+v, want %+v", result.Resources[0].Resource, instance)
	}

	want := []rating.UnpricedResourceType{
		{Platform: "hetzner", ResourceType: "server", Count: 1},
		{Platform: "openstack", ResourceType: "loadbalancer", Count: 1},
		{Platform: "openstack", ResourceType: "router", Count: 2},
	}
	if len(result.Unpriced) != len(want) {
		t.Fatalf("Unpriced = %v, want %v", result.Unpriced, want)
	}
	for i, w := range want {
		if result.Unpriced[i] != w {
			t.Errorf("Unpriced[%d] = %+v, want %+v", i, result.Unpriced[i], w)
		}
	}

	t.Run("the slice carries the field names the run's stats are read under", func(t *testing.T) {
		data, err := json.Marshal(result.Unpriced)
		if err != nil {
			t.Fatalf("Marshal() error = %v, want nil", err)
		}

		const document = `[{"platform":"hetzner","resource_type":"server","count":1},` +
			`{"platform":"openstack","resource_type":"loadbalancer","count":1},` +
			`{"platform":"openstack","resource_type":"router","count":2}]`
		if string(data) != document {
			t.Errorf("Marshal() = %s, want %s", data, document)
		}
	})
}

func TestRateEmptyInput(t *testing.T) {
	model := mustParse(t, conceptModel)

	cases := []struct {
		name      string
		resources []metering.ResourceUsage
	}{
		{"no resources at all", nil},
		{"an empty resource list", []metering.ResourceUsage{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := rating.Rate(model, tc.resources)

			if result.Currency != "EUR" {
				t.Errorf("Currency = %q, want %q", result.Currency, "EUR")
			}
			if len(result.Resources) != 0 {
				t.Errorf("Resources = %v, want none", result.Resources)
			}
			if len(result.Unpriced) != 0 {
				t.Errorf("Unpriced = %v, want none", result.Unpriced)
			}
		})
	}
}

// TestUnreadableQuantities rates drafts that hold a value under a field rating
// reads by name and no quantity comes out of. The field is billed the way an
// absent one is, at zero, and the pass names it: a line item short by a whole
// metric must not read as a resource that consumed nothing.
func TestUnreadableQuantities(t *testing.T) {
	model := mustParse(t, conceptModel)

	cases := []struct {
		name     string
		resource source.Resource
		draft    metering.UsageDraft
		want     []rating.UnreadableQuantity
	}{
		{
			// A collector that sends the flavor where the schema of its
			// resource type says nothing about the field. Nothing reads a
			// count of vCPUs from a name, so the dimension is billed at zero
			// and the field is named.
			name:     "a size sent as a flavor name",
			resource: instance,
			draft:    draft("active", 14400, map[string]any{"vcpus": "m1.small", "ram_gb": float64(8)}),
			want: []rating.UnreadableQuantity{
				{Platform: "openstack", ResourceType: "instance", Field: "vcpus", Count: 1},
			},
		},
		{
			// The spelling a collector reaches for when a size can outgrow the
			// range a float64 holds exactly, and the one a price is written in
			// too. The digits are the quantity, so nothing is unreadable here.
			name:     "a size sent as a JSON string",
			resource: instance,
			draft:    draft("active", 14400, map[string]any{"vcpus": "4", "ram_gb": float64(8)}),
		},
		{
			// Read once per time_gauge dimension, and the draft it was
			// unreadable in is one draft however many dimensions read it.
			name:     "the minutes the draft is billed over",
			resource: instance,
			draft: metering.UsageDraft{State: "active", Usage: map[string]any{
				"minutes": true, "vcpus": float64(4),
			}},
			want: []rating.UnreadableQuantity{
				{Platform: "openstack", ResourceType: "instance", Field: "minutes", Count: 1},
			},
		},
		{
			// The modifier stays at 1, which is a price the operator did not
			// write, so the field is named rather than passed for a volume
			// that carries no type.
			name:     "a type nothing reads as a name",
			resource: volume,
			draft:    draft("in-use", 17280, map[string]any{"size_gb": float64(200), "type": float64(2)}),
			want: []rating.UnreadableQuantity{
				{Platform: "openstack", ResourceType: "volume", Field: "type", Count: 1},
			},
		},
		{
			// A negative exponent rounds to zero, so the amount alone would not
			// tell this from a resource that consumed nothing. What the bound
			// refuses is the arithmetic behind it: an exponent near the int32 a
			// decimal holds it in panics when the hours are multiplied by it.
			name:     "a size spelled with an exponent past what a quantity carries",
			resource: instance,
			draft:    draft("active", 14400, map[string]any{"vcpus": "1e-2000"}),
			want: []rating.UnreadableQuantity{
				{Platform: "openstack", ResourceType: "instance", Field: "vcpus", Count: 1},
			},
		},
		{
			name:     "a metric the payload spelled as null",
			resource: instance,
			draft:    draft("active", 14400, map[string]any{"vcpus": nil}),
		},
		{
			name:     "a volume that carries no type at all",
			resource: volume,
			draft:    draft("in-use", 17280, map[string]any{"size_gb": float64(200)}),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := rating.Rate(model, []metering.ResourceUsage{{
				Resource: tc.resource,
				Drafts:   []metering.UsageDraft{tc.draft},
			}})

			if !slices.Equal(result.Unreadable, tc.want) {
				t.Errorf("Unreadable = %+v, want %+v", result.Unreadable, tc.want)
			}
		})
	}

	t.Run("counts one draft per resource and field", func(t *testing.T) {
		unreadable := draft("active", 14400, map[string]any{"vcpus": "m1.small"})
		result := rating.Rate(model, []metering.ResourceUsage{{
			Resource: instance,
			Drafts:   []metering.UsageDraft{unreadable, draft("active", 14400, instanceSize(0)), unreadable},
		}})

		want := []rating.UnreadableQuantity{
			{Platform: "openstack", ResourceType: "instance", Field: "vcpus", Count: 2},
		}
		if !slices.Equal(result.Unreadable, want) {
			t.Errorf("Unreadable = %+v, want %+v", result.Unreadable, want)
		}

		// The dimension is still emitted, at the amount an absent quantity
		// would have been billed at, so the record shows what it was held
		// against either way.
		assertAmounts(t, result.Resources[0].Records[0].Amounts, []wantAmount{
			{"vcpus", "0.00"}, {"ram_gb", "0.00"}, {"disk_gb", "0.00"}, {"egress_gb", "0.00"},
		})
	})
}
