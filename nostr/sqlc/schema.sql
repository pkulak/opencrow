CREATE TABLE IF NOT EXISTS seen_rumors (
    id   TEXT PRIMARY KEY,
    seen INTEGER NOT NULL  -- unix timestamp
);

CREATE INDEX IF NOT EXISTS idx_seen_rumors_seen ON seen_rumors (seen);

CREATE TABLE IF NOT EXISTS publish_queue (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    event_json TEXT    NOT NULL, -- signed Nostr event as JSON
    relays     TEXT    NOT NULL, -- newline-separated relay URLs
    created_at INTEGER NOT NULL  -- unix timestamp for ordering
);

-- Tracks what metadata was last published and when, so we skip
-- re-publishing when the content + target relays haven't changed
-- and the last publish is recent enough. Prevents relay spam during
-- rapid service restarts while still refreshing periodically in case
-- relays dropped the data.
CREATE TABLE IF NOT EXISTS published_metadata (
    kind         INTEGER PRIMARY KEY, -- nostr event kind (0, 10050, etc.)
    hash         TEXT    NOT NULL,     -- SHA-256 of content + target relays
    published_at INTEGER NOT NULL      -- unix timestamp of last publish
);
