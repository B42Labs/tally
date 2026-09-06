| Oslo event type | Tally event type | Resource type | State | Size | Resource id | Project id | Skipped when |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `compute.instance.create.end` | `compute.instance.create.end` | `instance` | `vmState` | `instanceSize` | `instance_id` | `tenant_id` | none |
| `compute.instance.shelve_offload.end` | `compute.instance.shelve` | `instance` | `fixedState("shelved")` | none | `instance_id` | `tenant_id` | none |
| `floatingip.delete.end` | `floatingip.delete.end` | `floating_ip` | none | none | `floatingip.id` or `floatingip_id` | request context | none |
| `octavia.loadbalancer.create.end` | `octavia.loadbalancer.create.end` | `loadbalancer` | `fixedState("active")` | `loadBalancerSize` | `loadbalancer_id` or `id` | `project_id` | none |
| `image.create` | `image.create` | `image` | `fixedState("active")` | `imageSize` | `id` | `owner` | `unsizedImage` |
