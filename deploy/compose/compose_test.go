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
	"net"
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
	// is mapped to the kind node's address by extra_hosts, and the port is the
	// node port of the Gateway's https listener.
	gatewayHost  = "api.tally.127-0-0-1.nip.io"
	gatewayPort  = "30443"
	reportingURL = "https://" + gatewayHost + ":" + gatewayPort

	// The OTLP endpoint of the same Gateway, as the simulator reaches it: a second
	// extra_hosts entry maps this name, and the month's samples are pushed there.
	otlpHost = "otlp.tally.127-0-0-1.nip.io"
	otlpURL  = "https://" + otlpHost + ":" + gatewayPort

	// The network kind creates for its node, the node's address on it as
	// `make simulator-up` writes it into .env, and the name the cluster reaches
	// the simulator by there.
	kindNetwork    = "kind"
	nodeAddress    = "${TALLY_KIND_NODE_IP}"
	simulatorAlias = "tally-openstack-simulator"
	collectorAlias = "tally-openstack-collector"

	// The dev overlay's EnvoyProxy, which pins the node ports of the Gateway's
	// listeners.
	envoyProxyPath = "../kubernetes/overlays/dev/envoyproxy.yaml"

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
	Networks map[string]struct {
		External bool
	}
	Volumes map[string]any
}

type service struct {
	Image       string
	PullPolicy  string `yaml:"pull_policy"`
	Ports       []string
	Environment map[string]string
	Volumes     []string
	ExtraHosts  []string `yaml:"extra_hosts"`
	Networks    map[string]network
	Command     []string
	DependsOn   map[string]struct {
		Condition string
	} `yaml:"depends_on"`
}

// network is a service's entry for one network it joins.
type network struct {
	Aliases []string
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

	if hosts := []string{gatewayHost + ":" + nodeAddress}; !slices.Equal(svc.ExtraHosts, hosts) {
		t.Errorf("extra_hosts = %v, want %v; the name is otherwise resolved in the container's network, where the dev cluster is not",
			svc.ExtraHosts, hosts)
	}
	if url := svc.Environment["TALLY_OSC_REPORTING_URL"]; !strings.HasPrefix(url, reportingURL) {
		t.Errorf("TALLY_OSC_REPORTING_URL = %q, want it to start with %q, which is the name extra_hosts maps to the kind node and the node port of the Gateway's https listener",
			url, reportingURL)
	}
}

func TestCollectorBindsEveryExchangeTheSimulatorPublishesOn(t *testing.T) {
	// A topic exchange copies a message only to the queues bound to it, so a
	// notification published on an exchange the collector never binds is dropped
	// by the broker: no queue, no counter, and no event. The collector's default
	// binds four of the eight the simulator publishes on, which makes the other
	// four this stack's to list.
	svc := serviceNamed(t, loadCompose(t), collectorService)

	bound := strings.Split(svc.Environment["TALLY_OSC_EXCHANGES"], ",")
	slices.Sort(bound)
	if want := slices.Sorted(slices.Values(simulator.ServiceExchanges)); !slices.Equal(bound, want) {
		t.Errorf("TALLY_OSC_EXCHANGES binds %v, want %v, the exchanges the simulator publishes on (simulator.ServiceExchanges); a notification on an unbound exchange reaches no queue and no counter",
			bound, want)
	}
}

