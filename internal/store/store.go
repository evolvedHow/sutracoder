// Package store persists Sutracoder sessions and turns, and emits the
// append-only JSONL interaction records the downstream self-improvement
// engine consumes (spec §3E).
package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // CGO-free SQLite driver
)

// Interaction is one emitted tool interaction — the schema-1 contract the
// downstream engine ingests. It is appended to the JSONL sidecar and mirrored
// into the turns table.
type Interaction struct {
	Schema           int             `json:"schema"`
	SessionID        string          `json:"session_id"`
	Turn             int             `json:"turn"`
	Timestamp        time.Time       `json:"ts"`
	Goal             string          `json:"goal"`
	Tool             string          `json:"tool"`
	Args             json.RawMessage `json:"args"`
	Status           string          `json:"status"`
	Stdout           string          `json:"stdout"`
	Stderr           string          `json:"stderr,omitempty"`
	ReturnCode       *int            `json:"returncode,omitempty"`
	DurationMS       int64           `json:"duration_ms"`
	Truncated        bool            `json:"truncated"`
	ToolError        string          `json:"tool_error,omitempty"`
	Corrected        bool            `json:"corrected"`
	TokensPrompt     int             `json:"tokens_prompt"`
	TokensCompletion int             `json:"tokens_completion"`
	SessionStatus    string          `json:"session_status"`
	// Reasoning holds the model's chain-of-thought for the turn when the
	// backend exposed it. Optional (omitempty) so schema-1 stays backward
	// compatible for the downstream engine.
	Reasoning string `json:"reasoning,omitempty"`
}

// Store wraps the SQLite database and the JSONL emitter.
type Store struct {
	db      *sql.DB
	emitter *emitter
}

// Open opens (creating if needed) the SQLite database at path and applies the
// schema. The emitter writes JSONL to interactionsDir.
func Open(dbPath, interactionsDir string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// modernc/sqlite is happy with a single writer; serialize via mutex-free
	// single connection to avoid lock contention in the CLI.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if err := applySchema(db); err != nil {
		db.Close()
		return nil, err
	}
	em, err := newEmitter(interactionsDir)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, emitter: em}, nil
}

func applySchema(db *sql.DB) error {
	const schema = `
CREATE TABLE IF NOT EXISTS sessions (
	id          TEXT PRIMARY KEY,
	goal        TEXT NOT NULL,
	status      TEXT NOT NULL DEFAULT 'running',
	summary     TEXT,
	created_at  INTEGER NOT NULL,
	updated_at  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS turns (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id      TEXT NOT NULL,
	idx             INTEGER NOT NULL,
	role            TEXT NOT NULL,
	content         TEXT,
	tool_calls      TEXT,
	tool_call_id    TEXT,
	tool_name       TEXT,
	status          TEXT,
	stdout          TEXT,
	stderr          TEXT,
	returncode      INTEGER,
	duration_ms     INTEGER,
	truncated       INTEGER,
	tool_error      TEXT,
	corrected       INTEGER,
	reasoning       TEXT,
	prompt_tokens   INTEGER,
	completion_tokens INTEGER,
	created_at      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_turns_session ON turns(session_id, idx);
`
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	if err := ensureColumn(db, "turns", "reasoning", "TEXT"); err != nil {
		return fmt.Errorf("migrate turns.reasoning: %w", err)
	}
	return nil
}

// ensureColumn adds a column to a table if it does not already exist. It backs
// the one-time additive migration for schemas created before the column was
// introduced (SQLite has no IF NOT EXISTS for ADD COLUMN).
func ensureColumn(db *sql.DB, table, column, declType string) error {
	rows, err := db.Query(fmt.Sprintf(`SELECT name FROM pragma_table_info(%q) WHERE name = ?`, table), column)
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		// Column already present; nothing to do.
		return rows.Err()
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, declType))
	return err
}

// NewSession creates a session row and returns its generated ID.
func (s *Store) NewSession(goal string) (string, error) {
	id := newID()
	now := time.Now().Unix()
	if _, err := s.db.Exec(
		`INSERT INTO sessions (id, goal, status, created_at, updated_at) VALUES (?, ?, 'running', ?, ?)`,
		id, goal, now, now); err != nil {
		return "", fmt.Errorf("insert session: %w", err)
	}
	return id, nil
}

// SetSessionStatus updates a session's status (running/completed/stopped).
func (s *Store) SetSessionStatus(id, status string) error {
	_, err := s.db.Exec(`UPDATE sessions SET status = ?, updated_at = ? WHERE id = ?`,
		status, time.Now().Unix(), id)
	return err
}

// SetSessionSummary records the completion summary on a session.
func (s *Store) SetSessionSummary(id, summary string) error {
	_, err := s.db.Exec(`UPDATE sessions SET summary = ?, updated_at = ? WHERE id = ?`,
		summary, time.Now().Unix(), id)
	return err
}

