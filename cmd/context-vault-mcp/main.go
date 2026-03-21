// CLAUDE:SUMMARY Thin MCP stdio client — forwards tools/call to context-vault-daemon via TCP JSON-RPC.
// CLAUDE:DEPENDS cmd/context-vault-daemon (TCP peer), cmd/context-vault/mcp.go (tool definitions reuse)
// CLAUDE:EXPORTS (binary)
package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hazyhaar/context-vault/internal/vault"
	_ "modernc.org/sqlite"
)

const (
	daemonAddr     = "localhost:9743"
	dialTimeout    = 2 * time.Second
	connectRetries = 3
)

// ── JSON-RPC types ───────────────────────────────────────────────────────────

type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type jsonrpcNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

type mcpToolDef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
}

// ── Stdout serializer ────────────────────────────────────────────────────────

type stdoutWriter struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func newStdoutWriter() *stdoutWriter {
	return &stdoutWriter{enc: json.NewEncoder(os.Stdout)}
}

func (w *stdoutWriter) encode(v any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.enc.Encode(v)
}

// ── Daemon connection ────────────────────────────────────────────────────────

type daemonConn struct {
	conn    net.Conn
	enc     *json.Encoder
	scanner *bufio.Scanner
	mu      sync.Mutex // serializes writes to daemon

	// Mux fields — populated by startMux
	muxed  bool
	respCh chan *jsonrpcResponse // responses (messages with id)
	closed chan struct{}         // closed when read goroutine exits
}

// CLAUDE:WARN retries 3 times with 500ms sleep — blocks caller up to 1.5s on daemon unreachable. Returns net.Conn that must be closed by caller.
func dialDaemon(addr string) (*daemonConn, error) {
	var conn net.Conn
	var err error
	for i := range connectRetries {
		conn, err = net.DialTimeout("tcp", addr, dialTimeout)
		if err == nil {
			break
		}
		slog.Warn("dial daemon retry", "attempt", i+1, "err", err)
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		return nil, fmt.Errorf("dial daemon %s: %w", addr, err)
	}

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)

	return &daemonConn{
		conn:    conn,
		enc:     json.NewEncoder(conn),
		scanner: scanner,
	}, nil
}

const callTimeout = 10 * time.Second

// CLAUDE:WARN launches goroutine — demuxes TCP stream into responses (respCh) and notifications (onNotif callback). Closes dc.closed and dc.respCh on disconnect — callDaemon detects this via select.
func (dc *daemonConn) startMux(onNotif func(jsonrpcNotification)) {
	dc.respCh = make(chan *jsonrpcResponse, 16)
	dc.closed = make(chan struct{})
	dc.muxed = true

	go func() {
		defer close(dc.closed)
		defer close(dc.respCh)

		for dc.scanner.Scan() {
			line := dc.scanner.Bytes()
			if len(line) == 0 {
				continue
			}

			// Peek: does the message have an "id" field?
			var peek struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if err := json.Unmarshal(line, &peek); err != nil {
				slog.Warn("mux: invalid json from daemon", "err", err)
				continue
			}

			if len(peek.ID) > 0 && string(peek.ID) != "null" {
				// Response — route to respCh
				var resp jsonrpcResponse
				if err := json.Unmarshal(line, &resp); err != nil {
					slog.Warn("mux: unmarshal response", "err", err)
					continue
				}
				dc.respCh <- &resp
			} else {
				// Notification — forward
				var notif jsonrpcNotification
				if err := json.Unmarshal(line, &notif); err != nil {
					slog.Warn("mux: unmarshal notification", "err", err)
					continue
				}
				if onNotif != nil {
					onNotif(notif)
				}
			}
		}
		if err := dc.scanner.Err(); err != nil {
			slog.Warn("mux: scanner error", "err", err)
		}
	}()
}

