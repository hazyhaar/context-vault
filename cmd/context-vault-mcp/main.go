// CLAUDE:SUMMARY Thin MCP stdio client — forwards tools/call to context-vault-daemon via TCP JSON-RPC.
// CLAUDE:DEPENDS cmd/context-vault-daemon (TCP peer), cmd/context-vault/mcp.go (tool definitions reuse)
// CLAUDE:EXPORTS (binary)
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"time"
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
}

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

// callDaemon sends a JSON-RPC request to the daemon and returns the response.
func (dc *daemonConn) callDaemon(method string, params json.RawMessage) (*jsonrpcResponse, error) {
	dc.mu.Lock()
	defer dc.mu.Unlock()

	req := jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  method,
		Params:  params,
	}
	if err := dc.enc.Encode(req); err != nil {
		return nil, err
	}

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

func (dc *daemonConn) close() {
	dc.conn.Close()
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
	}
}

// ── Main ─────────────────────────────────────────────────────────────────────

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	channelFlag := flag.Bool("channel", false, "Enable claude/channel capability for checkpoint notifications")
	roleFlag := flag.String("role", "worker", "Client role: worker or supervisor")
	flag.Parse()

	out := newStdoutWriter()

	// Dial daemon
	dc, err := dialDaemon(daemonAddr)
	if err != nil {
		slog.Error("cannot connect to daemon", "addr", daemonAddr, "err", err)
		os.Exit(1)
	}
	defer dc.close()

	// Register with daemon
	sessionID := os.Getenv("CLAUDE_SESSION_ID")
	if sessionID == "" {
		sessionID = fmt.Sprintf("mcp-%d", os.Getpid())
	}
	regParams, _ := json.Marshal(map[string]string{"session_id": sessionID, "role": *roleFlag})
	if _, err := dc.callDaemon("register", regParams); err != nil {
		slog.Error("register failed", "err", err)
		os.Exit(1)
	}
	slog.Info("registered with daemon", "session", sessionID, "role", *roleFlag)

	// TODO: background goroutine to read push notifications from daemon
	// For now, the daemon sends notifications inline (not yet implemented as async push).
	// This will be wired in #694 (event routing).

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

		resp := handleRequest(dc, &req, *channelFlag)
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

func handleRequest(dc *daemonConn, req *jsonrpcRequest, channelEnabled bool) *jsonrpcResponse {
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

		// Translate tool name to daemon method: vault_get_context → vault/get_context
		method := toolNameToMethod(params.Name)

		daemonResp, err := dc.callDaemon(method, params.Arguments)
		if err != nil {
			slog.Error("daemon call failed", "method", method, "err", err)
			return &jsonrpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32603, Message: "daemon error: " + err.Error()}}
		}

		// Forward daemon response with original request ID
		return &jsonrpcResponse{JSONRPC: "2.0", ID: req.ID, Result: daemonResp.Result, Error: daemonResp.Error}

	default:
		return &jsonrpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32601, Message: "method not found: " + req.Method}}
	}
}
