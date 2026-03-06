// context-vault — HTTP hooks server for Claude Code persistent memory.
//
// Listens on localhost:9742. Claude Code sends hook events via HTTP POST.
// Writes all session data to SQLite JSONB at $PROJECT_DIR/.claude/vault.db.
// RingDumper measures accumulated context and signals Claude via
// UserPromptSubmit stdout when threshold is exceeded.
//
// Build:
//
//	go build -o bin/context-vault-$(go env GOOS)-$(go env GOARCH) ./cmd/context-vault
//
// Requires: modernc.org/sqlite (SQLite >= 3.45.0, JSONB support, no CGO)
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

const (
	addr             = "localhost:9742"
	ringSize         = 10
	ringThreshold    = 2000 // estimated tokens before signaling
	dbRelPath        = ".claude/vault.db"
	gitignoreEntry   = ".claude/vault.db"
)

// ── Schema ────────────────────────────────────────────────────────────────────

const schema = `
PRAGMA journal_mode=WAL;
PRAGMA foreign_keys=ON;

CREATE TABLE IF NOT EXISTS sessions (
    id          TEXT    PRIMARY KEY,
    started_at  INTEGER NOT NULL,
    ended_at    INTEGER,
    project     TEXT,
    model       TEXT
);

CREATE TABLE IF NOT EXISTS entities (
    id             INTEGER PRIMARY KEY,
    namespace      TEXT    NOT NULL,
    type           TEXT    NOT NULL,
    label          TEXT    NOT NULL,
    sensitivity    INTEGER DEFAULT 0,
    ts_created     INTEGER NOT NULL,
    ts_updated     INTEGER NOT NULL,
    session_origin TEXT,
    meta           BLOB
);

CREATE TABLE IF NOT EXISTS relations (
    id         INTEGER PRIMARY KEY,
    from_id    INTEGER NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    to_id      INTEGER NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    type       TEXT    NOT NULL,
    ts_created INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS buffer (
    id         INTEGER PRIMARY KEY,
    session_id TEXT    NOT NULL,
    ts         INTEGER NOT NULL,
    hook       TEXT    NOT NULL,
    payload    BLOB    NOT NULL,
    size_est   INTEGER NOT NULL,
    processed  INTEGER DEFAULT 0
);

CREATE TABLE IF NOT EXISTS compact_log (
    id          INTEGER PRIMARY KEY,
    ts          INTEGER NOT NULL,
    session_id  TEXT    NOT NULL,
    trigger     TEXT,
    reasoning   TEXT,
    query_used  TEXT,
    result_text TEXT
);

CREATE INDEX IF NOT EXISTS idx_entities_ns_type ON entities(namespace, type);
CREATE INDEX IF NOT EXISTS idx_entities_ts      ON entities(ts_updated DESC);
CREATE INDEX IF NOT EXISTS idx_relations_from   ON relations(from_id);
CREATE INDEX IF NOT EXISTS idx_relations_to     ON relations(to_id);
CREATE INDEX IF NOT EXISTS idx_buffer_session   ON buffer(session_id, processed, ts);
`

// ── RingDumper ────────────────────────────────────────────────────────────────

type entry struct {
	hook string
	size int
	ts   int64
}

type RingDumper struct {
	mu        sync.Mutex
	buf       [ringSize]entry
	head      int
	total     int
}

func (r *RingDumper) Push(hook string, payloadSize int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	evicted := r.buf[r.head]
	r.total -= evicted.size
	r.buf[r.head] = entry{hook: hook, size: payloadSize, ts: time.Now().Unix()}
	r.total += payloadSize
	r.head = (r.head + 1) % ringSize
}

func (r *RingDumper) ShouldSignal() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.total/4 > ringThreshold
}

func (r *RingDumper) Flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = [ringSize]entry{}
	r.head = 0
	r.total = 0
}

func (r *RingDumper) Total() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.total
}

// ── Server ────────────────────────────────────────────────────────────────────

type Server struct {
	db        *sql.DB
	ring      *RingDumper
	projectDir string
	sessionID  string
	mu         sync.RWMutex
}

func NewServer(dbPath, projectDir string) (*Server, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0700); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}

	if err := os.Chmod(dbPath, 0600); err != nil {
		log.Printf("warning: chmod %s: %v", dbPath, err)
	}

	return &Server{
		db:         db,
		ring:       &RingDumper{},
		projectDir: projectDir,
	}, nil
}

