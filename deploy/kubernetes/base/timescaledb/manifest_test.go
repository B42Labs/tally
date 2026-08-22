// This file pins the wiring that creates the engine's database and the roles it
// reads the reporting database through on a fresh cluster. It fails quietly: a
// StatefulSet that lost an initdb mount, a ConfigMap that stopped carrying a
// script, or a script that no longer names what it creates starts a Postgres
// which passes both probes and answers every reporting query, and what is
// missing surfaces one step later as `make migrate` or the engine dying on it.
// The test reads the YAML from disk and needs no cluster.
package timescaledb_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	manifestFile      = "timescaledb.yaml"
	kustomizationFile = "kustomization.yaml"

	// The name the kustomization generates and the manifest mounts. kustomize
	// appends a content hash to it in the rendered output, so this is the name
	// both files write rather than the one the cluster sees.
	initdbConfigMap = "timescaledb-initdb"

	// Where the Postgres entrypoint looks for the scripts it runs. Each mount
	// names one file inside it rather than the directory, because a directory
	// mount hides what the image ships there.
	initdbDir = "/docker-entrypoint-initdb.d"
)

// initdbScripts is every script this directory hands the Postgres entrypoint,
// in the order the entrypoint runs them. The tests below walk the list, so a
// script added to the directory belongs here and nowhere else in this file.
var initdbScripts = []string{
	"01-create-engine-db.sql",
	"02-create-engine-reader.sh",
}

// initdbPath is where one script is mounted inside the container.
func initdbPath(script string) string { return initdbDir + "/" + script }

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
	Env          []envVar      `yaml:"env"`
	VolumeMounts []volumeMount `yaml:"volumeMounts"`
}

type envVar struct {
	Name      string `yaml:"name"`
	Value     string `yaml:"value"`
	ValueFrom struct {
		SecretKeyRef struct {
			Name     string `yaml:"name"`
			Key      string `yaml:"key"`
			Optional bool   `yaml:"optional"`
		} `yaml:"secretKeyRef"`
	} `yaml:"valueFrom"`
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

	script := initdbScripts[0]
	if !strings.Contains(readScript(t, script), stmt) {
		t.Errorf("%s does not contain %q, so a fresh cluster starts without the engine's database and `make migrate` fails on it",
			script, stmt)
	}
}

func TestInitdbScriptCreatesTheReaderRole(t *testing.T) {
	// Migration 0008 of the reporting chain grants SELECT to
	// tally_engine_reader and creates that role only when it is missing, so the
	// login role the engine connects as has to be a member of it by the time
	// the chain runs.
	script := initdbScripts[1]
	content := readScript(t, script)
	for _, stmt := range []string{
		"CREATE ROLE tally_engine_reader",
		"GRANT tally_engine_reader TO tally_engine",
	} {
		if !strings.Contains(content, stmt) {
			t.Errorf("%s does not contain %q, so the engine connects as a role the reporting grants never reach and every metering read ends in permission denied",
				script, stmt)
		}
	}
}

func TestInitdbScriptTakesTheLoginPasswordFromTheSecret(t *testing.T) {
	// This directory is the deployment surface dev and prod share, so a
	// password written into the script is the password of the login role every
	// deployment built on the base creates, published in the repository, on a
	// database the Gateway publishes a TCP listener for. It has to come from
	// the secret, whose value an overlay sets.
	const passwordEnv = "TALLY_ENGINE_PASSWORD"

	script := initdbScripts[1]
	content := readScript(t, script)
	// psql's :'name' quotes the value it was handed, so the password stays a
	// string literal whatever it contains. Pasting the variable into the SQL
	// text instead would let the first apostrophe of a generated password end
	// that literal, and a chosen one continue as statements the superuser this
	// runs as executes.
	for _, want := range []string{
		"--set=engine_password=\"$" + passwordEnv + "\"",
		"PASSWORD :'engine_password'",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("%s does not contain %q, so the login role it creates carries a password from somewhere other than the deployment's secret, quoted by psql",
				script, want)
		}
	}
	if interpolated := "'${" + passwordEnv + "}'"; strings.Contains(content, interpolated) {
		t.Errorf("%s contains %q, which the shell expands into the SQL text unescaped: an apostrophe in the secret ends the literal and what follows it runs as a statement",
			script, interpolated)
	}
	// The dev overlay's literal, which is the one this script used to carry.
	if literal := "tally-dev-password"; strings.Contains(content, literal) {
		t.Errorf("%s contains the literal %q, which every deployment built on this base would create the login role with",
			script, literal)
	}

	c := containerNamed(t, objectNamed(t, objects(t, manifestFile), "StatefulSet", "timescaledb"), "timescaledb")
	i := slices.IndexFunc(c.Env, func(e envVar) bool { return e.Name == passwordEnv })
	if i < 0 {
		t.Fatalf("the timescaledb container declares no %s, so the script above meets an empty password and initdb fails", passwordEnv)
	}
	if env := c.Env[i]; env.Value != "" || env.ValueFrom.SecretKeyRef.Name == "" || env.ValueFrom.SecretKeyRef.Key == "" {
		t.Errorf("%s = %+v, want it read from a secretKeyRef rather than written into the manifest", passwordEnv, env)
	}
	// kubelet resolves the reference on every container start, while the script
	// that reads it runs against an empty data directory alone. A required
	// reference to a key an overlay's secret does not carry would hold the pod
	// in CreateContainerConfigError and take the reporting database with it,
	// over a value that start would not have read.
	if !c.Env[i].ValueFrom.SecretKeyRef.Optional {
		t.Errorf("%s references its secret key as required, so a secret without that key stops the database from starting at all rather than only the initdb script that reads it",
			passwordEnv)
	}
}

func TestKustomizationGeneratesTheInitdbConfigMap(t *testing.T) {
	// Without a generator entry the script stays a file in the repository that
	// no rendered manifest carries, and the volume below references a ConfigMap
	// that is missing what the mounts below expect of it.
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
		for _, script := range initdbScripts {
			if !slices.Contains(gen.Files, script) {
				t.Errorf("the %s generator carries %v rather than %s, so the mounted ConfigMap holds no such key and Postgres initializes without what that script creates",
					initdbConfigMap, gen.Files, script)
			}
		}
		return
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
	for _, script := range initdbScripts {
		want := initdbPath(script)
		mounted := slices.ContainsFunc(c.VolumeMounts, func(m volumeMount) bool {
			return m.Name == name && m.MountPath == want && m.SubPath == script
		})
		if !mounted {
			t.Errorf("the timescaledb container carries %v rather than a mount of volume %q at %q with subPath %s, so that script never reaches %s and what it creates is missing from a fresh cluster; mounting the volume as a directory instead hides the initdb scripts the image ships there, which install the timescaledb extension",
				c.VolumeMounts, name, want, script, initdbDir)
		}
	}
}

// readScript returns the content of one initdb script.
func readScript(t *testing.T, script string) string {
	t.Helper()

	raw, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("reading %s: %v", script, err)
	}
	return string(raw)
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
