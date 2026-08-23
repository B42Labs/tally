package money_test

import (
	"encoding/json"
	"testing"

	"github.com/b42labs/tally/internal/core/money"
	"github.com/shopspring/decimal"
)

// dec builds a decimal from its string form, which is the only construction the
// money rules allow.
func dec(t *testing.T, s string) decimal.Decimal {
	t.Helper()

	d, err := decimal.NewFromString(s)
	if err != nil {
		t.Fatalf("parsing %q: %v", s, err)
	}
	return d
}

func TestRound2IsHalfUp(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "2.025", want: "2.03"},
		{in: "-2.025", want: "-2.03"},
		{in: "2.024", want: "2.02"},
		{in: "-2.024", want: "-2.02"},
		{in: "2.035", want: "2.04"},
		{in: "19.2", want: "19.2"},
		{in: "0", want: "0"},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got := money.Round2(dec(t, tc.in))
			if !got.Equal(dec(t, tc.want)) {
				t.Errorf("Round2(%s) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

func TestMinutes(t *testing.T) {
	tests := []struct {
		name    string
		seconds int64
		want    string
	}{
		{name: "zero", seconds: 0, want: "0"},
		{name: "exact minute", seconds: 60, want: "1"},
		{name: "minute and a half", seconds: 90, want: "1.5"},
		{name: "repeating decimal", seconds: 59, want: "0.9833"},
		{name: "an hour", seconds: 3600, want: "60"},
		{name: "negative", seconds: -90, want: "-1.5"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := money.Minutes(tc.seconds)
			if !got.Equal(dec(t, tc.want)) {
				t.Errorf("Minutes(%d) = %s, want %s", tc.seconds, got, tc.want)
			}
		})
	}
}

func TestRoundQuantity(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		want      string
		wantFixed string
	}{
		{name: "half up at the fifth place", in: "22.49995", want: "22.5", wantFixed: "22.5000"},
		{name: "smallest value that rounds up", in: "0.00005", want: "0.0001", wantFixed: "0.0001"},
		{name: "largest value that rounds down", in: "0.00004", want: "0", wantFixed: "0.0000"},
		{name: "half away from zero on the negative side", in: "-1.23455", want: "-1.2346", wantFixed: "-1.2346"},
		{name: "counter value left unchanged", in: "812", want: "812", wantFixed: "812.0000"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := money.RoundQuantity(dec(t, tc.in))
			if !got.Equal(dec(t, tc.want)) {
				t.Errorf("RoundQuantity(%s) = %s, want %s", tc.in, got, tc.want)
			}
			if fixed := got.StringFixed(4); fixed != tc.wantFixed {
				t.Errorf("RoundQuantity(%s).StringFixed(4) = %s, want %s", tc.in, fixed, tc.wantFixed)
			}
		})
	}
}

func TestDivPrecision(t *testing.T) {
	// One third at the working precision: 28 digits after the point, and the
	// last one rounded up rather than truncated.
	got := money.Div(dec(t, "1"), dec(t, "3"))
	want := dec(t, "0.3333333333333333333333333333")

	if !got.Equal(want) {
		t.Errorf("Div(1, 3) = %s, want %s", got, want)
	}
	if got.Exponent() != -28 {
		t.Errorf("Div(1, 3) exponent = %d, want -28", got.Exponent())
	}
}

func TestDivByZeroPanics(t *testing.T) {
	// No billing input is legitimately a zero divisor, so this is a caller bug
	// that must surface loudly rather than produce a silent zero.
	defer func() {
		if recover() == nil {
			t.Error("Div(1, 0) returned normally, want a panic")
		}
	}()

	money.Div(dec(t, "1"), decimal.Zero)
}

func TestAmountMarshalsWithTwoDecimals(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "2.03", want: "2.03"},
		{in: "19.2", want: "19.20"},
		{in: "0", want: "0.00"},
		{in: "124.8", want: "124.80"},
		{in: "-3.5", want: "-3.50"},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := json.Marshal(money.NewAmount(dec(t, tc.in)))
			if err != nil {
				t.Fatalf("Marshal() = %v, want nil", err)
			}
			if string(got) != tc.want {
				t.Errorf("Marshal(%s) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

func TestAmountMarshalsInsideAStruct(t *testing.T) {
	// The exported shape is what a customer reconciles against an invoice, so
	// the two places must survive being nested in a response body.
	body := struct {
		Total money.Amount `json:"total"`
	}{Total: money.NewAmount(dec(t, "19.2"))}

	got, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("Marshal() = %v, want nil", err)
	}
	if want := `{"total":19.20}`; string(got) != want {
		t.Errorf("Marshal() = %s, want %s", got, want)
	}
}

func TestQuantityMarshalsWithFourDecimals(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "21600", want: "21600.0000"},
		{in: "23039.9833", want: "23039.9833"},
		{in: "0", want: "0.0000"},
		{in: "-1.5", want: "-1.5000"},
		// StringFixed rounds half away from zero, in both directions.
		{in: "1.23455", want: "1.2346"},
		{in: "-1.23456", want: "-1.2346"},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := json.Marshal(money.NewQuantity(dec(t, tc.in)))
			if err != nil {
				t.Fatalf("Marshal() = %v, want nil", err)
			}
			if string(got) != tc.want {
				t.Errorf("Marshal(%s) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

func TestQuantityMarshalsInsideAStruct(t *testing.T) {
	// Usage quantities reach JSONB nested in a usage map, so the four places
	// must survive the enclosing struct rather than collapsing to 21600.
	body := struct {
		Minutes money.Quantity `json:"minutes"`
	}{Minutes: money.NewQuantity(dec(t, "21600"))}

	got, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("Marshal() = %v, want nil", err)
	}
	if want := `{"minutes":21600.0000}`; string(got) != want {
		t.Errorf("Marshal() = %s, want %s", got, want)
	}
}