func TestCollectorRequiresEveryExchangeBeforeItConsumes(t *testing.T) {
	// The collector and the simulator start in parallel, and the simulator reads
	// one consumer on the collector's queue as a queue that is bound. That holds
	// only for a collector that refuses to consume while an exchange is missing:
	// one that skips missing exchanges and connects first consumes unbound, and
	// the simulator publishes the head of the month into nothing.
	svc := serviceNamed(t, loadCompose(t), collectorService)

	if got := svc.Environment["TALLY_OSC_REQUIRE_EXCHANGES"]; got != "true" {
		t.Errorf("TALLY_OSC_REQUIRE_EXCHANGES = %q, want %q; a collector that connects before the simulator has declared the exchanges would otherwise consume from an unbound queue, and the simulator would take that consumer for a bound one",
			got, "true")
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

	ca := mountFrom(t, svc, caSource)
	if path := svc.Environment["SSL_CERT_FILE"]; path != ca.target {
		t.Errorf("SSL_CERT_FILE = %q, want %q, where %s is mounted; without it the registrar rejects the Gateway's certificate and the run registers nothing",
			path, ca.target, caSource)
	}
	if ca.options != "ro" {
		t.Errorf("the %s mount carries the options %q, want %q; the simulator has no reason to write the CA", caSource, ca.options, "ro")
	}

	if hosts := []string{gatewayHost + ":" + nodeAddress, otlpHost + ":" + nodeAddress}; !slices.Equal(svc.ExtraHosts, hosts) {
		t.Errorf("extra_hosts = %v, want %v; the names are otherwise resolved in the container's network, where the dev cluster is not",
			svc.ExtraHosts, hosts)
	}
	if url := svc.Environment["TALLY_SIM_REPORTING_URL"]; !strings.HasPrefix(url, reportingURL) {
		t.Errorf("TALLY_SIM_REPORTING_URL = %q, want it to start with %q, which is the name extra_hosts maps to the kind node and the node port of the Gateway's https listener",
			url, reportingURL)
	}
	if url := svc.Environment["TALLY_SIM_OTLP_URL"]; !strings.HasPrefix(url, otlpURL) {
		t.Errorf("TALLY_SIM_OTLP_URL = %q, want it to start with %q, which is the name extra_hosts maps to the kind node and the node port of the Gateway's https listener",
			url, otlpURL)
	}
	// A push without the two is refused by the binary before it dials the broker,
	// which is a stack that starts and publishes nothing.
	for _, name := range []string{"TALLY_SIM_OTLP_USER", "TALLY_SIM_OTLP_PASSWORD"} {
		if _, ok := svc.Environment[name]; !ok {
			t.Errorf("environment = %v, want %s among it", svc.Environment, name)
		}
	}

	if len(svc.Command) == 0 || svc.Command[0] != "run" {
		t.Fatalf("command = %v, want it to start with %q; the entrypoint's root command prints its help text and exits zero, which is a container that succeeds without publishing a notification",
			svc.Command, "run")
	}
	// The period is required by the binary, but the seed and the factor are not:
	// dropping either leaves a month that is not the one asked for, published at
	// a pace nobody chose. A dropped --faults leaves SIM_FAULTS with nowhere to
	// arrive, so a stack asked for a fault switch publishes the month without it.
	// A dropped --metrics-interval leaves SIM_METRICS_INTERVAL with nowhere to
	// arrive, so the month is pushed on a grid nobody chose.
	// Without --allow-remote-broker the container refuses the broker beside it,
	// which is a stack that starts and publishes nothing. A dropped
	// --register-projects costs SIM_REGISTER_PROJECTS the same way, and it is
	// checked below rather than in this loop.
	for _, flag := range []string{"--period", "--seed", "--factor", "--metrics-interval", "--faults", "--allow-remote-broker"} {
		if !slices.Contains(svc.Command, flag) {
			t.Errorf("command = %v, want %s among the arguments", svc.Command, flag)
		}
	}
	// Checked by prefix because a boolean flag carries its value in the same
	// argument, and a dropped one leaves SIM_REGISTER_PROJECTS with nowhere to
	// arrive: the stack publishes the month and registers none of it.
	registers := false
	for _, arg := range svc.Command {
		if strings.HasPrefix(arg, "--register-projects=") {
			registers = true
			break
		}
	}
	if !registers {
		t.Errorf("command = %v, want an argument starting with %q", svc.Command, "--register-projects=")
	}
}

func TestCollectorAndSimulatorJoinTheKindNetwork(t *testing.T) {
	// The collector and the simulator reach the Gateway at the kind node's
	// address, and the cluster reaches both by their aliases, all on the
	// network kind creates for its node. A network compose does not take as
	// external is one it creates under the project's name, with no node on it.
	// A service that leaves default loses the broker, and a broker on kind puts
	// the guest password in reach of every pod. A renamed alias is a name no pod
	// resolves, so the scrape jobs and the reconciliation reach nothing.
	file := loadCompose(t)

	if kind, ok := file.Networks[kindNetwork]; !ok || !kind.External {
		t.Errorf("networks = %v, want %q declared external; otherwise compose creates a network of that name for itself, and the kind node is not on it",
			file.Networks, kindNetwork)
	}

	want := []string{"default", kindNetwork}
	for _, name := range []string{collectorService, simulatorService} {
		if got := slices.Sorted(maps.Keys(serviceNamed(t, file, name).Networks)); !slices.Equal(got, want) {
			t.Errorf("%s joins %v, want %v: default carries the broker and %s the cluster", name, got, want, kindNetwork)
		}
	}
	if got := serviceNamed(t, file, rabbitmqService).Networks; len(got) != 0 {
		t.Errorf("%s joins %v, want no networks key, which leaves it on default alone and out of the cluster's reach",
			rabbitmqService, slices.Sorted(maps.Keys(got)))
	}

	if got := serviceNamed(t, file, simulatorService).Networks[kindNetwork].Aliases; !slices.Equal(got, []string{simulatorAlias}) {
		t.Errorf("%s answers on %s to the aliases %v, want [%s], the name the scrape job and the reconciliation reach it by",
			simulatorService, kindNetwork, got, simulatorAlias)
	}
	if got := serviceNamed(t, file, collectorService).Networks[kindNetwork].Aliases; !slices.Equal(got, []string{collectorAlias}) {
		t.Errorf("%s answers on %s to the aliases %v, want [%s], the name the openstack-collector scrape job reaches it by",
			collectorService, kindNetwork, got, collectorAlias)
	}
}

func TestSimulatorListensWhereTheClusterAndTheHostReachIt(t *testing.T) {
	// The cluster dials the simulator's own listener by its alias on the kind
	// network, and the host reaches the same listener through the port compose
	// publishes. A listener on loopback answers neither, and a publish onto
	// another container port reaches nothing: the stack starts cleanly either
	// way while the scrape job stays down and every sync fails to authenticate.
	svc := serviceNamed(t, loadCompose(t), simulatorService)
	cfg := simulatorListener(t, svc.Environment)

	if got, want := cfg.ControlAddr(), net.JoinHostPort("0.0.0.0", strconv.Itoa(cfg.HTTPPort)); got != want {
		t.Errorf("the simulator listens on %s, want %s, every interface of its container", got, want)
	}
	for _, entry := range svc.Ports {
		if port := entry[strings.LastIndex(entry, ":")+1:]; port != strconv.Itoa(cfg.HTTPPort) {
			t.Errorf("%s publishes %q onto the container port %s, want %d, the port the simulator listens on", simulatorService, entry, port, cfg.HTTPPort)
		}
	}
}

func TestGatewayPortIsTheNodePortOfTheHTTPSListener(t *testing.T) {
	// The compose services dial the node itself rather than the host port kind
	// publishes, so the port their URLs name is the node port the dev overlay
	// pins for the https listener. A node port moved there alone leaves every
	// flush on a refused dial, which the collector logs and nothing else reports.
	const httpsPort = 443

	type servicePort struct {
		Port     int `yaml:"port"`
		NodePort int `yaml:"nodePort"`
	}
	var proxy struct {
		Spec struct {
			Provider struct {
				Kubernetes struct {
					EnvoyService struct {
						Patch struct {
							Value struct {
								Spec struct {
									Ports []servicePort `yaml:"ports"`
								} `yaml:"spec"`
							} `yaml:"value"`
						} `yaml:"patch"`
					} `yaml:"envoyService"`
				} `yaml:"kubernetes"`
			} `yaml:"provider"`
		} `yaml:"spec"`
	}
	raw, err := os.ReadFile(envoyProxyPath)
	if err != nil {
		t.Fatalf("reading %s: %v", envoyProxyPath, err)
	}
	if err := yaml.Unmarshal(raw, &proxy); err != nil {
		t.Fatalf("parsing %s: %v", envoyProxyPath, err)
	}

	ports := proxy.Spec.Provider.Kubernetes.EnvoyService.Patch.Value.Spec.Ports
	i := slices.IndexFunc(ports, func(p servicePort) bool { return p.Port == httpsPort })
	if i < 0 {
		t.Fatalf("%s pins no node port for the service port %d, so the https listener's node port is whatever Envoy Gateway allocates", envoyProxyPath, httpsPort)
	}
	if got := strconv.Itoa(ports[i].NodePort); got != gatewayPort {
		t.Errorf("%s pins node port %s for the https listener, want %s, the port %s and %s name",
			envoyProxyPath, got, gatewayPort, reportingURL, otlpURL)
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

// simulatorListener loads the simulator's configuration from the two variables
// of env that place its listener, with every other variable it reads blanked to
// its default, so a value in the developer's shell never reaches it.
func simulatorListener(t *testing.T, env map[string]string) simulator.Config {
	t.Helper()

	for _, name := range simulator.EnvNames {
		t.Setenv(name, "")
	}
	for _, name := range []string{"TALLY_SIM_HTTP_ADDR", "TALLY_SIM_HTTP_PORT"} {
		if value, ok := env[name]; ok {
			t.Setenv(name, value)
		}
	}
	cfg, err := simulator.Load()
	if err != nil {
		t.Fatalf("loading the simulator's configuration from the %s environment: %v", composePath, err)
	}
	return cfg
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
