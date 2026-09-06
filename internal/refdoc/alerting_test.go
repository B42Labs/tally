package refdoc

import (
	"os"
	"strings"
	"testing"
)

// The alerting configuration of this repository, and the size of it a page
// states. A rule added without a section is noticed here.
const (
	realRules   = "../../deploy/kubernetes/base/vmalert/rules.yaml"
	realRouting = "../../deploy/kubernetes/base/alertmanager/config.yaml"
	realRuleSet = 12
)

func TestAlertRules(t *testing.T) {
	got, err := AlertRules(readFixture(t, "rules.yaml"))
	if err != nil {
		t.Fatalf("AlertRules() error = %v, want nil", err)
	}

	assertWant(t, "rules.want.md", got)
}

func TestAlertRulesRendersEachRuleShape(t *testing.T) {
	got, err := AlertRules(readFixture(t, "rules.yaml"))
	if err != nil {
		t.Fatalf("AlertRules() error = %v, want nil", err)
	}

	for _, want := range []string{
		"Group `fixture`, evaluated every `1m`.",
		"| Runbook | `docs/runbooks/FixtureCloudSilent.md` |",
		// A rule that fires at once carries no timer, and one nobody is woken
		// for carries no runbook.
		"| For | none |\n| Runbook | none |",
		// The template stays inside a fence, where the site reads it as text.
		"```text\nNo collector events from {{ $labels.cloud }} for >1h\n```",
		"```promql\nsum by (cloud, resource_type) (tally_current_resources)\n```",
		// A recording rule writes a series back rather than firing.
		"### `fixture:current_resources:sum`\n\nRecorded series.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the rendering does not carry %q:\n%s", want, got)
		}
	}
}

func TestAlertRulesRendersTheRulesOfThisRepository(t *testing.T) {
	got, err := AlertRules(readRepositoryFile(t, realRules))
	if err != nil {
		t.Fatalf("AlertRules() error = %v, want nil", err)
	}

	if n := countHeadings(got, "### "); n != realRuleSet {
		t.Errorf("the rule file rendered %d rules, want %d", n, realRuleSet)
	}
	for _, want := range []string{
		"Group `tally`, evaluated every `1m`.",
		"### `TallyCloudEventsSilent`",
		"| Runbook | `docs/runbooks/TallyCloudEventsSilent.md` |",
		"### `tally:current_resources:sum`\n\nRecorded series.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the rendering does not carry %q", want)
		}
	}
	// The one rule with no timer of its own.
	stale := section(t, got, "### `TallySyncStale`")
	if !strings.Contains(stale, "| For | none |") {
		t.Errorf("TallySyncStale is rendered with a timer:\n%s", stale)
	}
}

func TestAlertRulesRejectsWhatItCannotRender(t *testing.T) {
	cases := map[string]struct {
		rules string
		want  string
	}{
		"no groups": {
			rules: "groups: []\n",
			want:  "refdoc: no groups",
		},
		"a rule that is neither an alert nor a record": {
			rules: "groups:\n  - name: tally\n    rules:\n      - expr: up == 0\n",
			want:  "refdoc: rule 0 of group tally has no name",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := AlertRules([]byte(tc.rules))
			if err == nil {
				t.Fatalf("AlertRules() error = nil, want %q", tc.want)
			}
			if err.Error() != tc.want {
				t.Errorf("AlertRules() error = %q, want %q", err, tc.want)
			}
		})
	}
}

func TestAlertRulesReportsWhatTheParserSaid(t *testing.T) {
	_, err := AlertRules([]byte("groups: [\n"))
	if err == nil {
		t.Fatal("AlertRules() error = nil, want a parse error")
	}
	// The parser names the line it stumbled over, which is what a reader of the
	// failure needs.
	if !strings.Contains(err.Error(), "yaml:") {
		t.Errorf("AlertRules() error = %q, want it to carry what the parser said", err)
	}
}

