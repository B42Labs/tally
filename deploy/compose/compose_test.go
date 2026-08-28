// This file pins the compose stack to what the two binaries in it read. Every
// mismatch it looks for fails quietly. A variable no binary reads leaves the
// container starting on the default the variable was meant to replace, a buffer
// path outside the outbox volume puts the undelivered events in the writable
// layer that `docker compose down` drops, a missing extra_hosts entry sends
// every flush to a name the container's resolver does not know, and a host port
// Docker Desktop cannot publish fails the whole stack rather than the service
// that asked for it. The test reads the YAML from disk and starts no container.
package compose_test

import (
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/b42labs/tally/internal/providers/openstack"
	"github.com/b42labs/tally/internal/providers/openstack/simulator"
)

const (
	composePath = "compose.yaml"

	// The services, by the names the rest of this file refers to them by.
	rabbitmqService  = "rabbitmq"
	collectorService = "collector"
	simulatorService = "simulator"

	// The volume the collector's outbox lives on, and the file the dev CA is
	// mounted from.
	outboxVolume = "outbox"
	caSource     = "../../tally-ca.crt"

	// The Reporting API of the dev cluster as the collector reaches it: the name
	// is mapped to the host by extra_hosts, and kind publishes the Gateway's
	// https port there.
	gatewayHost  = "api.tally.127-0-0-1.nip.io"
	reportingURL = "https://" + gatewayHost + ":8443"

	// Every variable Tally itself reads carries this prefix. The environment maps
	// below also hold variables of the base image, which no EnvNames list knows.
	tallyPrefix = "TALLY_"

	// The address every published port binds, and the lowest port Docker Desktop
	// publishes without the privileged-port helper.
	loopback           = "127.0.0.1"
	lowestUnprivileged = 1024
)

// composeFile is the part of the stack these tests assert over. yaml.v3 ignores
// every field not named here.
type composeFile struct {
	Name     string
	Services map[string]service
	Volumes  map[string]any
}

type service struct {
	Image       string
	PullPolicy  string `yaml:"pull_policy"`
	Ports       []string
	Environment map[string]string
	Volumes     []string
	ExtraHosts  []string `yaml:"extra_hosts"`
	Command     []string
	DependsOn   map[string]struct {
		Condition string
	} `yaml:"depends_on"`
}

func TestComposeRunsTheThreeServices(t *testing.T) {
	// The three are the pipeline: the simulator publishes onto the broker, the
	// collector consumes from it. A renamed or dropped service leaves compose
	// starting a stack with one end of that missing, and it starts cleanly. Both
	// Tally images are built locally, so a pull policy other than never sends
	// compose to a registry that carries neither.
	file := loadCompose(t)

	names := slices.Sorted(maps.Keys(file.Services))
	want := []string{collectorService, rabbitmqService, simulatorService}
	if !slices.Equal(names, want) {
		t.Fatalf("services = %v, want %v", names, want)
	}

	images := map[string]string{
		collectorService: "tally-openstack-collector:dev",
		simulatorService: "tally-openstack-simulator:dev",
	}
	for name, image := range images {
		svc := file.Services[name]
		if svc.Image != image {
			t.Errorf("%s image = %q, want %q, which is the tag make images builds", name, svc.Image, image)
		}
		if svc.PullPolicy != "never" {
			t.Errorf("%s pull_policy = %q, want %q; the image exists on this machine alone", name, svc.PullPolicy, "never")
		}
	}
	if image := file.Services[rabbitmqService].Image; !strings.HasPrefix(image, "rabbitmq:") {
		t.Errorf("%s image = %q, want a rabbitmq: tag", rabbitmqService, image)
	}

	for _, name := range []string{collectorService, simulatorService} {
		dependency, ok := file.Services[name].DependsOn[rabbitmqService]
		if !ok {
			t.Errorf("%s does not depend on %s, so it dials a broker that may not exist yet", name, rabbitmqService)
			continue
		}
		if condition := "service_healthy"; dependency.Condition != condition {
			t.Errorf("%s depends on %s with condition %q, want %q; a started container is not yet a broker that answers",
				name, rabbitmqService, dependency.Condition, condition)
		}
	}

	if _, ok := file.Volumes[outboxVolume]; !ok {
		t.Errorf("volumes = %v, want the %q volume declared, which is what makes the outbox outlive the container",
			file.Volumes, outboxVolume)
	}
}

