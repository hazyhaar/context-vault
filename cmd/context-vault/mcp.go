// CLAUDE:SUMMARY MCP stdio server — JSON-RPC 2.0 over stdin/stdout, 10 vault tools for entity CRUD.
// CLAUDE:DEPENDS main.go (same package: Server, NewServer, context.Background SQL pattern)
// CLAUDE:EXPORTS runMCP (package main — called by main() when os.Args[1]=="mcp")
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/hazyhaar/context-vault/internal/vault"
)

// ── JSON-RPC 2.0 wire types ─────────────────────────────────────────────────

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

// ── MCP types ────────────────────────────────────────────────────────────────

type mcpToolDef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
}

type mcpContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type mcpToolResult struct {
	Content []mcpContent `json:"content"`
	IsError bool         `json:"isError,omitempty"`
}

// ── Main loop ────────────────────────────────────────────────────────────────

// stdoutWriter serializes writes to the JSON-RPC stdout stream.
// Both the main request loop and the channel polling goroutine write here.
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

// jsonrpcNotification is a server-initiated notification (no ID field).
type jsonrpcNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

func runMCP(srv *Server, channelEnabled bool) {
	out := newStdoutWriter()
	srv.channelEnabled = channelEnabled

	// WAL checkpoint goroutine — prevents WAL bloat when multiple MCP instances share vault.db.
	go func() {
		ticker := time.NewTicker(2 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			if _, err := srv.db.Exec("PRAGMA wal_checkpoint(PASSIVE)"); err != nil {
				slog.Warn("mcp: wal checkpoint", "err", err)
			}
		}
	}()

	// Channel polling goroutine — only starts when -channel flag is passed.
	// Detects checkpoint answers and pushes them as channel events.
	if channelEnabled {
		slog.Info("mcp: channel mode enabled — polling checkpoints")
		go channelPollCheckpoints(srv, out)
	}

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req jsonrpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			slog.Warn("mcp: invalid json-rpc", "err", err)
			continue
		}
		resp := handleMCPRequest(srv, &req)
		if resp != nil {
			if err := out.encode(resp); err != nil {
				slog.Error("mcp: encode response", "err", err)
				return
			}
		}
	}
	if err := scanner.Err(); err != nil {
		slog.Error("mcp: scanner", "err", err)
	}
}

// channelPollCheckpoints polls vault.db every 3s for blocking checkpoints that
// received an answer. Uses vault.PollAnsweredCheckpoints.
func channelPollCheckpoints(srv *Server, out *stdoutWriter) {
	watermark := time.Now().Unix()

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		checkpoints, err := srv.vault.PollAnsweredCheckpoints(watermark)
		if err != nil {
			slog.Warn("channel: poll checkpoints", "err", err)
			continue
		}

		for _, cp := range checkpoints {
			notif := jsonrpcNotification{
				JSONRPC: "2.0",
				Method:  "notifications/claude/channel",
				Params: map[string]any{
					"content": fmt.Sprintf("Checkpoint #%d answered.\nLabel: %s\nAnswer: %s", cp.ID, cp.Label, cp.Answer),
					"meta": map[string]string{
						"checkpoint_id": fmt.Sprintf("%d", cp.ID),
						"event":         "checkpoint_answered",
					},
				},
			}
			if err := out.encode(notif); err != nil {
				slog.Error("channel: push notification", "err", err)
				return
			}
			slog.Info("channel: pushed checkpoint answer", "id", cp.ID, "label", cp.Label)

			if cp.TsUpdated > watermark {
				watermark = cp.TsUpdated
			}
		}
	}
}

// ── Router ───────────────────────────────────────────────────────────────────

func handleMCPRequest(srv *Server, req *jsonrpcRequest) *jsonrpcResponse {
	switch req.Method {
	case "initialize":
		caps := map[string]any{"tools": map[string]any{}}
		result := map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":   caps,
			"serverInfo": map[string]any{
				"name":    "context-vault",
				"version": "1.2.0",
			},
		}
		if srv.channelEnabled {
			caps["experimental"] = map[string]any{"claude/channel": map[string]any{}}
			result["instructions"] = "Events from the context-vault channel arrive as <channel source=\"context-vault\" event=\"checkpoint_answered\" checkpoint_id=\"N\">. " +
				"They indicate that a blocking checkpoint you created has been answered by the supervisor. " +
				"Read the answer in the event body and resume your work accordingly. " +
				"If the answer says 'approuver' or equivalent, continue with the approved approach. " +
				"If the answer says 'refuser' or gives a different directive, follow that directive."
		}
		return &jsonrpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  result,
		}

	case "notifications/initialized", "notifications/cancelled":
		return nil // no response for notifications

	case "tools/list":
		return &jsonrpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  map[string]any{"tools": toolDefinitions()},
		}

	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return &jsonrpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32602, Message: "invalid params"}}
		}
		result := dispatchTool(srv, params.Name, params.Arguments)
		return &jsonrpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result}

	default:
		return &jsonrpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32601, Message: "method not found: " + req.Method}}
	}
}

// ── Tool definitions ─────────────────────────────────────────────────────────

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

// ── Tool dispatch ────────────────────────────────────────────────────────────