// CLAUDE:WARN takes mu.Lock — two modes: pre-mux (synchronous read, blocks on scanner) and post-mux (sends then waits on respCh with 10s timeout). All requests use ID=1 — no concurrent request pipelining.
func (dc *daemonConn) callDaemon(method string, params json.RawMessage) (*jsonrpcResponse, error) {
	dc.mu.Lock()

	req := jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  method,
		Params:  params,
	}
	if err := dc.enc.Encode(req); err != nil {
		dc.mu.Unlock()
		return nil, err
	}

	// Pre-mux: synchronous read (used for register)
	if !dc.muxed {
		defer dc.mu.Unlock()
		if !dc.scanner.Scan() {
			if err := dc.scanner.Err(); err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("daemon connection closed")
		}
		var resp jsonrpcResponse
		if err := json.Unmarshal(dc.scanner.Bytes(), &resp); err != nil {
			return nil, err
		}
		return &resp, nil
	}

	// Post-mux: wait on channel
	dc.mu.Unlock()
	select {
	case resp, ok := <-dc.respCh:
		if !ok {
			return nil, fmt.Errorf("daemon connection closed")
		}
		return resp, nil
	case <-dc.closed:
		return nil, fmt.Errorf("daemon connection closed")
	case <-time.After(callTimeout):
		return nil, fmt.Errorf("daemon call timeout (%s)", callTimeout)
	}
}

func (dc *daemonConn) close() {
	dc.conn.Close()
}

// forwardNotification writes a daemon notification as an MCP channel event on stdout.
// Uses notifications/claude/channel with content+meta (same format as mcp.go channelPollCheckpoints).
// Maps notify/checkpoint_created → event "checkpoint_created", etc.
func forwardNotification(out *stdoutWriter, notif jsonrpcNotification) {
	// Extract event name from method: "notify/checkpoint_created" → "checkpoint_created"
	event := notif.Method
	if i := strings.LastIndex(event, "/"); i >= 0 {
		event = event[i+1:]
	}

	// Extract params as map for building content and meta
	paramsMap, _ := toStringMap(notif.Params)

	// Build human-readable content string
	content := buildChannelContent(event, paramsMap)

	// Build meta: event + all scalar params as strings (checkpoint_id, label, etc.)
	meta := map[string]string{"event": event}
	for k, v := range paramsMap {
		meta[k] = fmt.Sprintf("%v", v)
	}

	msg := jsonrpcNotification{
		JSONRPC: "2.0",
		Method:  "notifications/claude/channel",
		Params: map[string]any{
			"content": content,
			"meta":    meta,
		},
	}
	if err := out.encode(msg); err != nil {
		slog.Warn("forward notification failed", "event", event, "err", err)
	}
}

// toStringMap converts params (any) to map[string]any, handling json.RawMessage.
func toStringMap(v any) (map[string]any, bool) {
	switch p := v.(type) {
	case map[string]any:
		return p, true
	case json.RawMessage:
		var m map[string]any
		if json.Unmarshal(p, &m) == nil {
			return m, true
		}
	}
	return nil, false
}

// buildChannelContent creates a human-readable string for the channel event body.
func buildChannelContent(event string, params map[string]any) string {
	switch event {
	case "checkpoint_answered":
		return fmt.Sprintf("Checkpoint #%v answered.\nLabel: %v\nAnswer: %v",
			params["checkpoint_id"], params["label"], params["answer"])
	case "checkpoint_created":
		return fmt.Sprintf("Checkpoint #%v created.\nLabel: %v\nQuestion: %v",
			params["checkpoint_id"], params["label"], params["question"])
	default:
		return fmt.Sprintf("Event: %s\nParams: %v", event, params)
	}
}

// ── Tool name translation ────────────────────────────────────────────────────

// toolNameToMethod converts "vault_get_context" to "vault/get_context".
func toolNameToMethod(name string) string {
	if i := strings.IndexByte(name, '_'); i > 0 {
		return name[:i] + "/" + name[i+1:]
	}
	return name
}

// ── Tool definitions (same as mcp.go) ────────────────────────────────────────

