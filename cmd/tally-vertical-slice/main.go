// Command tally-vertical-slice rates one project's instance usage for one
// calendar month and prints it as JSON.
//
// It is a throwaway prototype (roadmap/01-phase-1-core-platform-openstack.md,
// WP 1.15). It exists to prove the billing chain end to end on numbers that can
// be checked by hand: it reads the Reporting API, folds each instance's history
// with internal/core/timeline, clips the intervals to the month, derives usage
// records, rates them against a pricing file, and checks the metering
// invariants over what it derived. Nothing under internal/ knows about it, and
// Phase 3 replaces it with the metering engine; only the timeline fold carries
// over.
//
// The clipping, the rating, and the invariant checks therefore live here rather
// than in a package: they are the engine's work, and putting a prototype's
// version of them in the core would be the wrong home for code that is meant to
// be deleted.
//
// The document goes to stdout even when an invariant is broken, and the process
// then exits 1: the numbers are what the run is for, so they stay inspectable
// while the exit status still reports the failure.
//
//	TALLY_SLICE_TOKEN=<api-token> go run ./cmd/tally-vertical-slice \
//	  --cloud os-prod-eu1 --project proj-456 --month 2026-03 \
//	  --reporting-url https://api.tally.127-0-0-1.nip.io:8443 \
//	  --pricing pricing/prototype.yaml --ca-file tally-ca.crt
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/core/money"
	"github.com/b42labs/tally/internal/core/timeline"
)

const (
	// tokenEnv names the variable the API token is read from. It stays out of
	// the flags so that the credential never appears in the process's argv.
	tokenEnv = "TALLY_SLICE_TOKEN"
	// violationLimit caps what one failure may add to the document. The text is
	// the API's to choose, and one line of it is held per resource until the run
	// encodes the document at the end, so an unbounded one would let a project's
	// worth of oversized problem documents grow the run out of memory before it
	// prints anything at all.
	violationLimit = 4 << 10
)

// config is one run's input. It is a value rather than a set of globals, so a
// test drives run against an httptest server and a buffer.
type config struct {
	cloud        string
	project      string
	month        string
	reportingURL string
	pricingPath  string
	caFile       string
	token        string
}

func main() {
	var cfg config
	// ExitOnError exits before Parse returns an error.
	_ = newFlagSet(&cfg).Parse(os.Args[1:])

	cfg.token = os.Getenv(tokenEnv)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg, os.Stdout); err != nil {
		slog.Error("tally-vertical-slice stopped", "error", err)
		os.Exit(1)
	}
}

// newFlagSet is the flag set the slice parses its arguments with. It writes
// each flag into the field of cfg it configures. The set is built here rather
// than in main so that the flags a run accepts are readable without running
// the command.
func newFlagSet(cfg *config) *flag.FlagSet {
	fs := flag.NewFlagSet("tally-vertical-slice", flag.ExitOnError)
	fs.StringVar(&cfg.cloud, "cloud", "", "the cloud the instances live in, os-prod-eu1 for example")
	fs.StringVar(&cfg.project, "project", "", "the project to rate, named the way its cloud names it")
	fs.StringVar(&cfg.month, "month", "", "the calendar month to rate, as YYYY-MM")
	fs.StringVar(&cfg.reportingURL, "reporting-url", "",
		"https base URL of the Reporting API, without the /api/v1 suffix")
	fs.StringVar(&cfg.pricingPath, "pricing", "", "path to the pricing file, pricing/prototype.yaml for example")
	fs.StringVar(&cfg.caFile, "ca-file", "",
		"PEM bundle to trust instead of the system store, for a dev cluster's own CA")
	return fs
}

