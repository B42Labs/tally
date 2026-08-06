-- The four OpenStack resource types of Phase 1 and the JSON Schemas their
-- reported sizes are validated against. They ride the chain so that every
-- database the chain reaches knows the types the collector emits without an
-- operator registering them first.
--
-- The normative specification is roadmap/01-phase-1-core-platform-openstack.md,
-- WP 1.5.

-- +goose Up
INSERT INTO resource_types (platform, resource_type, size_schema) VALUES
    ('openstack', 'instance', '{
        "type": "object",
        "required": ["vcpus", "ram_gb", "disk_gb", "flavor"],
        "properties": {
            "vcpus": {"type": "integer", "minimum": 1},
            "ram_gb": {"type": "number", "exclusiveMinimum": 0},
            "disk_gb": {"type": "number", "minimum": 0},
            "flavor": {"type": "string"}
        },
        "additionalProperties": true
    }'),
    ('openstack', 'volume', '{
        "type": "object",
        "required": ["size_gb", "type"],
        "properties": {
            "size_gb": {"type": "number", "exclusiveMinimum": 0},
            "type": {"type": "string"}
        },
        "additionalProperties": true
    }'),
    ('openstack', 'floating_ip', '{
        "type": "object",
        "required": ["ip_version"],
        "properties": {
            "ip_version": {"enum": [4, 6]}
        },
        "additionalProperties": true
    }'),
    ('openstack', 'image', '{
        "type": "object",
        "required": ["size_gb"],
        "properties": {
            "size_gb": {"type": "number", "minimum": 0}
        },
        "additionalProperties": true
    }')
-- A pair an operator registered before this migration reached their database is
-- theirs, not the chain's. Without this the duplicate key would roll the whole
-- migration back, and every pod of the release that needs schema 2 would stay
-- unready until someone deleted rows by hand.
ON CONFLICT (platform, resource_type) DO NOTHING;

-- +goose Down
-- Named row by row rather than by platform, and by content rather than by key:
-- a seed an operator replaced through PUT /resource-types is theirs, and
-- nothing anywhere holds the document this would delete.
DELETE FROM resource_types
WHERE (platform, resource_type) = ('openstack', 'instance')
  AND size_schema = '{
        "type": "object",
        "required": ["vcpus", "ram_gb", "disk_gb", "flavor"],
        "properties": {
            "vcpus": {"type": "integer", "minimum": 1},
            "ram_gb": {"type": "number", "exclusiveMinimum": 0},
            "disk_gb": {"type": "number", "minimum": 0},
            "flavor": {"type": "string"}
        },
        "additionalProperties": true
    }'::jsonb;

DELETE FROM resource_types
WHERE (platform, resource_type) = ('openstack', 'volume')
  AND size_schema = '{
        "type": "object",
        "required": ["size_gb", "type"],
        "properties": {
            "size_gb": {"type": "number", "exclusiveMinimum": 0},
            "type": {"type": "string"}
        },
        "additionalProperties": true
    }'::jsonb;

DELETE FROM resource_types
WHERE (platform, resource_type) = ('openstack', 'floating_ip')
  AND size_schema = '{
        "type": "object",
        "required": ["ip_version"],
        "properties": {
            "ip_version": {"enum": [4, 6]}
        },
        "additionalProperties": true
    }'::jsonb;

DELETE FROM resource_types
WHERE (platform, resource_type) = ('openstack', 'image')
  AND size_schema = '{
        "type": "object",
        "required": ["size_gb"],
        "properties": {
            "size_gb": {"type": "number", "minimum": 0}
        },
        "additionalProperties": true
    }'::jsonb;
