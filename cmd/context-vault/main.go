// CLAUDE:SUMMARY HTTP hooks server — persists Claude Code session events to SQLite JSONB, signals compaction via RingDumper.
// CLAUDE:DEPENDS modernc.org/sqlite (external, CGO_ENABLED=0)
// CLAUDE:EXPORTS Server, RingDumper, NewServer (package main — not importable, used by mcp.go same package)
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hazyhaar/context-vault/internal/vault"
	_ "modernc.org/sqlite"
)

const (
	addr             = "localhost:9742"
	ringSize         = 10
	ringThreshold    = 2000 // estimated tokens before signaling
	maxRings         = 50   // safety cap — evict all if exceeded (session-end is the normal cleanup path)
	dbRelPath        = ".claude/vault.db"
	gitignoreEntry   = ".claude/vault.db"
)

// Schema is imported from internal/vault.
var schema = vault.Schema

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

// CLAUDE:WARN takes mu.Lock — mutates buf, head, total. Evicts oldest entry.
func (r *RingDumper) Push(hook string, payloadSize int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	evicted := r.buf[r.head]
	r.total -= evicted.size
	r.buf[r.head] = entry{hook: hook, size: payloadSize, ts: time.Now().Unix()}
	r.total += payloadSize
	r.head = (r.head + 1) % ringSize
}

// CLAUDE:WARN takes mu.Lock — threshold is total/4 > 2000 (estimated tokens, not bytes).
func (r *RingDumper) ShouldSignal() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.total/4 > ringThreshold
}

// CLAUDE:WARN takes mu.Lock — zeroes entire ring buffer and total counter.
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

// Server holds the HTTP server state. Must be created via NewServer.
// mu protects rings and lastSessionID. db is safe for concurrent use (sql.DB internal pool).
type Server struct {
	db             *sql.DB
	vault          *vault.Vault
	rings          map[string]*RingDumper // per-session ring buffers, guarded by mu
	projectDir     string
	lastSessionID  string // fallback when payload has no session_id, guarded by mu
	channelEnabled bool   // true when -channel flag is passed (MCP channel mode)
	mu             sync.RWMutex
}

// CLAUDE:WARN touches filesystem (MkdirAll, Chmod) and opens SQLite. DSN includes _pragma for WAL/FK.
func NewServer(dbPath, projectDir string) (*Server, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0700); err != nil {
		return nil, fmt.Errorf("mkdirall %s: %w", filepath.Dir(dbPath), err)
	}

	dsn := dbPath + "?_txlock=immediate&_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(2) // 2: one for MCP requests, one for checkpoint goroutine

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}

	vault.Migrate(db)

	if err := os.Chmod(dbPath, 0600); err != nil {
		slog.Warn("chmod failed", "path", dbPath, "err", err)
	}

	srv := &Server{
		db:         db,
		rings:      make(map[string]*RingDumper),
		projectDir: projectDir,
	}
	srv.vault = vault.New(db, srv.currentSession)
	return srv, nil
}

// readBody reads and returns the request body as raw bytes.
func readBody(r *http.Request) []byte {
	b, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		slog.Warn("readBody failed", "err", err)
	}
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