// run rates the configured month and writes the document to out. It returns an
// error for a run that could not produce a document, and for one whose numbers
// broke an invariant: the second kind has already written its output by then.
func run(ctx context.Context, cfg config, out io.Writer) error {
	for _, required := range []struct{ name, value string }{
		{"--cloud", cfg.cloud},
		{"--project", cfg.project},
		{"--month", cfg.month},
		{"--reporting-url", cfg.reportingURL},
		{"--pricing", cfg.pricingPath},
	} {
		if required.value == "" {
			return fmt.Errorf("%s is required", required.name)
		}
	}
	if cfg.token == "" {
		return fmt.Errorf("%s is required", tokenEnv)
	}

	p, err := parseMonth(cfg.month)
	if err != nil {
		return err
	}
	pr, err := loadPricing(cfg.pricingPath)
	if err != nil {
		return err
	}
	api, err := newClient(cfg.reportingURL, cfg.token, cfg.caFile)
	if err != nil {
		return err
	}

	resourceIDs, err := api.listResources(ctx, cfg.cloud, cfg.project)
	if err != nil {
		return fmt.Errorf("listing resources for %s/%s: %w", cfg.cloud, cfg.project, err)
	}

	doc := document{
		Cloud:     cfg.cloud,
		ProjectID: cfg.project,
		Period:    periodDoc(p),
		Currency:  pr.Currency,
		Resources: make([]resourceDoc, 0, len(resourceIDs)),
	}
	total := decimal.Zero
	violations := 0

	for _, resourceID := range resourceIDs {
		resource, billed, err := rateResource(ctx, api, cfg.cloud, resourceID, p, pr)
		if err != nil {
			return err
		}
		if !billed {
			continue
		}

		violations += len(resource.Violations)
		total = total.Add(resource.Total.Decimal)
		doc.Resources = append(doc.Resources, resource)
	}
	doc.Total = money.NewAmount(total)

	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(doc); err != nil {
		return fmt.Errorf("writing the document: %w", err)
	}

	// The document is out before this returns, which is the point of checking
	// here: a broken run is one whose numbers are read, not one that printed
	// nothing.
	if violations > 0 {
		return fmt.Errorf("the document reports %d broken metering invariants", violations)
	}
	return nil
}

// rateResource reads one instance's history and rates it for the period. A
// history the API will not serve and a history the rating refuses both become a
// resource carrying the failure as a violation rather than an error: one
// instance the API lost between the listing and the walk, or one whose history
// carries no size, is not a reason to withhold the numbers of every other
// instance of the project. The document already has the slot that says these
// are not to be trusted.
//
// The error return is what stops the run, and a cancelled one is what it is
// for: every remaining fetch would fail the same way, so the run would print a
// document that reports the operator's own interrupt once per resource.
func rateResource(ctx context.Context, api *client, cloud, resourceID string, p period, pr pricing) (resourceDoc, bool, error) {
	events, err := api.fetchHistory(ctx, cloud, resourceID)
	if err != nil {
		if ctx.Err() != nil {
			return resourceDoc{}, false, err
		}
		return brokenResource(resourceID, fmt.Sprintf("the history could not be read: %v", err)), true, nil
	}

	resource, billed, err := buildResourceDoc(resourceID, timeline.Build(events), p, pr)
	if err != nil {
		return brokenResource(resourceID, fmt.Sprintf("the resource could not be rated: %v", err)), true, nil
	}
	return resource, billed, nil
}

// brokenResource is a resource the run could not put numbers on, reported with
// what went wrong in the slot the document keeps for numbers that are not to be
// trusted.
func brokenResource(resourceID, violation string) resourceDoc {
	if len(violation) > violationLimit {
		// The cut lands on a byte, so what it leaves of a multi-byte character
		// is dropped rather than encoded as the U+FFFD the encoder would put
		// there.
		violation = strings.ToValidUTF8(violation[:violationLimit], "") + "…"
	}
	return resourceDoc{
		ResourceID: resourceID,
		Warnings:   []string{},
		Violations: []string{violation},
		Records:    []recordDoc{},
		Total:      money.NewAmount(decimal.Zero),
	}
}
