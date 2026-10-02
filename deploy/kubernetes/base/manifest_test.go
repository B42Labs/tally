// This file holds the base to plain Gateway API. Dev and prod build on it, and
// so does a cluster that runs Traefik, Cilium or any other implementation, and
// such a cluster has no CRD for an object of one implementation's own group.
// Nothing in the repository shows the mismatch: both overlays render, the dev
// stack runs Envoy Gateway, and the apply fails on the other cluster alone.
// The test reads the YAML from disk and needs neither a cluster nor kustomize.
package base_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// baseDir is the directory this file lies in, which is the base.
const baseDir = "."

// neutralGroups is every API group an object of the base is in today. The
// empty one is the core group, whose apiVersion is v1 alone. A group every
// cluster serves, such as networking.k8s.io or policy, is added here when the
// base first declares an object in it. A group one Gateway API implementation
// brings is not, and its objects belong in a component.
var neutralGroups = []string{
	"",
	"apps",
	"batch",
	"rbac.authorization.k8s.io",
	"gateway.networking.k8s.io",
	"cert-manager.io",
}

// object is the part of a manifest document this test asserts over, with the
// file that declares it. raw is the whole document, whose spec the test walks.
type object struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	path string
	raw  map[string]any
}

func TestBaseNamesNoImplementation(t *testing.T) {
	const (
		where      = "it belongs in a component under deploy/kubernetes/components/"
		whichGroup = "a group every cluster serves is added to neutralGroups, and what is in the group of one implementation belongs in a component under deploy/kubernetes/components/"
	)

	objects := baseObjects(t)
	if len(objects) == 0 {
		t.Fatalf("no YAML file under %s declares an object, so this test would assert over nothing", baseDir)
	}

	for _, o := range objects {
		group, _, found := strings.Cut(o.APIVersion, "/")
		if !found {
			group = ""
		}
		if !slices.Contains(neutralGroups, group) {
			t.Errorf("%s declares %s %s in the API group %q, which is not in neutralGroups; %s",
				o.path, o.Kind, o.Metadata.Name, group, whichGroup)
		}
		// A GatewayClass is in the neutral group and names one controller all
		// the same.
		if o.Kind == "GatewayClass" {
			t.Errorf("%s declares GatewayClass %s, which binds the Gateway to one controller; %s", o.path, o.Metadata.Name, where)
		}
		// An object of a neutral group names one implementation's kind by a
		// reference as well: a parametersRef on the Gateway, a backendRef, or
		// an ExtensionRef filter on a rule or on one of its backends. The walk
		// reads the whole spec, so the place of the reference does not matter.
		var walk func(v any, at string)
		walk = func(v any, at string) {
			switch v := v.(type) {
			case map[string]any:
				if g, ok := v["group"].(string); ok && !slices.Contains(neutralGroups, g) {
					t.Errorf("%s declares %s %s, which references the API group %q at %s, and that group is not in neutralGroups; %s",
						o.path, o.Kind, o.Metadata.Name, g, at, whichGroup)
				}
				if v["type"] == "ExtensionRef" {
					t.Errorf("%s declares %s %s, which carries an ExtensionRef filter at %s, and what it references is one implementation's kind; %s",
						o.path, o.Kind, o.Metadata.Name, at, where)
				}
				for key, child := range v {
					walk(child, at+"/"+key)
				}
			case []any:
				for i, child := range v {
					walk(child, fmt.Sprintf("%s/%d", at, i))
				}
			}
		}
		walk(o.raw["spec"], "/spec")
	}
}

// baseObjects decodes every object the YAML files of the base declare. A
// document without a kind and a name, such as a scrape, a rules or a
// provisioning file, is not an object and is skipped.
func baseObjects(t *testing.T) []object {
	t.Helper()

	var objects []object
	err := filepath.WalkDir(baseDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || filepath.Ext(path) != ".yaml" {
			return walkErr
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		for {
			var node yaml.Node
			err := dec.Decode(&node)
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("parsing %s: %w", path, err)
			}
			var shape any
			if err := node.Decode(&shape); err != nil {
				return fmt.Errorf("parsing %s: %w", path, err)
			}
			doc, _ := shape.(map[string]any)
			metadata, _ := doc["metadata"].(map[string]any)
			_, kind := doc["kind"].(string)
			if _, name := metadata["name"].(string); !kind || !name {
				continue
			}
			o := object{path: path, raw: doc}
			if err := node.Decode(&o); err != nil {
				return fmt.Errorf("parsing %s: %w", path, err)
			}
			objects = append(objects, o)
		}
	})
	if err != nil {
		t.Fatalf("reading the objects of %s: %v", baseDir, err)
	}
	return objects
}
