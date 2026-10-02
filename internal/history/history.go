// Package history stores immutable conversation trees and file-tool snapshots.
package history

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"scicode/internal/filelock"
	"scicode/internal/provider"
	"scicode/internal/render"

	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

const schemaVersion = 4

// Store serializes commits; callers must close it after stopping runtime workers.
type Store struct {
	DB   *sql.DB
	Root string
	mu   sync.Mutex
}

// Session identifies the selected history/file tips. Zero tips mean an empty tree.
type Session struct {
	CompactionError                       string // Nonempty permanently disables inference in this context; history remains inspectable.
	ID, WorkspaceID, LineageID, Name      string
	ReadOnly                              bool
	EntryTip, FileTip, RedoTip, UndoFloor int64
	Model                                 provider.Selection
	LastActivityMS                        int64 // Unix milliseconds; date grouping uses the frontend local timezone.
}

// Entry is one immutable node in chronological history.
type Entry struct {
	MainTurnID, UndoOwnerTurnID          string // Chronological inference attribution and most recent human undo checkpoint.
	ID, Parent, Source, FileTip          int64
	SessionID, TurnID, Actor, Kind, Role string
	Visible                              bool
	Content                              json.RawMessage
	CreatedMS                            int64
}

// NewID returns a readable prefix and 16 URL-safe characters encoding 96 random bits.
func NewID(prefix string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return prefix + "_" + base64.RawURLEncoding.EncodeToString(b[:])
}

// DataRoot resolves an absolute XDG root, falling back to the user's home.
func DataRoot() (string, error) {
	if p := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(p) {
		return filepath.Join(p, "ttc"), nil
	}
	h, e := os.UserHomeDir()
	return filepath.Join(h, ".local/share/ttc"), e
}

