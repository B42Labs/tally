# Dev loop: rebuild the image on change, side-load it into kind, redeploy.
#
# Prerequisite: `make up` has created the cluster and installed the add-ons.
# Tilt only owns what the dev overlay deploys.

allow_k8s_contexts('kind-tally')

docker_build(
    'tally-reporting',
    context='.',
    build_args={'CMD': 'tally-reporting'},
    # Only a Go change or a migration can alter the binary, which embeds the
    # migration chain, so nothing else triggers a rebuild.
    only=['go.mod', 'go.sum', 'cmd', 'internal', 'migrations', 'Dockerfile'],
)

k8s_yaml(kustomize('deploy/kubernetes/overlays/dev'))

# Every service is reachable through the Gateway, so no port-forward is
# configured: the links below are the real URLs. They carry :8443 because kind
# publishes https there rather than on 443 — see deploy/kind/kind.yaml.
# The health probes are not published through the Gateway, so the link is the
# API surface itself. Until the first endpoint lands it answers a 404 problem
# document, which still says the service is reachable.
k8s_resource(
    'reporting-api',
    labels=['tally'],
    links=[link('https://api.tally.127-0-0-1.nip.io:8443/api/v1', 'API')],
)
k8s_resource('timescaledb', labels=['infrastructure'])
k8s_resource(
    'victoriametrics',
    labels=['infrastructure'],
    links=[link('https://vm.tally.127-0-0-1.nip.io:8443/vmui/', 'UI')],
)
k8s_resource(
    'otel-collector',
    labels=['infrastructure'],
    links=[link('https://otlp.tally.127-0-0-1.nip.io:8443', 'OTLP/HTTP')],
)
k8s_resource(
    'grafana',
    labels=['infrastructure'],
    links=[link('https://grafana.tally.127-0-0-1.nip.io:8443', 'Grafana')],
)
k8s_resource(
    'alertmanager',
    labels=['infrastructure'],
    links=[link('https://alertmanager.tally.127-0-0-1.nip.io:8443', 'Alertmanager')],
)
k8s_resource(
    'vmalert',
    labels=['infrastructure'],
    links=[link('https://vmalert.tally.127-0-0-1.nip.io:8443/vmalert/', 'vmalert')],
)