func toolDefinitions() []mcpToolDef {
	return []mcpToolDef{
		{
			Name:        "vault_get_context",
			Description: "Retourne les checkpoints, todos, decisions et contraintes actives du vault. Equivalent de buildStartContext.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"namespace": map[string]any{"type": "string", "description": "Filtrer par namespace (optionnel)"},
					"session":   map[string]any{"type": "string", "description": "Filtrer par session_origin (optionnel)"},
				},
			},
		},
		{
			Name:        "vault_search_entities",
			Description: "Recherche d'entites dans le vault par type, namespace, ou label (LIKE).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"type":      map[string]any{"type": "string", "description": "Type d'entite (todo, decision, constraint, pattern, function, etc.)"},
					"namespace": map[string]any{"type": "string", "description": "Filtrer par namespace"},
					"query":     map[string]any{"type": "string", "description": "Recherche substring sur le label"},
					"session":   map[string]any{"type": "string", "description": "Filtrer par session_origin (optionnel)"},
					"limit":     map[string]any{"type": "number", "description": "Nombre max de resultats (defaut 20, max 50)"},
				},
			},
		},
		{
			Name:        "vault_upsert_entity",
			Description: "Cree ou met a jour une entite dans le vault. Omit id pour creer, fournis id pour mettre a jour.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"label"},
				"properties": map[string]any{
					"id":          map[string]any{"type": "number", "description": "ID entite pour update (omit pour create)"},
					"namespace":   map[string]any{"type": "string", "description": "Namespace (requis pour create)"},
					"type":        map[string]any{"type": "string", "description": "Type d'entite (requis pour create)", "enum": []string{"function", "type", "package", "file", "decision", "constraint", "todo", "mission", "api", "dependency", "pattern", "credential", "checkpoint"}},
					"label":       map[string]any{"type": "string", "description": "Label de l'entite"},
					"sensitivity": map[string]any{"type": "number", "description": "0=public, 1=internal, 2=secret (defaut 0)"},
					"meta":        map[string]any{"type": "object", "description": "Metadonnees (blob_plus, blob_minus, status, priority, target_file, etc.)"},
				},
			},
		},
		{
			Name:        "vault_todo_transition",
			Description: "Change le statut d'un todo ou d'une mission. Sur mission in_progress: verrouille les todos enfants. Sur mission done: deverrouille.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"id", "status"},
				"properties": map[string]any{
					"id":     map[string]any{"type": "number", "description": "ID du todo ou de la mission"},
					"status": map[string]any{"type": "string", "description": "Nouveau statut", "enum": []string{"open", "in_progress", "blocked", "done"}},
				},
			},
		},
		{
			Name:        "vault_list_todos",
			Description: "Liste les todos avec leur statut, priorite et dependances.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"namespace":    map[string]any{"type": "string", "description": "Filtrer par namespace"},
					"session":      map[string]any{"type": "string", "description": "Filtrer par session_origin (optionnel)"},
					"include_done": map[string]any{"type": "boolean", "description": "Inclure les todos termines (defaut false)"},
				},
			},
		},
		{
			Name:        "vault_create_relation",
			Description: "Cree une relation entre deux entites.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"from_id", "to_id", "type"},
				"properties": map[string]any{
					"from_id": map[string]any{"type": "number", "description": "ID entite source"},
					"to_id":   map[string]any{"type": "number", "description": "ID entite cible"},
					"type":    map[string]any{"type": "string", "description": "Type de relation", "enum": []string{"depends_on", "blocks", "subtask_of"}},
				},
			},
		},
		{
			Name:        "vault_delete_entity",
			Description: "Supprime une entite et ses relations (CASCADE).",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"id"},
				"properties": map[string]any{
					"id": map[string]any{"type": "number", "description": "ID de l'entite a supprimer"},
				},
			},
		},
		{
			Name:        "vault_add_steps",
			Description: "Ajoute des etapes a un todo existant. Ignore les doublons (INSERT OR IGNORE).",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"todo_id", "steps"},
				"properties": map[string]any{
					"todo_id": map[string]any{"type": "number", "description": "ID du todo"},
					"steps":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Liste des etapes a ajouter"},
				},
			},
		},
		{
			Name:        "vault_step_done",
			Description: "Marque une etape comme terminee. Si toutes les etapes required sont done, le todo passe automatiquement a done.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"todo_id", "step"},
				"properties": map[string]any{
					"todo_id": map[string]any{"type": "number", "description": "ID du todo"},
					"step":    map[string]any{"type": "string", "description": "Nom de l'etape a marquer done"},
				},
			},
		},
		{
			Name:        "vault_get_entity",
			Description: "Lit une entite complete par son ID. Retourne tous les champs, meta JSON, relations et steps.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"id"},
				"properties": map[string]any{
					"id": map[string]any{"type": "number", "description": "ID de l'entite a lire"},
				},
			},
		},
		{
			Name:        "vault_assume_role",
			Description: "Change le role de la session (worker/supervisor). Un seul superviseur actif a la fois. session_id optionnel pour declarer un UUID stable.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"role"},
				"properties": map[string]any{
					"role":       map[string]any{"type": "string", "description": "Nouveau role", "enum": []string{"worker", "supervisor"}},
					"session_id": map[string]any{"type": "string", "description": "UUID stable de la conversation (optionnel, remplace le mcp-<pid>)"},
				},
			},
		},
		{
			Name:        "vault_list_workers",
			Description: "Liste les clients connectes au daemon (session_id, role, remote_addr).",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	}
}

