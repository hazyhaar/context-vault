// CLAUDE:SUMMARY context-vault daemon — TCP JSON-RPC server over localhost, sole writer to vault.db.
// CLAUDE:DEPENDS internal/vault
// CLAUDE:EXPORTS (binary)
package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/hazyhaar/context-vault/internal/vault"
	_ "modernc.org/sqlite"
)

const (
	listenAddr = "localhost:9743"
	dbRelPath  = ".claude/vault.db"
)

// ── JSON-RPC types ───────────────────────────────────────────────────────────

type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonrpcResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ── Client tracking ──────────────────────────────────────────────────────────

type clientInfo struct {
	sessionID string
	role      string // "worker" or "supervisor"
	conn      net.Conn
	enc       *json.Encoder
}

type daemon struct {
	v       *vault.Vault
	db      *sql.DB
	mu      sync.RWMutex
	clients map[net.Conn]*clientInfo
}

func newDaemon(db *sql.DB) *daemon {
	d := &daemon{
		db:      db,
		clients: make(map[net.Conn]*clientInfo),
	}
	d.v = vault.New(db, func() string { return "" })
	return d
}

func (d *daemon) addClient(conn net.Conn, info *clientInfo) {
	d.mu.Lock()
	d.clients[conn] = info
	d.mu.Unlock()
}

func (d *daemon) removeClient(conn net.Conn) {
	d.mu.Lock()
	delete(d.clients, conn)
	d.mu.Unlock()
}

// ── Connection handler ───────────────────────────────────────────────────────

func (d *daemon) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	remote := conn.RemoteAddr().String()
	slog.Info("client connected", "remote", remote)

	info := &clientInfo{conn: conn, enc: json.NewEncoder(conn)}
	d.addClient(conn, info)
	defer d.removeClient(conn)

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)

	for scanner.Scan() {
		if ctx.Err() != nil {
			return
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req jsonrpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			slog.Warn("invalid json-rpc", "remote", remote, "err", err)
			continue
		}

		resp := d.handleRequest(&req, info)
		if resp != nil {
			if err := info.enc.Encode(resp); err != nil {
				slog.Warn("encode error", "remote", remote, "err", err)
				return
			}
		}
	}
	if err := scanner.Err(); err != nil {
		slog.Info("client disconnected", "remote", remote, "err", err)
	} else {
		slog.Info("client disconnected", "remote", remote)
	}
}

// ── Request router ───────────────────────────────────────────────────────────

func (d *daemon) handleRequest(req *jsonrpcRequest, info *clientInfo) *jsonrpcResponse {
	switch req.Method {
	case "register":
		return d.handleRegister(req, info)
	case "vault/get_context":
		return d.callTool(req, func(args json.RawMessage) vault.Result {
			var p struct {
				Namespace string `json:"namespace"`
				Session   string `json:"session"`
			}
			json.Unmarshal(args, &p)
			return d.v.GetContext(p.Namespace, p.Session)
		})
	case "vault/search_entities":
		return d.callTool(req, func(args json.RawMessage) vault.Result {
			var p struct {
				Type      string `json:"type"`
				Namespace string `json:"namespace"`
				Query     string `json:"query"`
				Session   string `json:"session"`
				Limit     int    `json:"limit"`
			}
			json.Unmarshal(args, &p)
			return d.v.SearchEntities(p.Type, p.Namespace, p.Query, p.Session, p.Limit)
		})
	case "vault/upsert_entity":
		return d.callTool(req, func(args json.RawMessage) vault.Result {
			var p struct {
				ID          *int64          `json:"id"`
				Namespace   string          `json:"namespace"`
				Type        string          `json:"type"`
				Label       string          `json:"label"`
				Sensitivity int             `json:"sensitivity"`
				Meta        json.RawMessage `json:"meta"`
			}
			if err := json.Unmarshal(args, &p); err != nil {
				return vault.ErrorResult("invalid params: " + err.Error())
			}
			return d.v.UpsertEntity(p.ID, p.Namespace, p.Type, p.Label, p.Sensitivity, p.Meta)
		})
	case "vault/todo_transition":
		return d.callTool(req, func(args json.RawMessage) vault.Result {
			var p struct {
				ID     int64  `json:"id"`
				Status string `json:"status"`
			}
			if err := json.Unmarshal(args, &p); err != nil {
				return vault.ErrorResult("invalid params: " + err.Error())
			}
			return d.v.TodoTransition(p.ID, p.Status)
		})
	case "vault/list_todos":
		return d.callTool(req, func(args json.RawMessage) vault.Result {
			var p struct {
				Namespace   string `json:"namespace"`
				Session     string `json:"session"`
				IncludeDone bool   `json:"include_done"`
			}
			json.Unmarshal(args, &p)
			return d.v.ListTodos(p.Namespace, p.Session, p.IncludeDone)
		})
	case "vault/create_relation":
		return d.callTool(req, func(args json.RawMessage) vault.Result {
			var p struct {
				FromID int64  `json:"from_id"`
				ToID   int64  `json:"to_id"`
				Type   string `json:"type"`
			}
			if err := json.Unmarshal(args, &p); err != nil {
				return vault.ErrorResult("invalid params: " + err.Error())
			}
			return d.v.CreateRelation(p.FromID, p.ToID, p.Type)
		})
	case "vault/delete_entity":
		return d.callTool(req, func(args json.RawMessage) vault.Result {
			var p struct {
				ID int64 `json:"id"`
			}
			if err := json.Unmarshal(args, &p); err != nil {
				return vault.ErrorResult("invalid params: " + err.Error())
			}
			return d.v.DeleteEntity(p.ID)
		})
	case "vault/add_steps":
		return d.callTool(req, func(args json.RawMessage) vault.Result {
			var p struct {
				TodoID int64    `json:"todo_id"`
				Steps  []string `json:"steps"`
			}
			if err := json.Unmarshal(args, &p); err != nil {
				return vault.ErrorResult("invalid params: " + err.Error())
			}
			return d.v.AddSteps(p.TodoID, p.Steps)
		})
	case "vault/step_done":
		return d.callTool(req, func(args json.RawMessage) vault.Result {
			var p struct {
				TodoID int64  `json:"todo_id"`
				Step   string `json:"step"`
			}
			if err := json.Unmarshal(args, &p); err != nil {
				return vault.ErrorResult("invalid params: " + err.Error())
			}
			return d.v.StepDone(p.TodoID, p.Step)
		})
	case "vault/get_entity":
		return d.callTool(req, func(args json.RawMessage) vault.Result {
			var p struct {
				ID int64 `json:"id"`
			}
			if err := json.Unmarshal(args, &p); err != nil {
				return vault.ErrorResult("invalid params: " + err.Error())
			}
			return d.v.GetEntity(p.ID)
		})
	default:
		return &jsonrpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32601, Message: "method not found: " + req.Method}}
	}
}