// readBody reads and returns the request body as raw bytes.
func readBody(r *http.Request) []byte {
	b, _ := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	return b
}

// jsonPayload converts raw bytes to a JSONB-compatible string for SQLite.
func jsonPayload(b []byte) string {
	if json.Valid(b) {
		return string(b)
	}
	v, _ := json.Marshal(map[string]string{"raw": string(b)})
	return string(v)
}

func now() int64 { return time.Now().Unix() }

// ── Hook handlers ─────────────────────────────────────────────────────────────

func (s *Server) handleSessionStart(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)

	var data struct {
		SessionID  string `json:"session_id"`
		ProjectDir string `json:"project_dir"`
		Model      string `json:"model"`
	}
	_ = json.Unmarshal(body, &data)

	if data.ProjectDir == "" {
		data.ProjectDir = s.projectDir
	}

	s.mu.Lock()
	if data.SessionID != "" {
		s.sessionID = data.SessionID
	}
	s.mu.Unlock()

	if data.SessionID != "" {
		_, err := s.db.Exec(
			`INSERT OR IGNORE INTO sessions (id, started_at, project, model) VALUES (?, ?, ?, ?)`,
			data.SessionID, now(), data.ProjectDir, data.Model,
		)
		if err != nil {
			log.Printf("session insert: %v", err)
		}
	}

	// Ensure .gitignore
	if data.ProjectDir != "" {
		ensureGitignore(data.ProjectDir)
	}

	// Inject context summary into stdout (injected by Claude Code)
	fmt.Print(s.buildStartContext(data.SessionID))

	w.WriteHeader(http.StatusOK)
}

func (s *Server) buildStartContext(sessionID string) string {
	var sb strings.Builder
	sb.WriteString("[vault] Session active — vault.db initialisé\n")

	if sessionID != "" {
		sb.WriteString(fmt.Sprintf("Session : %s\n", sessionID))
	}

	// Types summary
	rows, err := s.db.Query(`
		SELECT type, COUNT(*) as n FROM entities
		WHERE namespace != '' GROUP BY type ORDER BY n DESC LIMIT 10
	`)
	if err == nil {
		defer rows.Close()
		var parts []string
		for rows.Next() {
			var t string
			var n int
			if rows.Scan(&t, &n) == nil {
				parts = append(parts, fmt.Sprintf("%s(%d)", t, n))
			}
		}
		if len(parts) > 0 {
			sb.WriteString("Types en DB : " + strings.Join(parts, " ") + "\n")
		}
	}

	// Open todos
	rows2, err := s.db.Query(`
		SELECT label, meta->>'$.priority', meta->>'$.blob_plus'
		FROM entities
		WHERE type = 'todo' AND meta->>'$.status' != 'done' AND sensitivity < 2
		ORDER BY CASE meta->>'$.priority' WHEN 'high' THEN 0 WHEN 'normal' THEN 1 ELSE 2 END
		LIMIT 5
	`)
	if err == nil {
		defer rows2.Close()
		var todos []string
		for rows2.Next() {
			var label, priority, quoi string
			if rows2.Scan(&label, &priority, &quoi) == nil {
				todos = append(todos, fmt.Sprintf("- %s [%s]", label, priority))
			}
		}
		if len(todos) > 0 {
			sb.WriteString("Todos ouverts :\n" + strings.Join(todos, "\n") + "\n")
		}
	}

	// Active constraints
	rows3, err := s.db.Query(`
		SELECT label FROM entities
		WHERE type = 'constraint' AND sensitivity < 2
		ORDER BY ts_updated DESC LIMIT 5
	`)
	if err == nil {
		defer rows3.Close()
		var cs []string
		for rows3.Next() {
			var label string
			if rows3.Scan(&label) == nil {
				cs = append(cs, "- "+label)
			}
		}
		if len(cs) > 0 {
			sb.WriteString("Contraintes actives :\n" + strings.Join(cs, "\n") + "\n")
		}
	}

	return sb.String()
}

