| Job | Interval | Timeout | Targets | Static labels |
| --- | --- | --- | --- | --- |
| `fixture-exporter` | `300s` | `60s` | `fixture-exporter:9180` | `cloud=os-prod-eu1`, `platform=openstack` |
| `fixture-api` | `30s` | none | discovered, role `endpointslice`, kept by `fixture-api;http` | none |
| `fixture-gateway` | `60s` | none | `fixture-gateway:9101` | none |