func (d *daemon) callTool(req *jsonrpcRequest, fn func(json.RawMessage) vault.Result) *jsonrpcResponse {
	r := fn(req.Params)
	return &jsonrpcResponse{JSONRPC: "2.0", ID: req.ID, Result: r}
}

func (d *daemon) handleRegister(req *jsonrpcRequest, info *clientInfo) *jsonrpcResponse {
	var p struct {
		SessionID string `json:"session_id"`
		Role      string `json:"role"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return &jsonrpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32602, Message: "invalid params"}}
	}
	if p.Role != "worker" && p.Role != "supervisor" {
		return &jsonrpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32602, Message: "role must be 'worker' or 'supervisor'"}}
	}

	d.mu.Lock()
	info.sessionID = p.SessionID
	info.role = p.Role
	d.mu.Unlock()

	slog.Info("client registered", "session", p.SessionID, "role", p.Role)
	return &jsonrpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]string{"status": "registered"}}
}

// ── Checkpoint watcher ───────────────────────────────────────────────────────

func (d *daemon) watchCheckpoints(ctx context.Context) {
	watermark := time.Now().Unix()
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		checkpoints, err := d.v.PollAnsweredCheckpoints(watermark)
		if err != nil {
			slog.Warn("watch: poll error", "err", err)
			continue
		}

		for _, cp := range checkpoints {
			d.mu.RLock()
			for _, ci := range d.clients {
				notif := map[string]any{
					"jsonrpc": "2.0",
					"method":  "notify/checkpoint_answered",
					"params": map[string]any{
						"checkpoint_id": cp.ID,
						"label":         cp.Label,
						"answer":        cp.Answer,
					},
				}
				if err := ci.enc.Encode(notif); err != nil {
					slog.Warn("watch: push failed", "session", ci.sessionID, "err", err)
				}
			}
			d.mu.RUnlock()

			if cp.TsUpdated > watermark {
				watermark = cp.TsUpdated
			}
		}
	}
}

// ── Main ─────────────────────────────────────────────────────────────────────

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	projectDir := os.Getenv("PROJECT_DIR")
	if projectDir == "" {
		projectDir, _ = os.Getwd()
	}
	dbPath := filepath.Join(projectDir, dbRelPath)

	if err := os.MkdirAll(filepath.Dir(dbPath), 0700); err != nil {
		slog.Error("mkdirall", "err", err)
		os.Exit(1)
	}

	dsn := dbPath + "?_txlock=immediate&_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		slog.Error("open db", "err", err)
		os.Exit(1)
	}
	defer db.Close()
	db.SetMaxOpenConns(4)

	if _, err := db.Exec(vault.Schema); err != nil {
		slog.Error("schema", "err", err)
		os.Exit(1)
	}
	vault.Migrate(db)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	d := newDaemon(db)

	// Start checkpoint watcher
	go d.watchCheckpoints(ctx)

	// WAL checkpoint goroutine
	go func() {
		ticker := time.NewTicker(2 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				db.Exec("PRAGMA wal_checkpoint(PASSIVE)")
			}
		}
	}()

	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		slog.Error("listen", "addr", listenAddr, "err", err)
		os.Exit(1)
	}
	slog.Info("daemon listening", "addr", listenAddr, "db", dbPath)

	// Accept loop in goroutine — cancel ctx to stop
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				if ctx.Err() != nil {
					return // shutting down
				}
				slog.Warn("accept", "err", err)
				continue
			}
			go d.handleConn(ctx, conn)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")
	ln.Close()

	// Give connections 2s to drain
	time.Sleep(2 * time.Second)
	fmt.Fprintln(os.Stderr, "daemon stopped")
}
