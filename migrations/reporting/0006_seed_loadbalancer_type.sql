-- The loadbalancer resource type and the JSON Schema its reported sizes are
-- validated against. The OpenStack reconciliation adapter enumerates Octavia
-- load balancers when include_octavia is set, and the strict-mode pipeline
-- refuses an event that carries a size for a (platform, resource_type) pair no
-- row registers. The pair rides the chain so that every database the chain
-- reaches knows it without an operator registering it first.
--
-- The normative specification is roadmap/01-phase-1-core-platform-openstack.md,
-- WP 1.13.

-- +goose Up
-- x-tally-seed is what makes this document one no operator would have written.
-- JSON Schema 2020-12 ignores a keyword it does not know, so it costs nothing at
-- validation time, and the rollback below deletes the whole document or nothing:
-- openstack/loadbalancer is exactly the pair an operator registered by hand to
-- unblock Octavia collection before this migration existed, and without the
-- marker the document they registered would be this one byte for byte.
INSERT INTO resource_types (platform, resource_type, size_schema) VALUES
    ('openstack', 'loadbalancer', '{
        "type": "object",
        "required": ["listeners", "pools"],
        "properties": {
            "listeners": {"type": "integer", "minimum": 0},
            "pools": {"type": "integer", "minimum": 0}
        },
        "additionalProperties": true,
        "x-tally-seed": 6
    }')
-- A pair an operator registered before this migration reached their database is
-- theirs, not the chain's. Without this the duplicate key would roll the whole
-- migration back, and every pod of the release that needs schema 6 would stay
-- unready until someone deleted rows by hand.
ON CONFLICT (platform, resource_type) DO NOTHING;

-- +goose Down
-- Named by content rather than by key, the way 0002 names its own rows: the Up
-- leaves an operator's row alone, so the Down has to as well.
--
-- The whole document is compared rather than the marker inside it. size_schema
-- is a public, round-trippable field — GET /resource-types/{platform}/{type}
-- answers the stored document verbatim and PUT stores what arrives verbatim — so
-- an operator who fetches this seed, tightens it, and registers it back keeps the
-- marker in a document that is now theirs. Deleting by the marker would take that
-- customization with it and leave the cloud's load-balancer ingest with no
-- schema at all; deleting by content leaves every edited document where it is,
-- and the marker is what keeps a hand-registered one from matching this.
DELETE FROM resource_types
WHERE (platform, resource_type) = ('openstack', 'loadbalancer')
  AND size_schema = '{
        "type": "object",
        "required": ["listeners", "pools"],
        "properties": {
            "listeners": {"type": "integer", "minimum": 0},
            "pools": {"type": "integer", "minimum": 0}
        },
        "additionalProperties": true,
        "x-tally-seed": 6
    }'::jsonb;