// ── Main ─────────────────────────────────────────────────────────────────────

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	channelFlag := flag.Bool("channel", false, "Enable claude/channel capability for checkpoint notifications")
	roleFlag := flag.String("role", "worker", "Client role: worker or supervisor")
	flag.Parse()

	out := newStdoutWriter()

	var dc *daemonConn
	var localVault *vault.Vault

	// Try to dial daemon; fallback to direct SQLite if unavailable
	dc, err := dialDaemon(daemonAddr)
	if err != nil {
		slog.Warn("daemon unreachable, fallback to direct SQLite", "addr", daemonAddr, "err", err)
		localVault = openFallbackVault()
		if localVault == nil {
			slog.Error("fallback SQLite also failed — no vault available")
			os.Exit(1)
		}
	} else {
		defer dc.close()

		// Register with daemon (pre-mux, synchronous)
		sessionID := os.Getenv("CLAUDE_SESSION_ID")
		if sessionID == "" {
			sessionID = fmt.Sprintf("mcp-%d", os.Getpid())
		}
		regParams, _ := json.Marshal(map[string]string{"session_id": sessionID, "role": *roleFlag})
		if _, regErr := dc.callDaemon("register", regParams); regErr != nil {
			slog.Warn("register failed, falling back to direct SQLite", "err", regErr)
			dc.close()
			dc = nil
			localVault = openFallbackVault()
			if localVault == nil {
				slog.Error("fallback SQLite also failed")
				os.Exit(1)
			}
		} else {
			slog.Info("registered with daemon", "session", sessionID, "role", *roleFlag)

			// Start TCP mux — continuous read, dispatch notifications to MCP stdout
			dc.startMux(func(notif jsonrpcNotification) {
				forwardNotification(out, notif)
			})
		}
	}

	// Stdio MCP loop
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req jsonrpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			slog.Warn("invalid json-rpc", "err", err)
			continue
		}

		resp := handleRequest(dc, localVault, &req, *channelFlag)
		if resp != nil {
			if err := out.encode(resp); err != nil {
				slog.Error("encode response", "err", err)
				return
			}
		}
	}
	if err := scanner.Err(); err != nil {
		slog.Error("stdin scanner", "err", err)
	}
}

const dbRelPath = ".claude/vault.db"

// openFallbackVault opens vault.db directly for fallback mode.
func openFallbackVault() *vault.Vault {
	projectDir := os.Getenv("PROJECT_DIR")
	if projectDir == "" {
		projectDir, _ = os.Getwd()
	}
	dbPath := filepath.Join(projectDir, dbRelPath)
	dsn := dbPath + "?_txlock=immediate&_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		slog.Error("fallback: open db", "err", err)
		return nil
	}
	if _, err := db.Exec(vault.Schema); err != nil {
		slog.Error("fallback: schema", "err", err)
		db.Close()
		return nil
	}
	vault.Migrate(db)
	slog.Info("fallback: using direct SQLite", "db", dbPath)
	return vault.New(db, func() string { return "" })
}

