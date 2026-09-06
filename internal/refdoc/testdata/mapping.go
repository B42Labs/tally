package testdata

// stateRule derives an event's payload state from the notification payload.
type stateRule func(payload map[string]any) string

// sizeBuilder derives an event's payload size object from the notification
// payload.
type sizeBuilder func(payload map[string]any) map[string]any

// mappingEntry is one row of the fixture mapping table.
type mappingEntry struct {
	eventType              string
	resourceType           string
	state                  stateRule
	size                   sizeBuilder
	resourceIDPath         []string
	resourceIDFallbackPath []string
	projectIDPath          []string
	skip                   func(payload map[string]any) bool
}

// mappings is one entry per oslo event type the fixture collector records,
// keyed by the type as the emitting service spells it.
var mappings = map[string]mappingEntry{
	"compute.instance.create.end": {
		eventType:      "compute.instance.create.end",
		resourceType:   "instance",
		state:          vmState,
		size:           instanceSize,
		resourceIDPath: []string{"instance_id"},
		projectIDPath:  []string{"tenant_id"},
	},
	"compute.instance.shelve_offload.end": {
		eventType:      "compute.instance.shelve",
		resourceType:   "instance",
		state:          fixedState("shelved"),
		resourceIDPath: []string{"instance_id"},
		projectIDPath:  []string{"tenant_id"},
	},
	// The delete names the address that is gone at the top level, so the project
	// comes from the request context.
	"floatingip.delete.end": {
		eventType:              "floatingip.delete.end",
		resourceType:           "floating_ip",
		resourceIDPath:         []string{"floatingip", "id"},
		resourceIDFallbackPath: []string{"floatingip_id"},
	},
	"octavia.loadbalancer.create.end": {
		eventType:              "octavia.loadbalancer.create.end",
		resourceType:           "loadbalancer",
		state:                  fixedState("active"),
		size:                   loadBalancerSize,
		resourceIDPath:         []string{"loadbalancer_id"},
		resourceIDFallbackPath: []string{"id"},
		projectIDPath:          []string{"project_id"},
	},
	"image.create": {
		eventType:      "image.create",
		resourceType:   "image",
		state:          fixedState("active"),
		size:           imageSize,
		resourceIDPath: []string{"id"},
		projectIDPath:  []string{"owner"},
		skip:           unsizedImage,
	},
}
