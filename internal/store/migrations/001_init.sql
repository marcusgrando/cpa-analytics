CREATE TABLE requests (
    id                     INTEGER PRIMARY KEY,
    request_id             TEXT    NOT NULL UNIQUE,
    trace_id               TEXT    NOT NULL DEFAULT '',
    requested_at           INTEGER NOT NULL,
    provider               TEXT    NOT NULL DEFAULT '',
    executor_type          TEXT    NOT NULL DEFAULT '',
    auth_type              TEXT    NOT NULL DEFAULT '',
    auth_id                TEXT    NOT NULL DEFAULT '',
    model                  TEXT    NOT NULL DEFAULT '',
    alias                  TEXT    NOT NULL DEFAULT '',
    response_model         TEXT    NOT NULL DEFAULT '',
    api_key_hash           TEXT    NOT NULL DEFAULT '',
    access_provider        TEXT    NOT NULL DEFAULT '',
    source                 TEXT    NOT NULL DEFAULT '',
    session_id             TEXT    NOT NULL DEFAULT '',
    parent_session_id      TEXT    NOT NULL DEFAULT '',
    reasoning_effort       TEXT    NOT NULL DEFAULT '',
    service_tier           TEXT    NOT NULL DEFAULT '',
    effective_service_tier TEXT    NOT NULL DEFAULT '',
    response_service_tier  TEXT    NOT NULL DEFAULT '',
    stream                 INTEGER NOT NULL DEFAULT 0,
    generate               INTEGER NOT NULL DEFAULT 1,
    failed                 INTEGER NOT NULL DEFAULT 0,
    status_code            INTEGER NOT NULL DEFAULT 0,
    latency_ms             INTEGER NOT NULL DEFAULT 0,
    ttft_ms                INTEGER NOT NULL DEFAULT 0,
    input_tokens           INTEGER NOT NULL DEFAULT 0,
    output_tokens          INTEGER NOT NULL DEFAULT 0,
    reasoning_tokens       INTEGER NOT NULL DEFAULT 0,
    cached_tokens          INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens      INTEGER NOT NULL DEFAULT 0,
    cache_creation_tokens  INTEGER NOT NULL DEFAULT 0,
    total_tokens           INTEGER NOT NULL DEFAULT 0,
    uncached_input         INTEGER NOT NULL DEFAULT 0,
    cache_read             INTEGER NOT NULL DEFAULT 0,
    cache_write            INTEGER NOT NULL DEFAULT 0,
    output_non_reasoning   INTEGER NOT NULL DEFAULT 0,
    output_reasoning       INTEGER NOT NULL DEFAULT 0,
    unclassified           INTEGER NOT NULL DEFAULT 0,
    accounting_quality     TEXT    NOT NULL DEFAULT '',
    user_agent             TEXT    NOT NULL DEFAULT '',
    client_name            TEXT    NOT NULL DEFAULT '',
    client_version         TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX requests_requested_at ON requests (requested_at);
CREATE INDEX requests_model_time ON requests (model, requested_at);
CREATE INDEX requests_client_time ON requests (client_name, client_version, requested_at);
CREATE INDEX requests_trace ON requests (trace_id);

CREATE TABLE kv (
    key   TEXT PRIMARY KEY,
    value BLOB NOT NULL
);
