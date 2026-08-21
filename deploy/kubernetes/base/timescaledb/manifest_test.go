// This file pins the wiring that creates the engine's database on a fresh
// cluster. It fails quietly: a StatefulSet that lost the initdb mount, a
// ConfigMap that stopped carrying the script, or a script that no longer names
// the database starts a Postgres which passes both probes and answers every
// reporting query, and the missing database surfaces one step later as
// `make migrate` dying on it. The test reads the YAML from disk and needs no
// cluster.
package timescaledb_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	manifestFile      = "timescaledb.yaml"
	kustomizationFile = "kustomization.yaml"
	initdbScript      = "01-create-engine-db.sql"

	// The name the kustomization generates and the manifest mounts. kustomize
	// appends a content hash to it in the rendered output, so this is the name
	// both files write rather than the one the cluster sees.
	initdbConfigMap = "timescaledb-initdb"

	// Where the Postgres entrypoint looks for the scripts it runs, and the path
	// our one script takes inside it. The mount names that file rather than the
	// directory, because a directory mount hides what the image ships there.
	initdbDir  = "/docker-entrypoint-initdb.d"
	initdbPath = initdbDir + "/" + initdbScript
)

// object is the part of a manifest document this test asserts over. yaml.v3
// ignores every field not named here, so one shape covers all three kinds.
type object struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		// StatefulSet
		Template struct {
			Spec struct {
				Containers []container `yaml:"containers"`
				Volumes    []volume    `yaml:"volumes"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

type container struct {
	Name         string        `yaml:"name"`
	VolumeMounts []volumeMount `yaml:"volumeMounts"`
}

type volumeMount struct {
	Name      string `yaml:"name"`
	MountPath string `yaml:"mountPath"`
	SubPath   string `yaml:"subPath"`
}

type volume struct {
	Name      string `yaml:"name"`
	ConfigMap struct {
		Name string `yaml:"name"`
	} `yaml:"configMap"`
}

func TestInitdbScriptCreatesTheEngineDatabase(t *testing.T) {
	// The engine chain runs against tally_engine, and nothing else creates it:
	// POSTGRES_DB names the reporting database alone, and the engine CLI
	// migrates a database rather than creating one.
	const stmt = "CREATE DATABASE tally_engine"

	raw, err := os.ReadFile(initdbScript)
	if err != nil {
		t.Fatalf("reading %s: %v", initdbScript, err)
	}
	if !strings.Contains(string(raw), stmt) {
		t.Errorf("%s does not contain %q, so a fresh cluster starts without the engine's database and `make migrate` fails on it",
			initdbScript, stmt)
	}
}

func TestKustomizationGeneratesTheInitdbConfigMap(t *testing.T) {
	// Without the generator entry the script stays a file in the repository
	// that no rendered manifest carries, and the volume below references a
	// ConfigMap that does not exist.
	var k struct {
		ConfigMapGenerator []struct {
			Name  string   `yaml:"name"`
			Files []string `yaml:"files"`
		} `yaml:"configMapGenerator"`
	}

	raw, err := os.ReadFile(kustomizationFile)
	if err != nil {
		t.Fatalf("reading %s: %v", kustomizationFile, err)
	}
	if err := yaml.Unmarshal(raw, &k); err != nil {
		t.Fatalf("parsing %s, which kustomize refuses to build: %v", kustomizationFile, err)
	}

	for _, gen := range k.ConfigMapGenerator {
		if gen.Name != initdbConfigMap {
			continue
		}
		for _, f := range gen.Files {
			if f == initdbScript {
				return
			}
		}
		t.Fatalf("the %s generator carries %v rather than %s, so the mounted ConfigMap holds no script and Postgres initializes with the reporting database alone",
			initdbConfigMap, gen.Files, initdbScript)
	}
	t.Fatalf("%s declares no configMapGenerator named %s, so the volume in %s references a ConfigMap nothing creates and the pod never starts",
		kustomizationFile, initdbConfigMap, manifestFile)
}

func TestInitdbConfigMapIsMounted(t *testing.T) {
	// The pair is what matters, not either half: a volume no container mounts
	// leaves the script outside the pod's filesystem, and a mount naming a
	// volume the pod does not declare stops the pod from starting at all.
	sts := objectNamed(t, objects(t, manifestFile), "StatefulSet", "timescaledb")

	var name string
	for _, v := range sts.Spec.Template.Spec.Volumes {
		if v.ConfigMap.Name == initdbConfigMap {
			name = v.Name
			break
		}
	}
	if name == "" {
		t.Fatalf("the timescaledb pod declares no volume backed by configMap %s, so the initdb script never reaches the container",
			initdbConfigMap)
	}

	c := containerNamed(t, sts, "timescaledb")
	for _, m := range c.VolumeMounts {
		if m.Name != name {
			continue
		}
		if m.MountPath != initdbPath || m.SubPath != initdbScript {
			t.Errorf("volume %q is mounted at %q with subPath %q, want %q with subPath %s; a whole-directory mount over %s hides the initdb scripts the image ships there, which install the timescaledb extension, so a fresh cluster comes up without it and the reporting chain refuses to migrate",
				name, m.MountPath, m.SubPath, initdbPath, initdbScript, initdbDir)
		}
		return
	}
	t.Errorf("the timescaledb container mounts no volume %q, so the script sits in the pod's spec without reaching %s and tally_engine is never created",
		name, initdbDir)
}

// objects decodes every document of one manifest file.
func objects(t *testing.T, path string) []object {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the manifest: %v", err)
	}

	var docs []object
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	for {
		var doc object
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return docs
		}
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		docs = append(docs, doc)
	}
}

// objectNamed returns one document, failing if it is missing rather than
// asserting over a zero value.
func objectNamed(t *testing.T, docs []object, kind, name string) object {
	t.Helper()

	for _, doc := range docs {
		if doc.Kind == kind && doc.Metadata.Name == name {
			return doc
		}
	}
	t.Fatalf("no %s named %q", kind, name)
	return object{}
}

// containerNamed returns one container of one workload.
func containerNamed(t *testing.T, doc object, name string) container {
	t.Helper()

	for _, c := range doc.Spec.Template.Spec.Containers {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("%s %s carries no container %q", doc.Kind, doc.Metadata.Name, name)
	return container{}
}
