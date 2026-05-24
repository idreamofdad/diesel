-- +goose Up
-- messages is the conversation log: one immutable row per chat turn, in
-- insertion order (id). naked is 0/1; timestamp is RFC3339Nano text.
CREATE TABLE messages (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    role      TEXT    NOT NULL,
    content   TEXT    NOT NULL,
    timestamp TEXT    NOT NULL,
    emotion   TEXT    NOT NULL DEFAULT '',
    naked     INTEGER NOT NULL DEFAULT 0
);

-- kv holds whole-blob records that are always read and written as a unit:
-- the settings struct and each bridge's polling bookkeeping, each stored
-- as a JSON value under a fixed key.
CREATE TABLE kv (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- +goose Down
DROP TABLE kv;
DROP TABLE messages;