// CLAUDE:WARN takes mu.Lock to set sessionID. Returns context summary in HTTP response body.
func (s *Server) handleSessionStart(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	slog.Info("session-start received", "body_len", len(body), "body", string(body))

	var data struct {
		SessionID  string `json:"session_id"`
		ProjectDir string `json:"project_dir"`
		Model      string `json:"model"`
		Trigger    string `json:"trigger"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		slog.Warn("session-start unmarshal", "err", err)
	}
	slog.Info("session-start parsed", "session_id", data.SessionID, "project_dir", data.ProjectDir)

	if data.ProjectDir == "" {
		data.ProjectDir = s.projectDir
	}

	if data.SessionID != "" {
		s.setSession(data.SessionID)
	}

	if data.SessionID != "" {
		_, err := s.db.ExecContext(r.Context(),
			`INSERT OR IGNORE INTO sessions (id, started_at, project, model) VALUES (?, ?, ?, ?)`,
			data.SessionID, now(), data.ProjectDir, data.Model,
		)
		if err != nil {
			slog.Error("session insert", "err", err)
		}
	}

	// Ensure .gitignore
	if data.ProjectDir != "" {
		ensureGitignore(data.ProjectDir)
	}

	// Inject context summary — varies by trigger
	ctx := s.buildStartContext(data.SessionID, data.Trigger)
	writeHookResponse(w, ctx)
}

// CLAUDE:WARN runs 3 SQL queries with context.Background — not cancellable by HTTP request.
func (s *Server) buildStartContext(sessionID, trigger string) string {
	var sb strings.Builder
	switch trigger {
	case "compact", "clear":
		sb.WriteString("[vault] Compactage effectué — contexte restauré depuis vault.db\n")
	default:
		sb.WriteString("[vault] Session active — vault.db initialisé\n")
	}

	if sessionID != "" {
		sb.WriteString(fmt.Sprintf("Session : %s\n", sessionID))
	}

	// Single query: todos, decisions, constraints — ordered by type then priority/recency
	rows, err := s.db.QueryContext(context.Background(), `
		SELECT id, type, label, meta->>'$.priority', meta->>'$.blob_plus', meta->>'$.status'
		FROM entities
		WHERE type IN ('todo','decision','constraint')
		  AND (type != 'todo' OR meta->>'$.status' != 'done')
		  AND sensitivity < 2
		ORDER BY
		  CASE type WHEN 'todo' THEN 0 WHEN 'decision' THEN 1 WHEN 'constraint' THEN 2 END,
		  CASE meta->>'$.priority' WHEN 'high' THEN 0 WHEN 'normal' THEN 1 ELSE 2 END,
		  ts_updated DESC
		LIMIT 25
	`)
	if err != nil {
		slog.Warn("buildStartContext query", "err", err)
	} else {
		type todoEntry struct {
			id     int64
			line   string
			status string
		}
		var todoEntries []todoEntry
		var decisions, constraints []string
		for rows.Next() {
			var id int64
			var typ, label, priority, blob, status string
			if rows.Scan(&id, &typ, &label, &priority, &blob, &status) != nil {
				continue
			}
			switch typ {
			case "todo":
				todoEntries = append(todoEntries, todoEntry{id: id, line: fmt.Sprintf("- #%d %s [%s]", id, label, priority), status: status})
			case "decision":
				if blob != "" {
					decisions = append(decisions, fmt.Sprintf("- %s : %s", label, blob))
				} else {
					decisions = append(decisions, "- "+label)
				}
			case "constraint":
				constraints = append(constraints, "- "+label)
			}
		}
		if err := rows.Err(); err != nil {
			slog.Warn("buildStartContext iteration", "err", err)
		}
		rows.Close()

		// Enrich in_progress todos with remaining steps
		if len(todoEntries) > 0 {
			var todos []string
			for _, te := range todoEntries {
				line := te.line
				if te.status == "in_progress" {
					stepRows, err := s.db.QueryContext(context.Background(),
						`SELECT step, done FROM todo_steps WHERE todo_id = ? AND required = 1 ORDER BY rowid`, te.id)
					if err == nil {
						var pending []string
						for stepRows.Next() {
							var step string
							var done int
							if stepRows.Scan(&step, &done) == nil && done == 0 {
								pending = append(pending, step)
							}
						}
						stepRows.Close()
						if len(pending) > 0 {
							line += fmt.Sprintf(" — steps restants: %s", strings.Join(pending, ", "))
						}
					}
				}
				todos = append(todos, line)
			}
			sb.WriteString("Todos ouverts :\n" + strings.Join(todos, "\n") + "\n")
		}
		if len(decisions) > 0 {
			sb.WriteString("Décisions actives :\n" + strings.Join(decisions, "\n") + "\n")
		}
		if len(constraints) > 0 {
			sb.WriteString("Contraintes :\n" + strings.Join(constraints, "\n") + "\n")
		}
	}

	// Directive
	switch trigger {
	case "compact", "clear":
		sb.WriteString("\n→ Reprends le travail. Utilise prends-note si tu identifies quelque chose d'important avant de continuer.")
	default:
		sb.WriteString("\n→ Consulte les todos et contraintes ci-dessus pour orienter ta réponse.")
	}

	return sb.String()
}

// CLAUDE:WARN returns "[vault] ~N tokens" in HTTP response body when threshold exceeded.
func (s *Server) handleUserPrompt(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	sid := extractSessionID(body)
	if sid == "" {
		sid = s.currentSession()
	}

	ring := s.ringFor(sid)

	// Detect post-compact: is there a pre_compact since the previous user_prompt?
	var compactDetected bool
	var dummy int
	err := s.db.QueryRowContext(r.Context(),
		`SELECT 1 FROM buffer
		 WHERE session_id = ?
		   AND hook = 'pre_compact'
		   AND ts > COALESCE(
		     (SELECT ts FROM buffer WHERE session_id = ? AND hook = 'user_prompt' ORDER BY ts DESC LIMIT 1),
		     0
		   )
		 LIMIT 1`, sid, sid,
	).Scan(&dummy)
	if err == nil {
		compactDetected = true
	}
	slog.Info("compact detection", "sid", sid, "detected", compactDetected, "err", err)

	// Buffer the prompt
	_, err = s.db.ExecContext(r.Context(),
		`INSERT INTO buffer (session_id, ts, hook, payload, size_est) VALUES (?, ?, 'user_prompt', jsonb(?), ?)`,
		sid, now(), jsonPayload(body), len(body),
	)
	if err != nil {
		slog.Error("buffer insert", "hook", "user_prompt", "err", err)
	}

	ring.Push("user_prompt", len(body))

	// Post-compact: inject restored context
	if compactDetected {
		ctx := s.buildStartContext(sid, "compact")
		slog.Info("post-compact injection", "sid", sid, "ctx_len", len(ctx))
		writeHookResponse(w, ctx)
		return
	}

	// Flush ring if previous entries have been processed
	if _, err := s.db.ExecContext(r.Context(),
		`UPDATE buffer SET processed = 1 WHERE session_id = ? AND processed = 0 AND hook = 'user_prompt' AND id < (SELECT MAX(id) FROM buffer WHERE hook = 'user_prompt')`,
		sid,
	); err != nil {
		slog.Warn("buffer flush", "err", err)
	}

	// Signal if threshold exceeded — Claude Code HTTP hooks require JSON with "additionalContext"
	if ring.ShouldSignal() {
		tokens := ring.Total() / 4
		writeHookResponse(w, fmt.Sprintf("[vault] ~%d tokens non traités — invoquer prends-note", tokens))
	} else {
		w.WriteHeader(http.StatusOK)
	}
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

func (s *Server) handleSubagentStop(w http.ResponseWriter, r *http.Request) {
	s.bufferHook(r, "subagent_stop")
	w.WriteHeader(http.StatusOK)
}

// CLAUDE:WARN marks ALL unprocessed buffer entries as processed. Cannot inject context — only SessionStart/UserPromptSubmit can.
func (s *Server) handlePreCompact(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	sid := extractSessionID(body)
	if sid == "" {
		sid = s.currentSession()
	}

	s.ringFor(sid).Push("pre_compact", len(body))
	_, bufErr := s.db.ExecContext(r.Context(),
		`INSERT INTO buffer (session_id, ts, hook, payload, size_est) VALUES (?, ?, 'pre_compact', jsonb(?), ?)`,
		sid, now(), jsonPayload(body), len(body),
	)
	if bufErr != nil {
		slog.Error("buffer insert", "hook", "pre_compact", "err", bufErr)
	}

	// Mark all unprocessed buffer entries as processed
	if _, err := s.db.ExecContext(r.Context(),
		`UPDATE buffer SET processed = 1 WHERE session_id = ? AND processed = 0`,
		sid,
	); err != nil {
		slog.Warn("pre-compact buffer flush", "err", err)
	}

	// Log compaction event
	if _, err := s.db.ExecContext(r.Context(),
		`INSERT INTO compact_log (ts, session_id, trigger, reasoning) VALUES (unixepoch(), ?, 'auto', 'pre-compact hook received')`,
		sid,
	); err != nil {
		slog.Warn("compact_log insert", "err", err)
	}

	// PreCompact cannot inject context — only SessionStart/UserPromptSubmit can.
	// Post-compact injection is handled by handleUserPrompt detecting pre_compact in buffer.
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleSessionEnd(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)

	var data struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		slog.Warn("session-end unmarshal", "err", err)
	}

	sid := data.SessionID
	if sid == "" {
		sid = s.currentSession()
	}

	if sid != "" {
		_, err := s.db.ExecContext(r.Context(),
			`UPDATE sessions SET ended_at = ? WHERE id = ?`,
			now(), sid,
		)
		if err != nil {
			slog.Error("session end", "err", err)
		}
		s.evictRing(sid)
	}

	w.WriteHeader(http.StatusOK)
}

// CLAUDE:WARN runs PRAGMA optimize + VACUUM — blocks DB for duration of VACUUM. Called by Setup hook (rare).
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	// Periodic maintenance: vacuum and reindex
	if _, err := s.db.ExecContext(r.Context(), `PRAGMA optimize`); err != nil {
		slog.Error("optimize", "err", err)
	}
	if _, err := s.db.ExecContext(r.Context(), `VACUUM`); err != nil {
		slog.Error("vacuum", "err", err)
	}
	slog.Info("setup: maintenance complete")
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "ok")
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// writeHookResponse writes a Claude Code HTTP hook response with additionalContext.
func writeHookResponse(w http.ResponseWriter, context string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"additionalContext": context})
}

func (s *Server) bufferHook(r *http.Request, hook string) {
	body := readBody(r)
	sid := extractSessionID(body)
	if sid == "" {
		sid = s.currentSession()
	}

	s.ringFor(sid).Push(hook, len(body))

	_, err := s.db.ExecContext(r.Context(),
		`INSERT INTO buffer (session_id, ts, hook, payload, size_est) VALUES (?, ?, ?, jsonb(?), ?)`,
		sid, now(), hook, jsonPayload(body), len(body),
	)
	if err != nil {
		slog.Error("buffer insert", "hook", hook, "err", err)
	}
}

func (s *Server) currentSession() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastSessionID
}

func (s *Server) setSession(sid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastSessionID = sid
}

func (s *Server) evictRing(sid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rings, sid)
}

// CLAUDE:WARN takes mu.Lock — creates RingDumper if absent. Purges all rings if map exceeds maxRings (safety net for missed session-end).
func (s *Server) ringFor(sid string) *RingDumper {
	if sid == "" {
		sid = s.currentSession()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.rings) > maxRings {
		clear(s.rings)
	}
	r, ok := s.rings[sid]
	if !ok {
		r = &RingDumper{}
		s.rings[sid] = r
	}
	return r
}

// extractSessionID tries to get session_id from a JSON payload.
func extractSessionID(body []byte) string {
	var d struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(body, &d)
	return d.SessionID
}

// CLAUDE:WARN touches filesystem — appends to .gitignore if entry missing. No-op on error.
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
		if _, err := f.WriteString("\n"); err != nil {
			slog.Warn("gitignore write", "err", err)
		}
	}
	if _, err := f.WriteString(gitignoreEntry + "\n"); err != nil {
		slog.Warn("gitignore write", "err", err)
	}
}

// ── Main ──────────────────────────────────────────────────────────────────────

// CLAUDE:WARN reads all stdin, forwards to HTTP localhost:9742. Silent return on any network error — caller gets no output.
func hookProxy(endpoint string) {
	payload, _ := io.ReadAll(os.Stdin)
	resp, err := http.Post("http://"+addr+"/hook/"+endpoint,
		"application/json", bytes.NewReader(payload))
	if err != nil {
		return
	}
	defer resp.Body.Close()
	var result map[string]any
	if json.NewDecoder(resp.Body).Decode(&result) != nil {
		return
	}
	if ctx, ok := result["additionalContext"].(string); ok && ctx != "" {
		fmt.Print(ctx)
	}
}

// CLAUDE:WARN 3 execution modes via os.Args[1]: "user-prompt"/"session-start" (stdin→HTTP proxy), "mcp" (stdio JSON-RPC), default (HTTP server on :9742).
func main() {
	// Subcommand mode: act as command hook proxy
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "user-prompt", "session-start":
			hookProxy(os.Args[1])
			return
		case "mcp":
			// MCP stdio server — JSON-RPC 2.0 over stdin/stdout
			projectDir := ""
			channelEnabled := false
			for i, arg := range os.Args[2:] {
				if arg == "-project" && i+3 < len(os.Args) {
					projectDir = os.Args[i+3]
				}
				if arg == "-channel" {
					channelEnabled = true
				}
			}
			if projectDir == "" {
				projectDir = os.Getenv("CLAUDE_PROJECT_DIR")
			}
			if projectDir == "" {
				projectDir, _ = os.Getwd()
			}
			slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
			srv, err := NewServer(filepath.Join(projectDir, dbRelPath), projectDir)
			if err != nil {
				slog.Error("mcp init failed", "err", err)
				os.Exit(1)
			}
			defer srv.db.Close()
			runMCP(srv, channelEnabled)
			return
		}
	}

	projectDir := ""
	for i, arg := range os.Args[1:] {
		if arg == "-project" && i+2 < len(os.Args) {
			projectDir = os.Args[i+2]
			break
		}
	}
	if projectDir == "" {
		projectDir = os.Getenv("CLAUDE_PROJECT_DIR")
	}
	if projectDir == "" {
		projectDir, _ = os.Getwd()
	}

	dbPath := filepath.Join(projectDir, dbRelPath)

	srv, err := NewServer(dbPath, projectDir)
	if err != nil {
		slog.Error("init failed", "err", err)
		os.Exit(1)
	}
	defer srv.db.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/hook/session-start", srv.handleSessionStart)
	mux.HandleFunc("/hook/user-prompt", srv.handleUserPrompt)
	mux.HandleFunc("/hook/post-tool", srv.handlePostTool)
	mux.HandleFunc("/hook/post-tool-failure", srv.handlePostToolFailure)
	mux.HandleFunc("/hook/stop", srv.handleStop)
	mux.HandleFunc("/hook/subagent-stop", srv.handleSubagentStop)
	mux.HandleFunc("/hook/pre-compact", srv.handlePreCompact)
	mux.HandleFunc("/hook/session-end", srv.handleSessionEnd)
	mux.HandleFunc("/hook/setup", srv.handleSetup)
	mux.HandleFunc("/health", srv.handleHealth)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		slog.Error("listen failed", "addr", addr, "err", err)
		os.Exit(1)
	}

	slog.Info("context-vault listening", "addr", addr, "db", dbPath)

	// Signal readiness (run.sh waits for this line)
	fmt.Printf("[vault] ready addr=%s\n", addr)

	httpSrv := &http.Server{
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	go func() {
		if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("serve", "err", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	slog.Info("context-vault shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		slog.Error("shutdown", "err", err)
	}
}