func handleRequest(dc *daemonConn, localVault *vault.Vault, req *jsonrpcRequest, channelEnabled bool) *jsonrpcResponse {
	switch req.Method {
	case "initialize":
		caps := map[string]any{"tools": map[string]any{}}
		result := map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":   caps,
			"serverInfo": map[string]any{
				"name":    "context-vault",
				"version": "2.0.0",
			},
		}
		if channelEnabled {
			caps["experimental"] = map[string]any{"claude/channel": map[string]any{}}
			result["instructions"] = "Events from the context-vault channel arrive as <channel source=\"context-vault\" event=\"checkpoint_answered\" checkpoint_id=\"N\">. " +
				"They indicate that a blocking checkpoint you created has been answered by the supervisor. " +
				"Read the answer in the event body and resume your work accordingly. " +
				"If the answer says 'approuver' or equivalent, continue with the approved approach. " +
				"If the answer says 'refuser' or gives a different directive, follow that directive."
		}
		return &jsonrpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result}

	case "notifications/initialized", "notifications/cancelled":
		return nil

	case "tools/list":
		return &jsonrpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"tools": toolDefinitions()}}

	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return &jsonrpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32602, Message: "invalid params"}}
		}

		// Daemon mode: forward to daemon
		if dc != nil {
			method := toolNameToMethod(params.Name)
			daemonResp, err := dc.callDaemon(method, params.Arguments)
			if err != nil {
				slog.Error("daemon call failed", "method", method, "err", err)
				return &jsonrpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32603, Message: "daemon error: " + err.Error()}}
			}
			return &jsonrpcResponse{JSONRPC: "2.0", ID: req.ID, Result: daemonResp.Result, Error: daemonResp.Error}
		}

		// Fallback mode: dispatch locally
		if localVault != nil {
			r := dispatchLocal(localVault, params.Name, params.Arguments)
			return &jsonrpcResponse{JSONRPC: "2.0", ID: req.ID, Result: r}
		}

		return &jsonrpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32603, Message: "no daemon and no fallback available"}}

	default:
		return &jsonrpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32601, Message: "method not found: " + req.Method}}
	}
}

// dispatchLocal handles tools/call in fallback mode using local vault.
func dispatchLocal(v *vault.Vault, name string, args json.RawMessage) vault.Result {
	switch name {
	case "vault_get_context":
		var p struct {
			Namespace string `json:"namespace"`
			Session   string `json:"session"`
		}
		json.Unmarshal(args, &p)
		return v.GetContext(p.Namespace, p.Session)
	case "vault_search_entities":
		var p struct {
			Type      string `json:"type"`
			Namespace string `json:"namespace"`
			Query     string `json:"query"`
			Session   string `json:"session"`
			Limit     int    `json:"limit"`
		}
		json.Unmarshal(args, &p)
		return v.SearchEntities(p.Type, p.Namespace, p.Query, p.Session, p.Limit)
	case "vault_upsert_entity":
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
		return v.UpsertEntity(p.ID, p.Namespace, p.Type, p.Label, p.Sensitivity, p.Meta)
	case "vault_todo_transition":
		var p struct {
			ID     int64  `json:"id"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return vault.ErrorResult("invalid params: " + err.Error())
		}
		return v.TodoTransition(p.ID, p.Status)
	case "vault_list_todos":
		var p struct {
			Namespace   string `json:"namespace"`
			Session     string `json:"session"`
			IncludeDone bool   `json:"include_done"`
		}
		json.Unmarshal(args, &p)
		return v.ListTodos(p.Namespace, p.Session, p.IncludeDone)
	case "vault_create_relation":
		var p struct {
			FromID int64  `json:"from_id"`
			ToID   int64  `json:"to_id"`
			Type   string `json:"type"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return vault.ErrorResult("invalid params: " + err.Error())
		}
		return v.CreateRelation(p.FromID, p.ToID, p.Type)
	case "vault_delete_entity":
		var p struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return vault.ErrorResult("invalid params: " + err.Error())
		}
		return v.DeleteEntity(p.ID)
	case "vault_add_steps":
		var p struct {
			TodoID int64    `json:"todo_id"`
			Steps  []string `json:"steps"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return vault.ErrorResult("invalid params: " + err.Error())
		}
		return v.AddSteps(p.TodoID, p.Steps)
	case "vault_step_done":
		var p struct {
			TodoID int64  `json:"todo_id"`
			Step   string `json:"step"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return vault.ErrorResult("invalid params: " + err.Error())
		}
		return v.StepDone(p.TodoID, p.Step)
	case "vault_get_entity":
		var p struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return vault.ErrorResult("invalid params: " + err.Error())
		}
		return v.GetEntity(p.ID)
	default:
		return vault.ErrorResult("unknown tool: " + name)
	}
}