func TestCollectorEnvironmentIsWhatTheCollectorReads(t *testing.T) {
	// The collector reads its configuration from the environment and ignores
	// what it does not know, so a misspelled variable is a default in place of a
	// setting: no broker, no cloud, or an outbox in the container.
	svc := serviceNamed(t, loadCompose(t), collectorService)

	for name := range svc.Environment {
		if strings.HasPrefix(name, tallyPrefix) && !slices.Contains(openstack.EnvNames, name) {
			t.Errorf("%s is set although the collector reads no variable of that name (openstack.EnvNames), so its value never arrives", name)
		}
	}

	outbox := mountFrom(t, svc, outboxVolume)
	if path := svc.Environment["TALLY_OSC_BUFFER_PATH"]; !strings.HasPrefix(path, outbox.target+"/") {
		t.Errorf("TALLY_OSC_BUFFER_PATH = %q, want a path under %q, where the %s volume is mounted; anywhere else the events the collector has not delivered go with the container",
			path, outbox.target, outboxVolume)
	}

	ca := mountFrom(t, svc, caSource)
	if path := svc.Environment["SSL_CERT_FILE"]; path != ca.target {
		t.Errorf("SSL_CERT_FILE = %q, want %q, where %s is mounted; without it the sender rejects the Gateway's certificate on every flush",
			path, ca.target, caSource)
	}
	if ca.options != "ro" {
		t.Errorf("the %s mount carries the options %q, want %q; the collector has no reason to write the CA", caSource, ca.options, "ro")
	}

	if hosts := []string{gatewayHost + ":host-gateway"}; !slices.Equal(svc.ExtraHosts, hosts) {
		t.Errorf("extra_hosts = %v, want %v; the name is otherwise resolved in the container's network, where the dev cluster is not",
			svc.ExtraHosts, hosts)
	}
	if url := svc.Environment["TALLY_OSC_REPORTING_URL"]; !strings.HasPrefix(url, reportingURL) {
		t.Errorf("TALLY_OSC_REPORTING_URL = %q, want it to start with %q, which is the name extra_hosts maps and the port kind publishes on the host",
			url, reportingURL)
	}
}

func TestSimulatorEnvironmentIsWhatTheSimulatorReads(t *testing.T) {
	// The simulator ignores an unknown variable the same way the collector does,
	// and a run without the broker in its environment writes the month nowhere
	// the collector looks.
	svc := serviceNamed(t, loadCompose(t), simulatorService)

	for name := range svc.Environment {
		if strings.HasPrefix(name, tallyPrefix) && !slices.Contains(simulator.EnvNames, name) {
			t.Errorf("%s is set although the simulator reads no variable of that name (simulator.EnvNames), so its value never arrives", name)
		}
	}

	if len(svc.Command) == 0 || svc.Command[0] != "run" {
		t.Fatalf("command = %v, want it to start with %q; the entrypoint's root command prints its help text and exits zero, which is a container that succeeds without publishing a notification",
			svc.Command, "run")
	}
	// The period is required by the binary, but the seed and the factor are not:
	// dropping either leaves a month that is not the one asked for, published at
	// a pace nobody chose. Without --allow-remote-broker the container refuses
	// the broker beside it, which is a stack that starts and publishes nothing.
	for _, flag := range []string{"--period", "--seed", "--factor", "--allow-remote-broker"} {
		if !slices.Contains(svc.Command, flag) {
			t.Errorf("command = %v, want %s among the arguments", svc.Command, flag)
		}
	}
}

func TestHostPortsAreLoopbackAndUnprivileged(t *testing.T) {
	// Docker Desktop publishes a privileged port only through a helper that is
	// not installed on every Mac, and a mapping without an address publishes on
	// every interface the machine has, which puts a broker holding the guest
	// password on whatever network it is joined to. deploy/kind/kind.yaml holds
	// the cluster's own ports to the same rule.
	file := loadCompose(t)

	for name, svc := range file.Services {
		if len(svc.Ports) == 0 {
			t.Errorf("%s publishes no port, so this check passes over a service that may still map one later", name)
		}
		for _, entry := range svc.Ports {
			parts := strings.Split(entry, ":")
			if len(parts) != 3 {
				t.Errorf("%s port %q, want the address:host:container form; a mapping without an address binds every interface", name, entry)
				continue
			}
			if parts[0] != loopback {
				t.Errorf("%s port %q binds %q, want %q", name, entry, parts[0], loopback)
			}
			port, err := strconv.Atoi(parts[1])
			if err != nil {
				t.Errorf("%s port %q: the host port %q is not a number: %v", name, entry, parts[1], err)
				continue
			}
			if port < lowestUnprivileged {
				t.Errorf("%s port %q publishes on host port %d, want %d or above", name, entry, port, lowestUnprivileged)
			}
		}
	}
}

// loadCompose decodes the stack from disk.
func loadCompose(t *testing.T) composeFile {
	t.Helper()

	raw, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatalf("reading the compose file: %v", err)
	}

	var file composeFile
	if err := yaml.Unmarshal(raw, &file); err != nil {
		t.Fatalf("parsing %s, which compose refuses to start: %v", composePath, err)
	}
	return file
}

// serviceNamed returns one service, failing when it is missing rather than
// asserting over a zero value.
func serviceNamed(t *testing.T, file composeFile, name string) service {
	t.Helper()

	svc, ok := file.Services[name]
	if !ok {
		t.Fatalf("%s declares no service %q", composePath, name)
	}
	return svc
}

// mount is one entry of a service's volumes list, source:target[:options].
type mount struct {
	source  string
	target  string
	options string
}

// mountFrom returns the mount of one source, failing when the service carries
// none from it.
func mountFrom(t *testing.T, svc service, source string) mount {
	t.Helper()

	for _, entry := range svc.Volumes {
		parts := strings.SplitN(entry, ":", 3)
		if parts[0] != source {
			continue
		}
		m := mount{source: parts[0]}
		if len(parts) > 1 {
			m.target = parts[1]
		}
		if len(parts) > 2 {
			m.options = parts[2]
		}
		return m
	}
	t.Fatalf("volumes = %v, none of them mounting %s", svc.Volumes, source)
	return mount{}
}