// Open opens shared history without recovering work or changing existing records.
// Empty databases initialize atomically; incompatible schemas are rejected.
func Open(root string) (*Store, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err = PrivateDir(root); err != nil {
		return nil, err
	}
	// SQLite's initial transition into WAL can return BUSY without invoking its
	// busy handler. Serialize only connection/schema setup, never session work.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lock, err := filelock.Acquire(ctx, filepath.Join(root, "history-init.lock"))
	if err != nil {
		return nil, fmt.Errorf("initialize shared history: %w", err)
	}
	defer lock.Close()
	db, err := openDatabase(root)
	if err != nil {
		return nil, err
	}
	s := &Store{DB: db, Root: root}
	err = s.transact(func(tx *sql.Tx) error {
		var version int
		if err := tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
			return err
		}
		if version == schemaVersion {
			return nil
		}
		var nonempty bool
		if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%')").Scan(&nonempty); err != nil {
			return err
		}
		if version != 0 || nonempty {
			return fmt.Errorf("incompatible history schema %d (require %d); select a new --data-dir", version, schemaVersion)
		}
		_, err := tx.Exec(schema)
		return err
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// openDatabase uses immediate transactions to serialize short commits across processes.
func openDatabase(root string) (*sql.DB, error) {
	path := filepath.Join(root, "history.sqlite")
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		info, err := os.Lstat(path + suffix)
		if err == nil && !info.Mode().IsRegular() {
			return nil, fmt.Errorf("history.sqlite%s must be a regular file, not a symlink", suffix)
		}
		if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("stat history.sqlite%s: %w", suffix, err)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	err = f.Chmod(0600)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	uri := url.URL{Scheme: "file", Path: path, RawQuery: "_txlock=immediate"}
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, q := range []string{"PRAGMA busy_timeout=5000", "PRAGMA foreign_keys=ON", "PRAGMA journal_mode=WAL", "PRAGMA synchronous=FULL"} {
		if _, err = db.Exec(q); err != nil {
			db.Close()
			return nil, err
		}
	}
	return db, nil
}

// PrivateDir creates a private directory and rejects a final symlink or unsafe mode.
func PrivateDir(p string) error {
	if err := os.MkdirAll(p, 0700); err != nil {
		return err
	}
	st, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm() != 0700 {
		return fmt.Errorf("unsafe private directory %s", p)
	}
	return nil
}

// Close closes history after the runtime workers have stopped.
func (s *Store) Close() error { return s.DB.Close() }
func n(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}
func textOrNil(v string) any {
	if v == "" {
		return nil
	}
	return v
}
func (s *Store) transact(fn func(*sql.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, e := s.DB.Begin()
	if e != nil {
		return e
	}
	if e = fn(tx); e != nil {
		tx.Rollback()
		return e
	}
	return tx.Commit()
}

// StartSession atomically saves a new writable lineage, its first user turn and
// user message. The caller owns id before saving; errors leave no partial rows.
// It returns the turn and message entry IDs, without restoring workspace files.
func (s *Store) StartSession(id, path string, model provider.Selection, message provider.Message) (turn string, entry int64, err error) {
	if id == "" || !filepath.IsAbs(path) || message.Role != "user" {
		return "", 0, errors.New("first session save requires an ID, absolute workspace path and user message")
	}
	wid := NewID("workspace")
	m, e := json.Marshal(model)
	if e != nil {
		return "", 0, e
	}
	content, e := json.Marshal(message)
	if e != nil {
		return "", 0, e
	}
	turn = NewID("turn")
	e = s.transact(func(tx *sql.Tx) error {
		if _, e := tx.Exec("INSERT INTO workspaces(id,path) VALUES(?,?) ON CONFLICT(path) DO NOTHING", wid, path); e != nil {
			return e
		}
		if e := tx.QueryRow("SELECT id FROM workspaces WHERE path=?", path).Scan(&wid); e != nil {
			return e
		}
		_, e := tx.Exec(`INSERT INTO sessions(id,workspace_id,lineage_id,name,name_source,model_json,last_activity_ms,metadata_json) VALUES(?,?,?,'New session','default',?,?,'{}')`, id, wid, id, string(m), time.Now().UnixMilli())
		if e != nil {
			return e
		}
		if _, e := tx.Exec(`INSERT INTO turns(id,session_id,trigger,status,model_json,started_ms) VALUES(?,?,'user','running',?,?)`, turn, id, string(m), time.Now().UnixMilli()); e != nil {
			return e
		}
		if _, e := tx.Exec("UPDATE sessions SET metadata_json=json_set(metadata_json,'$.main_turn_id',?,'$.undo_owner_turn_id',?) WHERE id=?", turn, turn, id); e != nil {
			return e
		}
		entry, e = appendTx(tx, id, turn, "main", "message", "user", true, content, 0)
		return e
	})
	if e != nil {
		return "", 0, e
	}
	return turn, entry, nil
}

// RenameSession sets a human title of 1–60 characters. A manual name cannot be
// overwritten by a pending automatic naming request. Missing sessions fail.
func (s *Store) RenameSession(id, name string) error {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 60 || strings.IndexFunc(name, unicode.IsControl) >= 0 || !utf8.ValidString(name) {
		return errors.New("session name must be 1–60 characters without control characters")
	}
	return s.transact(func(tx *sql.Tx) error {
		result, err := tx.Exec("UPDATE sessions SET name=?,name_source='manual' WHERE id=?", name, id)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err == nil && count == 0 {
			return sql.ErrNoRows
		}
		return err
	})
}

// Session loads metadata without reviving transient handles.
func (s *Store) Session(id string) (Session, error) {
	var v Session
	var model string
	e := s.DB.QueryRow(`SELECT id,workspace_id,lineage_id,name,read_only,coalesce(active_entry_id,0),coalesce(file_tip_id,0),coalesce(redo_entry_id,0),coalesce(undo_floor_id,0),model_json,last_activity_ms,coalesce(json_extract(metadata_json,'$.compaction_error'),'') FROM sessions WHERE id=?`, id).Scan(&v.ID, &v.WorkspaceID, &v.LineageID, &v.Name, &v.ReadOnly, &v.EntryTip, &v.FileTip, &v.RedoTip, &v.UndoFloor, &model, &v.LastActivityMS, &v.CompactionError)
	if e != nil {
		return v, e
	}
	e = json.Unmarshal([]byte(model), &v.Model)
	return v, e
}

// Sessions returns up to 100 sessions belonging to the absolute workspace path, newest activity
// first, without scanning transcripts. Filtering before the limit keeps other
// workspaces from hiding sessions.
func (s *Store) Sessions(path string) ([]Session, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("session listing requires an absolute workspace path")
	}
	rows, err := s.DB.Query("SELECT s.id,s.workspace_id,s.lineage_id,s.name,s.read_only,coalesce(s.active_entry_id,0),coalesce(s.file_tip_id,0),coalesce(s.redo_entry_id,0),coalesce(s.undo_floor_id,0),s.model_json,s.last_activity_ms,coalesce(json_extract(s.metadata_json,'$.compaction_error'),'') FROM sessions s JOIN workspaces w ON w.id=s.workspace_id WHERE w.path=? ORDER BY s.last_activity_ms DESC,s.id LIMIT 100", path)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Session{}
	for rows.Next() {
		var v Session
		var model string
		if err := rows.Scan(&v.ID, &v.WorkspaceID, &v.LineageID, &v.Name, &v.ReadOnly, &v.EntryTip, &v.FileTip, &v.RedoTip, &v.UndoFloor, &model, &v.LastActivityMS, &v.CompactionError); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(model), &v.Model); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// BeginTurn records the start checkpoint and frozen selection before model work.
func (s *Store) BeginTurn(session, trigger string, model provider.Selection) (string, error) {
	id, _, err := s.AdmitTurn(session, trigger, model, nil)
	return id, err
}

// AdmitTurn commits a main checkpoint and optional human instruction atomically.
// Call while owning the workspace admission gate so file mutations cannot cross it.
func (s *Store) AdmitTurn(session, trigger string, model provider.Selection, message *provider.Message) (string, int64, error) {
	if message != nil && trigger != "user" {
		return "", 0, errors.New("only user turns accept a human instruction")
	}
	id := NewID("turn")
	var entry int64
	m, _ := json.Marshal(model)
	e := s.transact(func(tx *sql.Tx) error {
		var ro bool
		if e := tx.QueryRow("SELECT read_only FROM sessions WHERE id=?", session).Scan(&ro); e != nil {
			return e
		}
		if ro {
			return errors.New("session is read-only")
		}
		_, e := tx.Exec(`INSERT INTO turns(id,session_id,trigger,start_entry_id,start_file_tip_id,status,model_json,started_ms) SELECT ?,id,?,active_entry_id,file_tip_id,'running',?,? FROM sessions WHERE id=?`, id, trigger, string(m), time.Now().UnixMilli(), session)
		if e != nil {
			return e
		}
		if _, e = tx.Exec("UPDATE sessions SET metadata_json=json_set(metadata_json,'$.main_turn_id',?) WHERE id=?", id, session); e != nil {
			return e
		}
		if trigger == "user" {
			if _, e = tx.Exec("UPDATE sessions SET metadata_json=json_set(metadata_json,'$.undo_owner_turn_id',?) WHERE id=?", id, session); e != nil {
				return e
			}
		}
		if message != nil {
			b, err := json.Marshal(message)
			if err != nil {
				return err
			}
			entry, e = appendTx(tx, session, id, "main", "message", "user", true, b, 0)
		}
		return e
	})
	return id, entry, e
}

// FinishTurn records a terminal status; it never resends a failed request.
func (s *Store) FinishTurn(id, status string) error {
	return s.transact(func(tx *sql.Tx) error {
		var session, actor string
		if err := tx.QueryRow("SELECT session_id,actor_id FROM turns WHERE id=?", id).Scan(&session, &actor); err != nil {
			return err
		}
		result, err := tx.Exec("UPDATE turns SET status=?,finished_ms=? WHERE id=? AND status='running'", status, time.Now().UnixMilli(), id)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil || count == 0 {
			return err
		}
		b, _ := json.Marshal(map[string]any{"type": "turn_finished", "status": status})
		if _, err = appendTx(tx, session, id, actor, "status", "", false, b, 0); err != nil {
			return err
		}
		if actor == "main" {
			_, err = tx.Exec("UPDATE sessions SET metadata_json=json_remove(metadata_json,'$.main_turn_id') WHERE id=? AND json_extract(metadata_json,'$.main_turn_id')=?", session, id)
		}
		return err
	})
}
func appendTx(tx *sql.Tx, session, turn, actor, kind, role string, visible bool, data json.RawMessage, source int64) (int64, error) {
	if !json.Valid(data) {
		return 0, errors.New("invalid history JSON")
	}
	var parent, tip sql.NullInt64
	var ro bool
	if e := tx.QueryRow("SELECT active_entry_id,file_tip_id,read_only FROM sessions WHERE id=?", session).Scan(&parent, &tip, &ro); e != nil {
		return 0, e
	}
	if ro {
		return 0, errors.New("session is read-only")
	}
	var mainTurn, undoOwner sql.NullString
	if e := tx.QueryRow("SELECT json_extract(metadata_json,'$.main_turn_id'),json_extract(metadata_json,'$.undo_owner_turn_id') FROM sessions WHERE id=?", session).Scan(&mainTurn, &undoOwner); e != nil {
		return 0, e
	}
	if source != 0 {
		if e := tx.QueryRow("SELECT main_turn_id,undo_owner_turn_id FROM entries WHERE id=?", source).Scan(&mainTurn, &undoOwner); e != nil {
			return 0, e
		}
	}
	r, e := tx.Exec(`INSERT INTO entries(session_id,parent_id,source_id,turn_id,main_turn_id,undo_owner_turn_id,actor_id,kind,role,model_visible,content_json,file_tip_id,created_ms) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, session, parent, n(source), textOrNil(turn), mainTurn, undoOwner, actor, kind, textOrNil(role), visible, string(data), tip, time.Now().UnixMilli())
	if e != nil {
		return 0, e
	}
	id, e := r.LastInsertId()
	if e != nil {
		return 0, e
	}
	_, e = tx.Exec("UPDATE sessions SET active_entry_id=?,redo_entry_id=NULL,last_activity_ms=? WHERE id=?", id, time.Now().UnixMilli(), session)
	return id, e
}

// Append adds one node to the selected branch in a short transaction.
func (s *Store) Append(session, turn, actor, kind, role string, visible bool, data any) (int64, error) {
	b, e := json.Marshal(data)
	if e != nil {
		return 0, e
	}
	var id int64
	e = s.transact(func(tx *sql.Tx) error {
		var e error
		id, e = appendTx(tx, session, turn, actor, kind, role, visible, b, 0)
		return e
	})
	return id, e
}

// Branch returns selected ancestry through tip (zero means the session's tip).
func (s *Store) Branch(session string, tip int64) ([]Entry, error) {
	return branchWith(s.DB, session, tip)
}
func branchWith(q historyReader, session string, tip int64) ([]Entry, error) {
	if tip == 0 {
		if e := q.QueryRow("SELECT coalesce(active_entry_id,0) FROM sessions WHERE id=?", session).Scan(&tip); e != nil {
			return nil, e
		}
	}
	rows, e := q.Query(`WITH RECURSIVE ancestry AS (SELECT * FROM entries WHERE id=? AND session_id=? UNION ALL SELECT e.* FROM entries e JOIN ancestry a ON e.id=a.parent_id) SELECT id,coalesce(parent_id,0),coalesce(source_id,0),coalesce(file_tip_id,0),session_id,coalesce(turn_id,''),actor_id,kind,coalesce(role,''),model_visible,content_json,created_ms,coalesce(main_turn_id,''),coalesce(undo_owner_turn_id,'') FROM ancestry ORDER BY id`, tip, session)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var v Entry
		var content string
		if e = rows.Scan(&v.ID, &v.Parent, &v.Source, &v.FileTip, &v.SessionID, &v.TurnID, &v.Actor, &v.Kind, &v.Role, &v.Visible, &content, &v.CreatedMS, &v.MainTurnID, &v.UndoOwnerTurnID); e != nil {
			return nil, e
		}
		v.Content = json.RawMessage(content)
		out = append(out, v)
	}
	return out, rows.Err()
}

// Messages projects only model-visible canonical messages, resolving tool references.
func (s *Store) Messages(session string) ([]provider.Message, error) {
	return messagesWith(s.DB, session)
}
func messagesWith(q historyReader, session string) ([]provider.Message, error) {
	entries, e := branchWith(q, session, 0)
	if e != nil {
		return nil, e
	}
	out := make([]provider.Message, 0, len(entries))
	for _, v := range entries {
		if !v.Visible {
			continue
		}
		if v.Kind == "tool_result" {
			var ref struct {
				CallID string `json:"call_id"`
			}
			if e = json.Unmarshal(v.Content, &ref); e != nil {
				return nil, e
			}
			var result, pcid string
			if e = q.QueryRow("SELECT result_json,provider_call_id FROM tool_calls WHERE id=?", ref.CallID).Scan(&result, &pcid); e != nil {
				return nil, e
			}
			out = append(out, provider.Message{Role: "tool", CallID: pcid, Content: result})
		} else {
			var m provider.Message
			if e = json.Unmarshal(v.Content, &m); e != nil {
				return nil, e
			}
			out = append(out, m)
		}
	}
	return orderedToolResults(out), nil
}

// orderedToolResults keeps canonical call order while entry rows retain actual
// completion chronology. Missing results stay missing until callers settle them.
func orderedToolResults(messages []provider.Message) []provider.Message {
	for i, m := range messages {
		if m.Role != "assistant" || len(m.Calls) < 2 {
			continue
		}
		end := i + 1
		for end < len(messages) && messages[end].Role == "tool" {
			end++
		}
		if end-i-1 < 2 {
			continue
		}
		results := map[string]provider.Message{}
		for _, r := range messages[i+1 : end] {
			results[r.CallID] = r
		}
		position := i + 1
		for _, call := range m.Calls {
			if r, ok := results[call.ID]; ok {
				messages[position] = r
				position++
			}
		}
	}
	return messages
}

// StartRequest persists uncertain request state before network I/O.
func (s *Store) StartRequest(session, turn, actor, purpose string, model provider.Selection) (id int64, err error) {
	m, err := json.Marshal(model)
	if err != nil {
		return 0, err
	}
	err = s.transact(func(tx *sql.Tx) error {
		var cutoff int64
		if err := tx.QueryRow("SELECT coalesce(max(id),0) FROM entries").Scan(&cutoff); err != nil {
			return err
		}
		result, err := tx.Exec(`INSERT INTO model_requests(session_id,turn_id,actor_id,purpose,model_json,status,attempts_json,event_cutoff,created_ms) VALUES(?,?,?,?,?,'running','[]',?,?)`, session, textOrNil(turn), actor, purpose, string(m), cutoff, time.Now().UnixMilli())
		if err != nil {
			return err
		}
		id, err = result.LastInsertId()
		if err != nil {
			return err
		}
		b, _ := json.Marshal(map[string]any{"type": "request_admitted", "request_id": id, "event_cutoff": cutoff, "purpose": purpose})
		_, err = appendTx(tx, session, turn, actor, "status", "", false, b, 0)
		return err
	})
	return id, err
}

// FinishRequest commits terminal request metadata and its chronological event.
func (s *Store) FinishRequest(id int64, status string, attempts any) error {
	b, err := json.Marshal(attempts)
	if err != nil {
		return err
	}
	return s.transact(func(tx *sql.Tx) error {
		var session, turn, actor string
		if err := tx.QueryRow("SELECT session_id,coalesce(turn_id,''),actor_id FROM model_requests WHERE id=?", id).Scan(&session, &turn, &actor); err != nil {
			return err
		}
		if err := tx.QueryRow(`WITH RECURSIVE successors AS (
            SELECT id,read_only FROM sessions WHERE id=?
            UNION ALL SELECT s.id,s.read_only FROM sessions s JOIN successors p ON s.predecessor_id=p.id
        ) SELECT id FROM successors WHERE read_only=0`, session).Scan(&session); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE model_requests SET status=?,attempts_json=? WHERE id=?", status, string(b), id); err != nil {
			return err
		}
		event, _ := json.Marshal(map[string]any{"type": "request_finished", "request_id": id, "status": status})
		_, err := appendTx(tx, session, turn, actor, "status", "", false, event, 0)
		return err
	})
}

// CallIntent commits validated or invalid input before execution begins.
func (s *Store) CallIntent(session, turn, actor string, request int64, c provider.ToolCall) (string, error) {
	id := NewID("call")
	if !json.Valid(c.Arguments) {
		return "", errors.New("tool arguments are not JSON")
	}
	e := s.transact(func(tx *sql.Tx) error {
		_, e := tx.Exec(`INSERT INTO tool_calls(id,session_id,request_id,provider_call_id,actor_id,name,call_version,call_json) VALUES(?,?,?,?,?,?,1,?)`, id, session, request, c.ID, actor, c.Name, string(c.Arguments))
		if e != nil {
			return e
		}
		b, _ := json.Marshal(map[string]string{"call_id": id})
		_, e = appendTx(tx, session, turn, actor, "tool_call", "", false, b, 0)
		return e
	})
	return id, e
}

// CallResult commits an immutable result and its portable presentation.
func (s *Store) CallResult(session, turn, actor, call string, result json.RawMessage, record any, md render.Markdown, visible bool) (int64, error) {
	b, e := json.Marshal(record)
	if e != nil {
		return 0, e
	}
	m, e := json.Marshal(md)
	if e != nil {
		return 0, e
	}
	var entry int64
	err := s.transact(func(tx *sql.Tx) error {
		_, e := tx.Exec("UPDATE tool_calls SET result_json=? WHERE id=?", string(result), call)
		if e != nil {
			return e
		}
		ref, _ := json.Marshal(map[string]string{"call_id": call})
		id, e := appendTx(tx, session, turn, actor, "tool_result", "tool", visible, ref, 0)
		if e != nil {
			return e
		}
		entry = id
		_, e = tx.Exec("INSERT INTO tool_records(entry_id,call_id,version,record_json,markdown_json) VALUES(?,?,1,?,?)", id, call, string(b), string(m))
		return e
	})
	return entry, err
}

// Artifact durably saves immutable private bytes under the session's lineage.
func (s *Store) Artifact(session, category string, data []byte) (string, error) {
	v, e := s.Session(session)
	if e != nil {
		return "", e
	}
	if strings.ContainsAny(category, "/\\.") {
		return "", errors.New("invalid artifact category")
	}
	dir := filepath.Join(s.Root, "lineages", v.LineageID, category)
	if e = PrivateDir(dir); e != nil {
		return "", e
	}
	h := sha256.Sum256(data)
	p := filepath.Join(dir, hex.EncodeToString(h[:]))
	if old, e := readArtifact(p, int64(len(data))); e == nil {
		if sha256.Sum256(old) != h {
			return "", errors.New("artifact hash conflict")
		}
		return p, nil
	} else if !os.IsNotExist(e) {
		return "", fmt.Errorf("read saved artifact: %w", e)
	}
	if e = AtomicFile(p, data, 0600); e != nil {
		return "", e
	}
	return p, nil
}

// AtomicFile writes/fyncs bytes then atomically renames within the same directory.
func AtomicFile(path string, data []byte, mode os.FileMode) error {
	f, e := os.CreateTemp(filepath.Dir(path), ".ttc-*")
	if e != nil {
		return e
	}
	temp := f.Name()
	defer os.Remove(temp)
	if e = f.Chmod(mode); e == nil {
		_, e = f.Write(data)
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	if e = os.Rename(temp, path); e != nil {
		return e
	}
	return SyncDir(filepath.Dir(path))
}

// SyncDir ensures directory entry updates reach durable storage.
func SyncDir(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}

// Ping validates storage availability with cancellation.
func (s *Store) Ping(ctx context.Context) error { return s.DB.PingContext(ctx) }

// RecordSystemPrompt stores the exact instruction snapshot as an inspectable UI
// entry without clearing redo: an instruction refresh is not a user action.
// Authentication never belongs in this prompt or its artifact.
func (s *Store) RecordSystemPrompt(session, turn, actor string, request int64, prompt string) (int64, error) {
	if len(prompt) > instructionSnapshotBytes {
		return 0, errors.New("instruction snapshot exceeds 1 MiB")
	}
	path, e := s.Artifact(session, "prompts", []byte(prompt))
	if e != nil {
		return 0, e
	}
	content, e := json.Marshal(map[string]any{"type": "system_prompt", "request_id": request, "path": path, "label": "System prompt · inspect"})
	if e != nil {
		return 0, e
	}
	var id int64
	e = s.transact(func(tx *sql.Tx) error {
		var redo sql.NullInt64
		if e := tx.QueryRow("SELECT redo_entry_id FROM sessions WHERE id=?", session).Scan(&redo); e != nil {
			return e
		}
		var e error
		id, e = appendTx(tx, session, turn, actor, "status", "", false, content, 0)
		if e != nil {
			return e
		}
		_, e = tx.Exec("UPDATE sessions SET redo_entry_id=? WHERE id=?", redo, session)
		return e
	})
	return id, e
}

// Inspect renders one history node, including system prompt placeholders, in the shared window.
func (s *Store) Inspect(entry Entry) (string, error) {
	if entry.Kind == "tool_call" || entry.Kind == "tool_result" {
		if entry.Kind == "tool_result" {
			var mdJSON string
			if e := s.DB.QueryRow("SELECT markdown_json FROM tool_records WHERE entry_id=?", entry.ID).Scan(&mdJSON); e == nil {
				var md render.Markdown
				if e = json.Unmarshal([]byte(mdJSON), &md); e != nil {
					return "", e
				}
				return md.Detail, nil
			} else if e != sql.ErrNoRows {
				return "", e
			}
		}

		var ref struct {
			CallID string `json:"call_id"`
		}
		if e := json.Unmarshal(entry.Content, &ref); e != nil {
			return "", e
		}
		var args, name string
		var result sql.NullString
		if e := s.DB.QueryRow("SELECT name,call_json,result_json FROM tool_calls WHERE id=?", ref.CallID).Scan(&name, &args, &result); e != nil {
			return "", e
		}
		return render.Tool(name, json.RawMessage(args), json.RawMessage(result.String)).Detail, nil
	}
	if entry.Kind == "status" {
		var job struct {
			Type     string
			Markdown render.Markdown
		}
		if json.Unmarshal(entry.Content, &job) == nil && job.Type == "job_completion" {
			if job.Markdown.Revision != 1 || job.Markdown.Summary == "" {
				return "", errors.New("invalid job completion presentation")
			}
			return job.Markdown.Detail, nil
		}
		var internal struct {
			Type, Purpose string
			Message       provider.Message
		}
		if json.Unmarshal(entry.Content, &internal) == nil && internal.Type == "request_message" {
			if internal.Purpose == "compaction" && internal.Message.Role == "assistant" {
				return render.Clean(internal.Message.Content), nil
			}
			b, e := json.MarshalIndent(internal.Message, "", "  ")
			return render.Fence(string(b), "json"), e
		}

		var status struct{ Type, Path string }
		if e := json.Unmarshal(entry.Content, &status); e != nil {
			return "", e
		}
		if status.Type == "system_prompt" {
			b, e := readArtifact(status.Path, instructionSnapshotBytes)
			return string(b), e
		}
	}
	var m provider.Message
	if entry.Visible {
		if e := json.Unmarshal(entry.Content, &m); e != nil {
			return "", e
		}
		m.State = nil
		var content string
		if json.Valid([]byte(m.Content)) {
			content = render.Fence(m.Content, "json")
		} else {
			content = render.Fence(m.Content, "text")
		}
		// Show content once, rather than again as a giant escaped metadata string.
		m.Content = ""
		b, e := json.Marshal(m)
		return content + "\n\nMessage metadata:\n" + render.Fence(string(b), "json"), e
	}
	return render.Fence(string(entry.Content), "json"), nil
}

// Assistant commits a response and all call intents in the same transaction.
// A crash can never leave emitted assistant calls without recoverable intent rows.
func (s *Store) Assistant(session, turn, actor string, request int64, message provider.Message) (int64, []string, error) {
	message.RequestID = request
	data, e := json.Marshal(message)
	if e != nil {
		return 0, nil, e
	}
	var entry int64
	ids := make([]string, 0, len(message.Calls))
	e = s.transact(func(tx *sql.Tx) error {
		var e error
		entry, e = appendTx(tx, session, turn, actor, "message", "assistant", actor == "main", data, 0)
		if e != nil {
			return e
		}
		for _, c := range message.Calls {
			if !json.Valid(c.Arguments) {
				return errors.New("invalid provider call arguments")
			}
			id := NewID("call")
			if _, e = tx.Exec(`INSERT INTO tool_calls(id,session_id,request_id,provider_call_id,actor_id,name,call_version,call_json) VALUES(?,?,?,?,?,?,1,?)`, id, session, request, c.ID, actor, c.Name, string(c.Arguments)); e != nil {
				return e
			}
			ref, _ := json.Marshal(map[string]string{"call_id": id})
			if _, e = appendTx(tx, session, turn, actor, "tool_call", "", false, ref, 0); e != nil {
				return e
			}
			ids = append(ids, id)
		}
		return nil
	})
	return entry, ids, e
}
