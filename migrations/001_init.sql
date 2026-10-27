CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE devices (
    udid           TEXT PRIMARY KEY,
    name           TEXT    NOT NULL DEFAULT '',
    product_type   TEXT    NOT NULL DEFAULT '',
    ios            TEXT    NOT NULL DEFAULT '',
    encrypted      INTEGER NOT NULL DEFAULT -1, -- -1 unknown, 0 no, 1 yes
    interval_hours INTEGER NOT NULL DEFAULT 72,
    window_start   TEXT    NOT NULL DEFAULT '18:00',
    window_end     TEXT    NOT NULL DEFAULT '22:00',
    keep_versions  INTEGER NOT NULL DEFAULT 7,
    info_at        INTEGER NOT NULL DEFAULT 0,
    last_seen      INTEGER NOT NULL DEFAULT 0,
    last_reminded  INTEGER NOT NULL DEFAULT 0,
    stale_alerted  INTEGER NOT NULL DEFAULT 0,
    created_at     INTEGER NOT NULL
);

CREATE TABLE jobs (
    id          INTEGER PRIMARY KEY,
    udid        TEXT    NOT NULL,
    trigger     TEXT    NOT NULL,
    status      TEXT    NOT NULL, -- queued, running, ok, failed, nopasscode
    started_at  INTEGER NOT NULL DEFAULT 0,
    finished_at INTEGER NOT NULL DEFAULT 0,
    files       INTEGER NOT NULL DEFAULT 0,
    bytes       INTEGER NOT NULL DEFAULT 0,
    snapshot    TEXT    NOT NULL DEFAULT '',
    error       TEXT    NOT NULL DEFAULT '',
    log         TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL
);
CREATE INDEX jobs_udid ON jobs (udid, id);

CREATE TABLE channels (
    id         INTEGER PRIMARY KEY,
    name       TEXT    NOT NULL,
    url        TEXT    NOT NULL,
    enabled    INTEGER NOT NULL DEFAULT 1,
    on_success INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE action_tokens (
    token      TEXT PRIMARY KEY,
    udid       TEXT    NOT NULL,
    expires_at INTEGER NOT NULL,
    used_at    INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE sessions (
    token      TEXT PRIMARY KEY,
    expires_at INTEGER NOT NULL
);