// toMCP converts a vault.Result to an mcpToolResult.
func toMCP(r vault.Result) mcpToolResult {
	res := mcpToolResult{IsError: r.IsError}
	for _, c := range r.Content {
		res.Content = append(res.Content, mcpContent{Type: c.Type, Text: c.Text})
	}
	return res
}

func dispatchTool(srv *Server, name string, args json.RawMessage) mcpToolResult {
	switch name {
	case "vault_get_context":
		return srv.mcpGetContext(args)
	case "vault_search_entities":
		return srv.mcpSearchEntities(args)
	case "vault_upsert_entity":
		return srv.mcpUpsertEntity(args)
	case "vault_todo_transition":
		return srv.mcpTodoTransition(args)
	case "vault_list_todos":
		return srv.mcpListTodos(args)
	case "vault_create_relation":
		return srv.mcpCreateRelation(args)
	case "vault_delete_entity":
		return srv.mcpDeleteEntity(args)
	case "vault_add_steps":
		return srv.mcpAddSteps(args)
	case "vault_step_done":
		return srv.mcpStepDone(args)
	case "vault_get_entity":
		return srv.mcpGetEntity(args)
	default:
		return mcpToolResult{Content: []mcpContent{{Type: "text", Text: "unknown tool: " + name}}, IsError: true}
	}
}

// ── Tool handlers — thin wrappers delegating to internal/vault ───────────────

func (s *Server) mcpGetContext(args json.RawMessage) mcpToolResult {
	var p struct {
		Namespace string `json:"namespace"`
		Session   string `json:"session"`
	}
	json.Unmarshal(args, &p)
	return toMCP(s.vault.GetContext(p.Namespace, p.Session))
}

func (s *Server) mcpSearchEntities(args json.RawMessage) mcpToolResult {
	var p struct {
		Type      string `json:"type"`
		Namespace string `json:"namespace"`
		Query     string `json:"query"`
		Session   string `json:"session"`
		Limit     int    `json:"limit"`
	}
	json.Unmarshal(args, &p)
	return toMCP(s.vault.SearchEntities(p.Type, p.Namespace, p.Query, p.Session, p.Limit))
}

func (s *Server) mcpUpsertEntity(args json.RawMessage) mcpToolResult {
	var p struct {
		ID          *int64          `json:"id"`
		Namespace   string          `json:"namespace"`
		Type        string          `json:"type"`
		Label       string          `json:"label"`
		Sensitivity int             `json:"sensitivity"`
		Meta        json.RawMessage `json:"meta"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return mcpToolResult{Content: []mcpContent{{Type: "text", Text: "invalid params: " + err.Error()}}, IsError: true}
	}
	return toMCP(s.vault.UpsertEntity(p.ID, p.Namespace, p.Type, p.Label, p.Sensitivity, p.Meta))
}

func (s *Server) mcpTodoTransition(args json.RawMessage) mcpToolResult {
	var p struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return mcpToolResult{Content: []mcpContent{{Type: "text", Text: "invalid params: " + err.Error()}}, IsError: true}
	}
	return toMCP(s.vault.TodoTransition(p.ID, p.Status))
}

func (s *Server) mcpListTodos(args json.RawMessage) mcpToolResult {
	var p struct {
		Namespace   string `json:"namespace"`
		Session     string `json:"session"`
		IncludeDone bool   `json:"include_done"`
	}
	json.Unmarshal(args, &p)
	return toMCP(s.vault.ListTodos(p.Namespace, p.Session, p.IncludeDone))
}

func (s *Server) mcpCreateRelation(args json.RawMessage) mcpToolResult {
	var p struct {
		FromID int64  `json:"from_id"`
		ToID   int64  `json:"to_id"`
		Type   string `json:"type"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return mcpToolResult{Content: []mcpContent{{Type: "text", Text: "invalid params: " + err.Error()}}, IsError: true}
	}
	return toMCP(s.vault.CreateRelation(p.FromID, p.ToID, p.Type))
}

func (s *Server) mcpDeleteEntity(args json.RawMessage) mcpToolResult {
	var p struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return mcpToolResult{Content: []mcpContent{{Type: "text", Text: "invalid params: " + err.Error()}}, IsError: true}
	}
	return toMCP(s.vault.DeleteEntity(p.ID))
}

func (s *Server) mcpAddSteps(args json.RawMessage) mcpToolResult {
	var p struct {
		TodoID int64    `json:"todo_id"`
		Steps  []string `json:"steps"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return mcpToolResult{Content: []mcpContent{{Type: "text", Text: "invalid params: " + err.Error()}}, IsError: true}
	}
	return toMCP(s.vault.AddSteps(p.TodoID, p.Steps))
}

func (s *Server) mcpStepDone(args json.RawMessage) mcpToolResult {
	var p struct {
		TodoID int64  `json:"todo_id"`
		Step   string `json:"step"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return mcpToolResult{Content: []mcpContent{{Type: "text", Text: "invalid params: " + err.Error()}}, IsError: true}
	}
	return toMCP(s.vault.StepDone(p.TodoID, p.Step))
}

func (s *Server) mcpGetEntity(args json.RawMessage) mcpToolResult {
	var p struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return mcpToolResult{Content: []mcpContent{{Type: "text", Text: "invalid params: " + err.Error()}}, IsError: true}
	}
	return toMCP(s.vault.GetEntity(p.ID))
}