func TestAlertRouting(t *testing.T) {
	got, err := AlertRouting(readFixture(t, "alertmanager.yaml"))
	if err != nil {
		t.Fatalf("AlertRouting() error = %v, want nil", err)
	}

	assertWant(t, "alertmanager.want.md", got)
}

func TestAlertRoutingRendersTheRoutingOfThisRepository(t *testing.T) {
	got, err := AlertRouting(readRepositoryFile(t, realRouting))
	if err != nil {
		t.Fatalf("AlertRouting() error = %v, want nil", err)
	}

	for _, want := range []string{
		"| Receiver | `default` |",
		"| Group by | `alertname`, `cloud` |",
		"| Group wait | `30s` |",
		"| Group interval | `5m` |",
		"| Repeat interval | `4h` |",
		"| `severity=\"critical\"` | `repeat_interval: 1h` |",
		// Where an alert is delivered is a property of a deployment, so the
		// receiver in this repository names no integration.
		"The receivers are `default`; none carries an integration.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the rendering does not carry %q:\n%s", want, got)
		}
	}
}

func TestAlertRoutingReportsAReceiverThatDelivers(t *testing.T) {
	got, err := AlertRouting([]byte(
		"route:\n  receiver: default\nreceivers:\n  - name: default\n    webhook_configs:\n      - url: http://x\n"))
	if err != nil {
		t.Fatalf("AlertRouting() error = %v, want nil", err)
	}

	want := "The receivers are `default`; `default` carries an integration."
	if !strings.Contains(got, want) {
		t.Errorf("the rendering does not carry %q:\n%s", want, got)
	}
}

func TestAlertRoutingNamesEveryReceiverThatDelivers(t *testing.T) {
	got, err := AlertRouting([]byte(
		"route:\n  receiver: default\nreceivers:\n" +
			"  - name: default\n    webhook_configs:\n      - url: http://x\n" +
			"  - name: oncall\n    email_configs:\n      - to: a@b\n"))
	if err != nil {
		t.Fatalf("AlertRouting() error = %v, want nil", err)
	}

	want := "The receivers are `default`, `oncall`; `default`, `oncall` carry an integration."
	if !strings.Contains(got, want) {
		t.Errorf("the rendering does not carry %q:\n%s", want, got)
	}
}

func TestAlertRoutingReportsAConfigurationWithoutAReceiver(t *testing.T) {
	got, err := AlertRouting([]byte("route:\n  receiver: default\n"))
	if err != nil {
		t.Fatalf("AlertRouting() error = %v, want nil", err)
	}

	// A route naming a receiver the configuration does not declare is a
	// deployment fault, and the page states it rather than hiding it.
	want := "The configuration names no receiver."
	if !strings.Contains(got, want) {
		t.Errorf("the rendering does not carry %q:\n%s", want, got)
	}
}

func TestAlertRoutingReportsWhatTheParserSaid(t *testing.T) {
	_, err := AlertRouting([]byte("route: [\n"))
	if err == nil {
		t.Fatal("AlertRouting() error = nil, want a parse error")
	}
	// The renderer names itself, so a failure of `make generate` says which of
	// the two alerting renderers stumbled.
	if !strings.HasPrefix(err.Error(), "refdoc: routing: ") {
		t.Errorf("AlertRouting() error = %q, want it to name the renderer", err)
	}
}

func TestAlertRoutingRejectsAConfigurationWithoutARoute(t *testing.T) {
	_, err := AlertRouting([]byte("receivers:\n  - name: default\n"))
	if err == nil {
		t.Fatal("AlertRouting() error = nil, want an error")
	}
	if want := "refdoc: no route"; err.Error() != want {
		t.Errorf("AlertRouting() error = %q, want %q", err, want)
	}
}

// readRepositoryFile returns the bytes of a file this repository deploys. The
// renderers take bytes rather than a path, so this is what the caller of one
// does with the file it generates a page from.
func readRepositoryFile(t *testing.T, path string) []byte {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return raw
}
