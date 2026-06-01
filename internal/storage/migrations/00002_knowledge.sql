-- +goose Up
-- The knowledge graph: a small, persistent memory the model edits through
-- MCP tools and reads back as a JSON blob injected into the system prompt.
-- Three normalized tables with foreign-key cascades so deleting an entity
-- can't leave dangling observations or relations behind. Foreign-key
-- enforcement is enabled per-connection via the DSN pragma in storage.go —
-- without it SQLite parses these REFERENCES clauses but never acts on them.

-- kg_entities is one row per node. name is the natural key the model uses
-- to refer to a node everywhere (relations point at names, not ids), so it
-- is the primary key: a second create_entities for the same name merges
-- rather than duplicating. type is free-form ("person", "cat", …).
CREATE TABLE kg_entities (
    name TEXT PRIMARY KEY,
    type TEXT NOT NULL
);

-- kg_observations are the free-text facts attached to an entity. The
-- (entity_name, content) pair is unique so re-adding the same observation
-- is idempotent rather than piling up duplicates.
CREATE TABLE kg_observations (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    entity_name TEXT NOT NULL REFERENCES kg_entities(name) ON DELETE CASCADE,
    content     TEXT NOT NULL,
    UNIQUE (entity_name, content)
);

-- kg_relations are directed, typed edges in active voice (from -owns-> to).
-- Both endpoints cascade-delete with their entity. The triple is unique so
-- the same edge can't be stored twice.
CREATE TABLE kg_relations (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    from_name     TEXT NOT NULL REFERENCES kg_entities(name) ON DELETE CASCADE,
    to_name       TEXT NOT NULL REFERENCES kg_entities(name) ON DELETE CASCADE,
    relation_type TEXT NOT NULL,
    UNIQUE (from_name, to_name, relation_type)
);

CREATE INDEX idx_kg_observations_entity ON kg_observations (entity_name);
CREATE INDEX idx_kg_relations_from ON kg_relations (from_name);
CREATE INDEX idx_kg_relations_to ON kg_relations (to_name);

-- +goose Down
DROP TABLE kg_relations;
DROP TABLE kg_observations;
DROP TABLE kg_entities;
