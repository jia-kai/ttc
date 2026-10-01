PRAGMA user_version = 2;

CREATE TABLE workspaces (
    id TEXT PRIMARY KEY NOT NULL,
    path TEXT NOT NULL UNIQUE,
    generation INTEGER NOT NULL DEFAULT 0 CHECK (generation >= 0)
);
CREATE TABLE sessions (
    id TEXT PRIMARY KEY NOT NULL,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id),
    lineage_id TEXT NOT NULL,
    predecessor_id TEXT UNIQUE REFERENCES sessions(id),
    name TEXT NOT NULL,
    name_source TEXT NOT NULL CHECK (name_source IN ('default','auto','manual','continuation')),
    naming_claimed INTEGER NOT NULL DEFAULT 0 CHECK (naming_claimed IN (0,1)),
    read_only INTEGER NOT NULL DEFAULT 0 CHECK (read_only IN (0,1)),
    model_json TEXT NOT NULL CHECK (json_valid(model_json)),
    active_entry_id INTEGER REFERENCES entries(id) DEFERRABLE INITIALLY DEFERRED,
    file_tip_id INTEGER REFERENCES file_changes(id) DEFERRABLE INITIALLY DEFERRED,
    redo_entry_id INTEGER REFERENCES entries(id),
    undo_floor_id INTEGER REFERENCES entries(id),
    observed_generation INTEGER NOT NULL CHECK (observed_generation >= 0),
    last_activity_ms INTEGER NOT NULL,
    metadata_json TEXT NOT NULL CHECK (json_valid(metadata_json))
);
CREATE UNIQUE INDEX writable_lineage ON sessions(lineage_id) WHERE read_only = 0;
CREATE INDEX session_list ON sessions(last_activity_ms DESC, id);
CREATE INDEX session_lineage ON sessions(lineage_id);
CREATE TABLE turns (
    id TEXT PRIMARY KEY NOT NULL,
    session_id TEXT NOT NULL REFERENCES sessions(id),
    trigger TEXT NOT NULL CHECK (trigger IN ('user','async')),
    start_entry_id INTEGER REFERENCES entries(id),
    start_file_tip_id INTEGER REFERENCES file_changes(id),
    status TEXT NOT NULL CHECK (status IN ('running','completed','interrupted','failed')),
    model_json TEXT NOT NULL CHECK (json_valid(model_json)),
    started_ms INTEGER NOT NULL,
    finished_ms INTEGER
);
CREATE UNIQUE INDEX active_turn ON turns(session_id) WHERE status = 'running';
CREATE INDEX turn_history ON turns(session_id, started_ms, id);
CREATE TABLE entries (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL REFERENCES sessions(id),
    parent_id INTEGER REFERENCES entries(id),
    source_id INTEGER REFERENCES entries(id),
    turn_id TEXT REFERENCES turns(id),
    actor_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('message','tool_call','tool_result','status','summary')),
    role TEXT CHECK (role IN ('user','assistant','tool','developer')),
    model_visible INTEGER NOT NULL CHECK (model_visible IN (0,1)),
    content_json TEXT NOT NULL CHECK (json_valid(content_json)),
    file_tip_id INTEGER REFERENCES file_changes(id),
    created_ms INTEGER NOT NULL,
    CHECK (model_visible = 0 OR role IS NOT NULL)
);
CREATE INDEX entry_session ON entries(session_id, id);
CREATE INDEX entry_parent ON entries(parent_id);
CREATE INDEX entry_source ON entries(session_id, source_id);
CREATE TABLE model_requests (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL REFERENCES sessions(id),
    turn_id TEXT REFERENCES turns(id),
    actor_id TEXT NOT NULL,
    purpose TEXT NOT NULL CHECK (purpose IN ('coding','naming','compaction')),
    model_json TEXT NOT NULL CHECK (json_valid(model_json)),
    status TEXT NOT NULL CHECK (status IN ('running','completed','failed','interrupted')),
    attempts_json TEXT NOT NULL CHECK (json_valid(attempts_json)),
    created_ms INTEGER NOT NULL
);
CREATE INDEX request_turn ON model_requests(turn_id, id);
CREATE TABLE tool_calls (
    id TEXT PRIMARY KEY NOT NULL,
    session_id TEXT NOT NULL REFERENCES sessions(id),
    request_id INTEGER NOT NULL REFERENCES model_requests(id),
    provider_call_id TEXT NOT NULL,
    actor_id TEXT NOT NULL,
    name TEXT NOT NULL,
    call_version INTEGER NOT NULL CHECK (call_version > 0),
    call_json TEXT NOT NULL CHECK (json_valid(call_json)),
    result_json TEXT CHECK (result_json IS NULL OR json_valid(result_json)),
    UNIQUE (request_id, provider_call_id)
);
CREATE INDEX tool_session ON tool_calls(session_id, id);
CREATE TRIGGER immutable_result BEFORE UPDATE OF result_json ON tool_calls
WHEN OLD.result_json IS NOT NULL AND NEW.result_json IS NOT OLD.result_json
BEGIN
    SELECT RAISE(ABORT, 'delivered tool result is immutable');
END;
CREATE TABLE tool_records (
    entry_id INTEGER PRIMARY KEY REFERENCES entries(id),
    call_id TEXT NOT NULL REFERENCES tool_calls(id),
    version INTEGER NOT NULL CHECK (version > 0),
    record_json TEXT NOT NULL CHECK (json_valid(record_json)),
    markdown_json TEXT NOT NULL CHECK (json_valid(markdown_json))
);
CREATE INDEX record_call ON tool_records(call_id, entry_id);
CREATE TABLE file_changes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL REFERENCES sessions(id),
    previous_id INTEGER REFERENCES file_changes(id),
    call_id TEXT NOT NULL UNIQUE REFERENCES tool_calls(id),
    paths_json TEXT NOT NULL CHECK (json_valid(paths_json)),
    reversible INTEGER NOT NULL CHECK (reversible IN (0,1)),
    created_ms INTEGER NOT NULL
);
CREATE INDEX change_session ON file_changes(session_id, id);
CREATE TABLE fs_operation (
    slot INTEGER PRIMARY KEY CHECK (slot = 1),
    session_id TEXT NOT NULL REFERENCES sessions(id),
    kind TEXT NOT NULL CHECK (kind IN ('apply','undo','redo','branch')),
    manifest_json TEXT NOT NULL CHECK (json_valid(manifest_json)),
    created_ms INTEGER NOT NULL
);
CREATE TABLE compactions (
    continuation_id TEXT PRIMARY KEY NOT NULL REFERENCES sessions(id),
    predecessor_id TEXT NOT NULL UNIQUE REFERENCES sessions(id),
    source_tip_id INTEGER NOT NULL REFERENCES entries(id),
    ordinal INTEGER NOT NULL CHECK (ordinal >= 0),
    archive_path TEXT NOT NULL,
    archive_sha256 TEXT NOT NULL,
    summary_entry_id INTEGER NOT NULL REFERENCES entries(id)
);
