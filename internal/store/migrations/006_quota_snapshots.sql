-- Polled subscription quota snapshots for providers that do not return limit
-- usage in response headers (such as Antigravity). Each row captures one
-- window (session or weekly) of one model group for one account at recorded_at
-- (Unix milliseconds). reset_at is NULL when the window has not started.
CREATE TABLE quota_snapshots (
    id          INTEGER PRIMARY KEY,
    recorded_at INTEGER NOT NULL,
    provider    TEXT    NOT NULL,
    auth_index  TEXT    NOT NULL,
    group_id    TEXT    NOT NULL,
    group_label TEXT    NOT NULL DEFAULT '',
    window_kind TEXT    NOT NULL,
    used_pct    REAL    NOT NULL,
    reset_at    INTEGER
);

CREATE INDEX quota_snapshots_lookup ON quota_snapshots (provider, auth_index, group_id, window_kind, recorded_at);
CREATE INDEX quota_snapshots_time ON quota_snapshots (recorded_at);
