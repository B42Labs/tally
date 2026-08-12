package reconciliation

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// CloudConfig is one syncable cloud: which platform it belongs to, which
// adapter observes it, and the settings that adapter needs.
type CloudConfig struct {
	// Cloud is the cloud name. It is the same name the events carry and the
	// same one the internal sync endpoint takes as its path segment.
	Cloud string `yaml:"cloud"`
	// Platform is the platform the cloud belongs to. It is stated here as well
	// as implied by the adapter, so that a cloud wired to the wrong adapter is
	// caught when the file is read rather than when the events land.
	Platform string `yaml:"platform"`
	// Adapter is the key of the adapter in the registry the process passes to
	// LoadConfig.
	Adapter string `yaml:"adapter"`
	// AdapterConfig is handed to the adapter unchanged. It stays uninterpreted
	// here because only the adapter knows what belongs in it, which is what
	// lets a new provider add settings without touching the framework.
	AdapterConfig map[string]any `yaml:"adapter_config"`
}

// Config is the set of clouds a deployment can reconcile. Having none is a
// valid configuration: the sync endpoint then answers 404 for every cloud,
// which is what a deployment that only receives collector events wants.
type Config struct {
	// Clouds is every configured cloud, in file order.
	Clouds []CloudConfig `yaml:"clouds"`
}

// LoadConfig reads the clouds configuration at path and checks every entry
// against the adapters compiled into the process. An empty path yields the zero
// Config and no error, so a deployment without clouds starts normally.
//
// The checks run here rather than at sync time, so that a typo stops startup
// instead of surfacing hours later as a cron job that keeps failing.
func LoadConfig(path string, adapters map[string]Adapter) (Config, error) {
	if path == "" {
		return Config{}, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("reading the clouds config %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parsing the clouds config %s: %w", path, err)
	}

	seen := make(map[string]bool, len(cfg.Clouds))
	for i, cloud := range cfg.Clouds {
		// An entry without a name cannot be reported by it, so the position in
		// the list is what points the operator at the right block.
		entry := fmt.Sprintf("clouds[%d]", i)
		if cloud.Cloud != "" {
			entry = fmt.Sprintf("cloud %q", cloud.Cloud)
		}

		if cloud.Cloud == "" {
			return Config{}, fmt.Errorf("%s: cloud must be set", entry)
		}
		// Two entries under one name would each claim the same projection rows
		// and the same advisory lock, so the second sync would look like a
		// duplicate of the first rather than a configuration mistake.
		if seen[cloud.Cloud] {
			return Config{}, fmt.Errorf("%s: configured twice", entry)
		}
		seen[cloud.Cloud] = true

		if cloud.Platform == "" {
			return Config{}, fmt.Errorf("%s: platform must be set", entry)
		}
		if cloud.Adapter == "" {
			return Config{}, fmt.Errorf("%s: adapter must be set", entry)
		}

		adapter, ok := adapters[cloud.Adapter]
		if !ok {
			return Config{}, fmt.Errorf("%s: no adapter named %q is registered", entry, cloud.Adapter)
		}
		if adapter.Platform() != cloud.Platform {
			return Config{}, fmt.Errorf("%s: adapter %q observes platform %q, not %q",
				entry, cloud.Adapter, adapter.Platform(), cloud.Platform)
		}
	}

	return cfg, nil
}
