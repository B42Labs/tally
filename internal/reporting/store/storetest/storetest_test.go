package storetest

import (
	"bytes"
	"errors"
	"io"
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

// baseManifest is the TimescaleDB manifest every deployment builds on.
const baseManifest = "../../../../deploy/kubernetes/base/timescaledb/timescaledb.yaml"

func TestImageIsTheOneTheBaseDeploys(t *testing.T) {
	// The integration tests are worth what they say about the database a
	// cluster runs. With two tags the tests pass on one TimescaleDB release
	// while the manifest deploys another, and neither side fails on its own.
	// The test reads the YAML from disk and starts no container.
	raw, err := os.ReadFile(baseManifest)
	if err != nil {
		t.Fatalf("reading %s: %v", baseManifest, err)
	}

	dec := yaml.NewDecoder(bytes.NewReader(raw))
	for {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
			Spec struct {
				Template struct {
					Spec struct {
						Containers []struct {
							Name  string `yaml:"name"`
							Image string `yaml:"image"`
						} `yaml:"containers"`
					} `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("parsing %s: %v", baseManifest, err)
		}
		if doc.Kind != "StatefulSet" || doc.Metadata.Name != "timescaledb" {
			continue
		}

		for _, c := range doc.Spec.Template.Spec.Containers {
			if c.Name != "timescaledb" {
				continue
			}
			if c.Image != image {
				t.Errorf("%s deploys %q and storetest.go runs the tests on %q; a version bump edits the image in both files",
					baseManifest, c.Image, image)
			}
			return
		}
		t.Fatalf("StatefulSet timescaledb in %s declares no container timescaledb, so there is no image to hold storetest.go to", baseManifest)
	}
	t.Fatalf("%s declares no StatefulSet timescaledb, so there is no image to hold storetest.go to", baseManifest)
}