// AppendTurn persists one message/interaction row for a session. Only the
// fields relevant to the role are populated; tool interactions carry the full
// structured result. reasoning is stored on assistant turns when the backend
// exposed chain-of-thought (empty otherwise).
func (s *Store) AppendTurn(sessionID string, idx int, role, content string, toolCalls, toolCallID, toolName, reasoning string, it *Interaction) error {
	var (
		status                    sql.NullString
		stdout, stderr, toolError sql.NullString
		reasoningNS               sql.NullString
		returncode                sql.NullInt64
		durationMS                sql.NullInt64
		truncated, corrected      sql.NullInt64
		promptTok, completionTok  sql.NullInt64
	)
	reasoningNS = nullIfEmpty(reasoning)
	if it != nil {
		status = sql.NullString{String: it.Status, Valid: it.Status != ""}
		stdout = sql.NullString{String: it.Stdout, Valid: it.Stdout != ""}
		stderr = sql.NullString{String: it.Stderr, Valid: it.Stderr != ""}
		toolError = sql.NullString{String: it.ToolError, Valid: it.ToolError != ""}
		if it.ReturnCode != nil {
			returncode = sql.NullInt64{Int64: int64(*it.ReturnCode), Valid: true}
		}
		durationMS = sql.NullInt64{Int64: it.DurationMS, Valid: true}
		truncated = boolInt(it.Truncated)
		corrected = boolInt(it.Corrected)
		promptTok = sql.NullInt64{Int64: int64(it.TokensPrompt), Valid: true}
		completionTok = sql.NullInt64{Int64: int64(it.TokensCompletion), Valid: true}
	}
	_, err := s.db.Exec(`INSERT INTO turns
		(session_id, idx, role, content, tool_calls, tool_call_id, tool_name,
		 status, stdout, stderr, returncode, duration_ms, truncated, tool_error,
		 corrected, reasoning, prompt_tokens, completion_tokens, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		sessionID, idx, role, content, nullIfEmpty(toolCalls), nullIfEmpty(toolCallID),
		nullIfEmpty(toolName), status, stdout, stderr, returncode, durationMS,
		truncated, toolError, corrected, reasoningNS, promptTok, completionTok, time.Now().Unix())
	return err
}

// Emit appends an Interaction to the JSONL sidecar. Emission is fire-and-forget:
// an error is returned but never interrupts the agent loop (the caller logs it).
func (s *Store) Emit(it Interaction) error {
	it.Schema = 1
	return s.emitter.emit(it)
}

// Turn is a persisted message row, returned verbatim by ListTurns so the agent
// loop can rebuild its provider message history on resume.
type Turn struct {
	Idx        int
	Role       string
	Content    string
	ToolCalls  string
	ToolCallID string
	ToolName   string
	Status     string
	ToolError  string
}

// ListTurns returns a session's turns in order, for conversation replay.
func (s *Store) ListTurns(sessionID string) ([]Turn, error) {
	rows, err := s.db.Query(`SELECT idx, role, content, tool_calls, tool_call_id, tool_name, status, tool_error
		FROM turns WHERE session_id = ? ORDER BY id ASC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Turn
	for rows.Next() {
		var t Turn
		var content, toolCalls, toolCallID, toolName, status, toolError sql.NullString
		if err := rows.Scan(&t.Idx, &t.Role, &content, &toolCalls, &toolCallID, &toolName, &status, &toolError); err != nil {
			return nil, err
		}
		t.Content = content.String
		t.ToolCalls = toolCalls.String
		t.ToolCallID = toolCallID.String
		t.ToolName = toolName.String
		t.Status = status.String
		t.ToolError = toolError.String
		out = append(out, t)
	}
	return out, rows.Err()
}

// SessionInfo is a session row for the sessions list.
type SessionInfo struct {
	ID        string
	Goal      string
	Status    string
	Summary   string
	CreatedAt time.Time
}

// ListSessions returns the most recent sessions, newest first.
func (s *Store) ListSessions(limit int) ([]SessionInfo, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(`SELECT id, goal, status, IFNULL(summary,''), created_at
		FROM sessions ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionInfo
	for rows.Next() {
		var si SessionInfo
		var created int64
		if err := rows.Scan(&si.ID, &si.Goal, &si.Status, &si.Summary, &created); err != nil {
			return nil, err
		}
		si.CreatedAt = time.Unix(created, 0)
		out = append(out, si)
	}
	return out, rows.Err()
}

// GetSession returns one session's goal and summary (for resume).
func (s *Store) GetSession(id string) (goal, status, summary string, err error) {
	err = s.db.QueryRow(`SELECT goal, status, IFNULL(summary,'') FROM sessions WHERE id = ?`, id).
		Scan(&goal, &status, &summary)
	return
}

// Close releases the database and the emitter.
func (s *Store) Close() error {
	s.emitter.close()
	return s.db.Close()
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func boolInt(b bool) sql.NullInt64 {
	if b {
		return sql.NullInt64{Int64: 1, Valid: true}
	}
	return sql.NullInt64{Int64: 0, Valid: true}
}

func nullIfEmpty(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
