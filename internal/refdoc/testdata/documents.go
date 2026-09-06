package testdata

import (
	"encoding/json"
	"time"

	"github.com/b42labs/tally/internal/core/money"
)

// Document is one project's statement as the export writes it.
type Document struct {
	BillingPeriod Period `json:"billing_period"`
	ProjectID     string `json:"project_id"`
	// LineItems is what the project is billed for, one entry per resource.
	LineItems []LineItem `json:"line_items"`
	// BaseCost is nil on a statement no adjustment reached.
	BaseCost *money.Amount   `json:"base_cost,omitempty"`
	Total    money.Amount    `json:"total"`
	Size     map[string]any  `json:"size"`
	Stats    json.RawMessage `json:"stats"`
	Received time.Time       `json:"received_at"`
	// Skipped is stored but never published, so the tag leaves it out.
	Skipped string `json:"-"`
	// Legacy carries no tag at all, which keeps it off the wire.
	Legacy string
	// internal is unexported and unpublished.
	internal string
	Period
}

// Period is the half-open interval the document bills, both ends in UTC.
type Period struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// LineItem is one resource as the project is billed for it.
type LineItem struct {
	ResourceID string `json:"resource_id"`
	// Quantities holds one entry per rated dimension.
	Quantities map[string]money.Quantity `json:"quantities"`
	Total      money.Amount              `json:"total"`
	Related    []LineItem                `json:"related,omitempty"`
}

type (
	// Grouped is declared in a group, so its comment sits on the spec rather
	// than on the declaration.
	Grouped struct {
		Name string `json:"name"`
	}
)

// Key is the pair a document is stored under. It is not a struct, so a call
// naming it is refused.
type Key string

// Untagged carries no member the wire format names.
type Untagged struct {
	internal string
}

// sourceEntry is one entry of the counter sources file.
type sourceEntry struct {
	Platform string `yaml:"platform"`
	// Required is a pointer so that an absent key can be told from false.
	Required *bool `yaml:"required"`
}