func (s *Server) handleUserPrompt(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	sid := s.currentSession()

	// Buffer the prompt
	_, err := s.db.Exec(
		`INSERT INTO buffer (session_id, ts, hook, payload, size_est) VALUES (?, ?, 'user_prompt', jsonb(?), ?)`,
		sid, now(), jsonPayload(body), len(body),
	)
	if err != nil {
		log.Printf("buffer insert (user_prompt): %v", err)
	}

	s.ring.Push("user_prompt", len(body))

	// Signal if threshold exceeded
	if s.ring.ShouldSignal() {
		tokens := s.ring.Total() / 4
		fmt.Printf("[vault] ~%d tokens non traités — invoquer prends-note\n", tokens)
	}

	// Flush ring if previous entries have been processed
	s.db.Exec(`UPDATE buffer SET processed = 1 WHERE session_id = ? AND processed = 0 AND hook = 'user_prompt' AND id < (SELECT MAX(id) FROM buffer WHERE hook = 'user_prompt')`, sid)

	w.WriteHeader(http.StatusOK)
}

func (s *Server) handlePostTool(w http.ResponseWriter, r *http.Request) {
	s.bufferHook(r, "post_tool")
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handlePostToolFailure(w http.ResponseWriter, r *http.Request) {
	s.bufferHook(r, "post_tool_failure")
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	s.bufferHook(r, "stop")
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handlePreCompact(w http.ResponseWriter, r *http.Request) {
	s.bufferHook(r, "pre_compact")
	// PreCompact stdout is NOT injected — hot-contexte skill handles this
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleSessionEnd(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)

	var data struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(body, &data)

	sid := data.SessionID
	if sid == "" {
		sid = s.currentSession()
	}

	if sid != "" {
		_, err := s.db.Exec(
			`UPDATE sessions SET ended_at = ? WHERE id = ?`,
			now(), sid,
		)
		if err != nil {
			log.Printf("session end: %v", err)
		}
	}

	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	// Periodic maintenance: vacuum and reindex
	if _, err := s.db.Exec(`PRAGMA optimize`); err != nil {
		log.Printf("optimize: %v", err)
	}
	if _, err := s.db.Exec(`VACUUM`); err != nil {
		log.Printf("vacuum: %v", err)
	}
	log.Println("setup: maintenance complete")
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "ok")
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func (s *Server) bufferHook(r *http.Request, hook string) {
	body := readBody(r)
	sid := s.currentSession()

	s.ring.Push(hook, len(body))

	_, err := s.db.Exec(
		`INSERT INTO buffer (session_id, ts, hook, payload, size_est) VALUES (?, ?, ?, jsonb(?), ?)`,
		sid, now(), hook, jsonPayload(body), len(body),
	)
	if err != nil {
		log.Printf("buffer insert (%s): %v", hook, err)
	}
}

func (s *Server) currentSession() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sessionID
}

func ensureGitignore(projectDir string) {
	path := filepath.Join(projectDir, ".gitignore")
	b, _ := os.ReadFile(path)
	lines := strings.Split(string(b), "\n")
	for _, l := range lines {
		if strings.TrimSpace(l) == gitignoreEntry {
			return
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	if len(b) > 0 && !strings.HasSuffix(string(b), "\n") {
		f.WriteString("\n")
	}
	f.WriteString(gitignoreEntry + "\n")
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	projectDir := os.Getenv("CLAUDE_PROJECT_DIR")
	if projectDir == "" {
		projectDir, _ = os.Getwd()
	}

	dbPath := filepath.Join(projectDir, dbRelPath)

	srv, err := NewServer(dbPath, projectDir)
	if err != nil {
		log.Fatalf("init: %v", err)
	}
	defer srv.db.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/hook/session-start", srv.handleSessionStart)
	mux.HandleFunc("/hook/user-prompt", srv.handleUserPrompt)
	mux.HandleFunc("/hook/post-tool", srv.handlePostTool)
	mux.HandleFunc("/hook/post-tool-failure", srv.handlePostToolFailure)
	mux.HandleFunc("/hook/stop", srv.handleStop)
	mux.HandleFunc("/hook/pre-compact", srv.handlePreCompact)
	mux.HandleFunc("/hook/session-end", srv.handleSessionEnd)
	mux.HandleFunc("/hook/setup", srv.handleSetup)
	mux.HandleFunc("/health", srv.handleHealth)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("listen %s: %v", addr, err)
	}

	log.Printf("context-vault listening on %s | db: %s", addr, dbPath)

	// Signal readiness (run.sh waits for this line)
	fmt.Printf("[vault] ready addr=%s\n", addr)

	httpSrv := &http.Server{
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	go func() {
		if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("serve: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("context-vault shutting down")
}
