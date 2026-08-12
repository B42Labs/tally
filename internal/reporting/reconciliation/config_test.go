package reconciliation_test

import (
	"context"
	"errors"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/b42labs/tally/internal/reporting/reconciliation"
)

// stubAdapter answers Platform and nothing else. LoadConfig calls no other
// method, so leaving the streaming half empty keeps the configuration tests
// free of a fake inventory.
type stubAdapter struct {
	platform string
}

func (a stubAdapter) Platform() string { return a.platform }

func (a stubAdapter) ResourceTypes(map[string]any) ([]string, error) { return nil, nil }

func (a stubAdapter) ListResources(context.Context, map[string]any, *time.Time,
) iter.Seq2[reconciliation.ObservedResource, error] {
	return nil
}

// testAdapters is the registry the tests load against: two platforms, so a
// mismatch between an entry's platform and its adapter is a case the registry
// can actually produce.
func testAdapters() map[string]reconciliation.Adapter {
	return map[string]reconciliation.Adapter{
		"openstack": stubAdapter{platform: "openstack"},
		"hetzner":   stubAdapter{platform: "hetzner"},
	}
}

// writeConfig writes a clouds configuration and returns its path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "clouds.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func TestLoadConfigReadsAValidFile(t *testing.T) {
	path := writeConfig(t, `clouds:
  - cloud: os-prod-eu1
    platform: openstack
    adapter: openstack
    adapter_config:
      os_cloud: os-prod-eu1
      include_octavia: false
  - cloud: hz-prod
    platform: hetzner
    adapter: hetzner
`)

	cfg, err := reconciliation.LoadConfig(path, testAdapters())
	if err != nil {
		t.Fatalf("LoadConfig() error = %v, want nil", err)
	}

	if len(cfg.Clouds) != 2 {
		t.Fatalf("Clouds = %d entries, want 2", len(cfg.Clouds))
	}

	first := cfg.Clouds[0]
	if first.Cloud != "os-prod-eu1" {
		t.Errorf("Clouds[0].Cloud = %q, want %q", first.Cloud, "os-prod-eu1")
	}
	if first.Platform != "openstack" {
		t.Errorf("Clouds[0].Platform = %q, want %q", first.Platform, "openstack")
	}
	if first.Adapter != "openstack" {
		t.Errorf("Clouds[0].Adapter = %q, want %q", first.Adapter, "openstack")
	}
	if osCloud, ok := first.AdapterConfig["os_cloud"].(string); !ok || osCloud != "os-prod-eu1" {
		t.Errorf("AdapterConfig[os_cloud] = %#v, want the string %q",
			first.AdapterConfig["os_cloud"], "os-prod-eu1")
	}
	if octavia, ok := first.AdapterConfig["include_octavia"].(bool); !ok || octavia {
		t.Errorf("AdapterConfig[include_octavia] = %#v, want the boolean false",
			first.AdapterConfig["include_octavia"])
	}

	// An entry without adapter_config keeps the nil map, which the adapter has
	// to accept: an empty configuration is not a missing one.
	if second := cfg.Clouds[1]; second.AdapterConfig != nil {
		t.Errorf("Clouds[1].AdapterConfig = %#v, want nil", second.AdapterConfig)
	}
}

func TestLoadConfigAcceptsNoClouds(t *testing.T) {
	t.Run("an empty path configures no cloud at all", func(t *testing.T) {
		cfg, err := reconciliation.LoadConfig("", testAdapters())
		if err != nil {
			t.Fatalf("LoadConfig() error = %v, want nil", err)
		}
		if len(cfg.Clouds) != 0 {
			t.Errorf("Clouds = %#v, want none", cfg.Clouds)
		}
	})

	t.Run("a file with an empty list is a valid file", func(t *testing.T) {
		cfg, err := reconciliation.LoadConfig(writeConfig(t, "clouds: []\n"), testAdapters())
		if err != nil {
			t.Fatalf("LoadConfig() error = %v, want nil", err)
		}
		if len(cfg.Clouds) != 0 {
			t.Errorf("Clouds = %#v, want none", cfg.Clouds)
		}
	})
}

func TestLoadConfigRejectsAnUnreadableFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.yaml")

	_, err := reconciliation.LoadConfig(path, testAdapters())
	if err == nil {
		t.Fatal("LoadConfig() error = nil, want an error")
	}
	if prefix := "reading the clouds config"; !strings.HasPrefix(err.Error(), prefix) {
		t.Errorf("LoadConfig() error = %q, want it to start with %q", err, prefix)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("LoadConfig() error = %v, want it to wrap os.ErrNotExist", err)
	}
}

func TestLoadConfigRejectsMalformedYAML(t *testing.T) {
	path := writeConfig(t, "clouds:\n  - cloud: os-prod-eu1\n   platform: openstack\n")

	_, err := reconciliation.LoadConfig(path, testAdapters())
	if err == nil {
		t.Fatal("LoadConfig() error = nil, want an error")
	}
	if prefix := "parsing the clouds config"; !strings.HasPrefix(err.Error(), prefix) {
		t.Errorf("LoadConfig() error = %q, want it to start with %q", err, prefix)
	}
}

func TestLoadConfigRejectsInvalidEntries(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		wantTexts []string
	}{
		{
			name: "the same cloud configured twice",
			content: `clouds:
  - cloud: os-prod-eu1
    platform: openstack
    adapter: openstack
  - cloud: os-prod-eu1
    platform: openstack
    adapter: openstack
`,
			wantTexts: []string{"os-prod-eu1"},
		},
		{
			name: "an entry without a cloud name is reported by its position",
			content: `clouds:
  - cloud: os-prod-eu1
    platform: openstack
    adapter: openstack
  - platform: openstack
    adapter: openstack
`,
			wantTexts: []string{"clouds[1]"},
		},
		{
			name: "an entry without a platform",
			content: `clouds:
  - cloud: os-prod-eu1
    adapter: openstack
`,
			wantTexts: []string{"os-prod-eu1", "platform"},
		},
		{
			name: "an entry without an adapter",
			content: `clouds:
  - cloud: os-prod-eu1
    platform: openstack
`,
			wantTexts: []string{"os-prod-eu1", "adapter"},
		},
		{
			name: "an adapter the registry does not hold",
			content: `clouds:
  - cloud: os-prod-eu1
    platform: openstack
    adapter: vsphere
`,
			wantTexts: []string{"os-prod-eu1", "vsphere"},
		},
		{
			name: "an adapter that observes another platform",
			content: `clouds:
  - cloud: os-prod-eu1
    platform: openstack
    adapter: hetzner
`,
			wantTexts: []string{"os-prod-eu1", "hetzner", "openstack"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := reconciliation.LoadConfig(writeConfig(t, tc.content), testAdapters())
			if err == nil {
				t.Fatal("LoadConfig() error = nil, want an error")
			}
			for _, want := range tc.wantTexts {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("LoadConfig() error = %q, want it to name %q", err, want)
				}
			}
		})
	}
}
