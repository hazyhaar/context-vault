// CLAUDE:SUMMARY MCP stdio server — JSON-RPC 2.0 over stdin/stdout, 10 vault tools for entity CRUD.
// CLAUDE:DEPENDS main.go (same package: Server, NewServer, context.Background SQL pattern)
// CLAUDE:EXPORTS runMCP (package main — called by main() when os.Args[1]=="mcp")
package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
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
// received an answer (meta->>'answer' non-NULL). When found, it pushes a
// notifications/claude/channel event on stdout so Claude can resume work.
func channelPollCheckpoints(srv *Server, out *stdoutWriter) {
	// Watermark: only notify checkpoints updated after this timestamp.
	// Start from now — we don't replay old answers.
	watermark := time.Now().Unix()

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	const query = `SELECT id, label, json_extract(meta, '$.answer'), ts_updated
		FROM entities
		WHERE type = 'checkpoint'
		  AND json_extract(meta, '$.blocking') = 1
		  AND json_extract(meta, '$.answer') IS NOT NULL
		  AND ts_updated > ?
		ORDER BY ts_updated ASC`

	for range ticker.C {
		rows, err := srv.db.QueryContext(context.Background(), query, watermark)
		if err != nil {
			slog.Warn("channel: poll checkpoints", "err", err)
			continue
		}

		for rows.Next() {
			var id int64
			var label, answer string
			var tsUpdated int64
			if err := rows.Scan(&id, &label, &answer, &tsUpdated); err != nil {
				slog.Warn("channel: scan checkpoint", "err", err)
				continue
			}

			// Push channel notification
			notif := jsonrpcNotification{
				JSONRPC: "2.0",
				Method:  "notifications/claude/channel",
				Params: map[string]any{
					"content": fmt.Sprintf("Checkpoint #%d answered.\nLabel: %s\nAnswer: %s", id, label, answer),
					"meta": map[string]string{
						"checkpoint_id": fmt.Sprintf("%d", id),
						"event":         "checkpoint_answered",
					},
				},
			}
			if err := out.encode(notif); err != nil {
				slog.Error("channel: push notification", "err", err)
				rows.Close()
				return
			}
			slog.Info("channel: pushed checkpoint answer", "id", id, "label", label)

			// Advance watermark
			if tsUpdated > watermark {
				watermark = tsUpdated
			}
		}
		rows.Close()
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

// ── Tool handlers ────────────────────────────────────────────────────────────

func (s *Server) mcpGetContext(args json.RawMessage) mcpToolResult {
	var p struct {
		Namespace string `json:"namespace"`
		Session   string `json:"session"`
	}
	json.Unmarshal(args, &p)

	query := `SELECT type, label, meta->>'$.priority', meta->>'$.blob_plus',
		  meta->>'$.question', meta->>'$.blocking',
		  datetime(ts_created, 'unixepoch'), datetime(ts_updated, 'unixepoch')
		FROM entities
		WHERE type IN ('todo','decision','constraint','checkpoint')
		  AND (type != 'todo' OR meta->>'$.status' != 'done')
		  AND (type != 'checkpoint' OR meta->>'$.answer' IS NULL)
		  AND sensitivity < 2`
	qargs := []any{}

	if p.Namespace != "" {
		query += ` AND namespace = ?`
		qargs = append(qargs, p.Namespace)
	}
	if p.Session != "" {
		query += ` AND session_origin = ?`
		qargs = append(qargs, p.Session)
	}

	query += ` ORDER BY
		CASE type WHEN 'checkpoint' THEN -1 WHEN 'todo' THEN 0 WHEN 'decision' THEN 1 WHEN 'constraint' THEN 2 END,
		CASE meta->>'$.priority' WHEN 'high' THEN 0 WHEN 'normal' THEN 1 ELSE 2 END,
		ts_updated DESC
		LIMIT 25`

	rows, err := s.db.QueryContext(context.Background(), query, qargs...)
	if err != nil {
		return mcpToolResult{Content: []mcpContent{{Type: "text", Text: "query error: " + err.Error()}}, IsError: true}
	}
	defer rows.Close()

	var checkpoints, todos, decisions, constraints []string
	for rows.Next() {
		var typ, label, priority, blob, question, blocking, created, updated sql.NullString
		if rows.Scan(&typ, &label, &priority, &blob, &question, &blocking, &created, &updated) != nil {
			continue
		}
		line := label.String
		if priority.Valid && priority.String != "" {
			line = "[" + priority.String + "] " + line
		}
		if blob.Valid && blob.String != "" {
			line += " — " + blob.String
		}
		switch typ.String {
		case "checkpoint":
			cp := line
			if question.Valid && question.String != "" {
				cp += "\n    Question : " + question.String
			}
			if blocking.Valid && (blocking.String == "true" || blocking.String == "1") {
				cp += "\n    Blocking : oui"
			}
			if created.Valid {
				cp += "\n    Créé : " + created.String
			}
			checkpoints = append(checkpoints, cp)
		case "todo":
			todos = append(todos, line)
		case "decision":
			decisions = append(decisions, line)
		case "constraint":
			constraints = append(constraints, line)
		}
	}

	var sb strings.Builder
	if len(checkpoints) > 0 {
		sb.WriteString("## CHECKPOINTS EN ATTENTE\n")
		for _, c := range checkpoints {
			sb.WriteString("- ⚠ " + c + "\n")
		}
	}
	if len(todos) > 0 {
		sb.WriteString("## Todos actifs\n")
		for _, t := range todos {
			sb.WriteString("- " + t + "\n")
		}
	}
	if len(decisions) > 0 {
		sb.WriteString("\n## Decisions\n")
		for _, d := range decisions {
			sb.WriteString("- " + d + "\n")
		}
	}
	if len(constraints) > 0 {
		sb.WriteString("\n## Contraintes\n")
		for _, c := range constraints {
			sb.WriteString("- " + c + "\n")
		}
	}
	if sb.Len() == 0 {
		sb.WriteString("Aucune entite active dans le vault.")
	}
	return mcpToolResult{Content: []mcpContent{{Type: "text", Text: sb.String()}}}
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

	if p.Limit <= 0 || p.Limit > 50 {
		p.Limit = 20
	}

	query := `SELECT id, namespace, type, label, sensitivity,
		meta->>'$.status' AS status,
		meta->>'$.priority' AS priority,
		meta->>'$.blob_plus' AS blob_plus,
		meta->>'$.target_file' AS target_file,
		datetime(ts_created, 'unixepoch') AS created,
		datetime(ts_updated, 'unixepoch') AS updated,
		meta->>'$.question' AS question,
		meta->>'$.options' AS options,
		meta->>'$.blocking' AS blocking,
		meta->>'$.answer' AS answer
		FROM entities WHERE sensitivity < 2`
	qargs := []any{}

	if p.Type != "" {
		query += ` AND type = ?`
		qargs = append(qargs, p.Type)
	}
	if p.Namespace != "" {
		query += ` AND namespace = ?`
		qargs = append(qargs, p.Namespace)
	}
	if p.Query != "" {
		query += ` AND label LIKE '%' || ? || '%'`
		qargs = append(qargs, p.Query)
	}
	if p.Session != "" {
		query += ` AND session_origin = ?`
		qargs = append(qargs, p.Session)
	}
	query += ` ORDER BY ts_updated DESC LIMIT ?`
	qargs = append(qargs, p.Limit)

	rows, err := s.db.QueryContext(context.Background(), query, qargs...)
	if err != nil {
		return mcpToolResult{Content: []mcpContent{{Type: "text", Text: "query error: " + err.Error()}}, IsError: true}
	}
	defer rows.Close()

	var results []map[string]any
	for rows.Next() {
		var id int64
		var ns, typ, label, status, priority, blobPlus, targetFile, created, updated sql.NullString
		var question, options, blocking, answer sql.NullString
		var sensitivity int
		if rows.Scan(&id, &ns, &typ, &label, &sensitivity, &status, &priority, &blobPlus, &targetFile, &created, &updated, &question, &options, &blocking, &answer) != nil {
			continue
		}
		row := map[string]any{"id": id}
		if ns.Valid {
			row["namespace"] = ns.String
		}
		if typ.Valid {
			row["type"] = typ.String
		}
		if label.Valid {
			row["label"] = label.String
		}
		row["sensitivity"] = sensitivity
		if status.Valid && status.String != "" {
			row["status"] = status.String
		}
		if priority.Valid && priority.String != "" {
			row["priority"] = priority.String
		}
		if blobPlus.Valid && blobPlus.String != "" {
			row["blob_plus"] = blobPlus.String
		}
		if targetFile.Valid && targetFile.String != "" {
			row["target_file"] = targetFile.String
		}
		if created.Valid {
			row["created"] = created.String
		}
		if updated.Valid {
			row["updated"] = updated.String
		}
		if question.Valid && question.String != "" {
			row["question"] = question.String
		}
		if options.Valid && options.String != "" {
			row["options"] = options.String
		}
		if blocking.Valid && blocking.String != "" {
			row["blocking"] = blocking.String
		}
		if answer.Valid && answer.String != "" {
			row["answer"] = answer.String
		}
		results = append(results, row)
	}

	if len(results) == 0 {
		return mcpToolResult{Content: []mcpContent{{Type: "text", Text: "Aucun resultat."}}}
	}
	out, _ := json.MarshalIndent(results, "", "  ")
	return mcpToolResult{Content: []mcpContent{{Type: "text", Text: string(out)}}}
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

	if p.Label == "" {
		return mcpToolResult{Content: []mcpContent{{Type: "text", Text: "label is required"}}, IsError: true}
	}

	// Security: reject credential values
	if p.Type == "credential" && p.Meta != nil {
		var metaMap map[string]any
		if json.Unmarshal(p.Meta, &metaMap) == nil {
			if _, hasValue := metaMap["value"]; hasValue {
				return mcpToolResult{Content: []mcpContent{{Type: "text", Text: "credential entities must not contain a 'value' key in meta"}}, IsError: true}
			}
		}
	}

	metaBlob := []byte("{}")
	if p.Meta != nil {
		metaBlob = p.Meta
	}

	ctx := context.Background()

	if p.ID != nil {
		// Update — merge meta via json_patch (RFC 7396): existing keys preserved,
		// patch keys merged, null values delete the key.
		// Also update namespace, type, sensitivity if provided.
		query := `UPDATE entities SET label = ?, meta = jsonb(json_patch(json(meta), ?)),`
		qargs := []any{p.Label, string(metaBlob)}
		if p.Namespace != "" {
			query += ` namespace = ?,`
			qargs = append(qargs, p.Namespace)
		}
		if p.Type != "" {
			query += ` type = ?,`
			qargs = append(qargs, p.Type)
		}
		if p.Sensitivity > 0 {
			query += ` sensitivity = ?,`
			qargs = append(qargs, p.Sensitivity)
		}
		query += ` ts_updated = unixepoch() WHERE id = ?`
		qargs = append(qargs, *p.ID)
		res, err := s.db.ExecContext(ctx, query, qargs...)
		if err != nil {
			return mcpToolResult{Content: []mcpContent{{Type: "text", Text: "update error: " + err.Error()}}, IsError: true}
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return mcpToolResult{Content: []mcpContent{{Type: "text", Text: fmt.Sprintf("entity %d not found", *p.ID)}}, IsError: true}
		}
		return mcpToolResult{Content: []mcpContent{{Type: "text", Text: fmt.Sprintf("updated entity %d", *p.ID)}}}
	}

	// Create
	if p.Namespace == "" || p.Type == "" {
		return mcpToolResult{Content: []mcpContent{{Type: "text", Text: "namespace and type are required for create"}}, IsError: true}
	}

	// Auto-inject created_at timestamp for todos and missions
	if p.Type == "todo" || p.Type == "mission" {
		var metaMap map[string]any
		if json.Unmarshal(metaBlob, &metaMap) == nil {
			if _, exists := metaMap["created_at"]; !exists {
				metaMap["created_at"] = time.Now().UTC().Format(time.RFC3339)
				metaBlob, _ = json.Marshal(metaMap)
			}
		}
	}

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO entities (namespace, type, label, sensitivity, ts_created, ts_updated, session_origin, meta)
		 VALUES (?, ?, ?, ?, unixepoch(), unixepoch(), ?, jsonb(?))`,
		p.Namespace, p.Type, p.Label, p.Sensitivity, s.currentSession(), string(metaBlob))
	if err != nil {
		return mcpToolResult{Content: []mcpContent{{Type: "text", Text: "insert error: " + err.Error()}}, IsError: true}
	}
	id, _ := res.LastInsertId()

	// Mission: auto-create subtask_of relations for each todo in meta.todos
	if p.Type == "mission" {
		var metaMap map[string]any
		if json.Unmarshal(metaBlob, &metaMap) == nil {
			if todosRaw, ok := metaMap["todos"]; ok {
				if todosSlice, ok := todosRaw.([]any); ok {
					for _, v := range todosSlice {
						var todoID int64
						switch n := v.(type) {
						case float64:
							todoID = int64(n)
						case json.Number:
							if i, err := n.Int64(); err == nil {
								todoID = i
							}
						}
						if todoID > 0 {
							s.db.ExecContext(ctx,
								`INSERT INTO relations (from_id, to_id, type, ts_created) VALUES (?, ?, 'subtask_of', unixepoch())`,
								todoID, id)
						}
					}
				}
			}
		}
	}

	return mcpToolResult{Content: []mcpContent{{Type: "text", Text: fmt.Sprintf("created entity %d", id)}}}
}

func (s *Server) mcpTodoTransition(args json.RawMessage) mcpToolResult {
	var p struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return mcpToolResult{Content: []mcpContent{{Type: "text", Text: "invalid params: " + err.Error()}}, IsError: true}
	}

	validStatuses := map[string]bool{"open": true, "in_progress": true, "blocked": true, "done": true}
	if !validStatuses[p.Status] {
		return mcpToolResult{Content: []mcpContent{{Type: "text", Text: "invalid status: " + p.Status}}, IsError: true}
	}

	ctx := context.Background()

	// Determine entity type (todo or mission)
	var entityType string
	err := s.db.QueryRowContext(ctx, `SELECT type FROM entities WHERE id = ? AND type IN ('todo','mission')`, p.ID).Scan(&entityType)
	if err != nil {
		return mcpToolResult{Content: []mcpContent{{Type: "text", Text: fmt.Sprintf("todo %d not found (or not a todo/mission)", p.ID)}}, IsError: true}
	}

	// Lock check: if this todo has meta.locked_by, verify session matches the mission's session
	if entityType == "todo" && p.Status == "in_progress" {
		var metaRaw string
		s.db.QueryRowContext(ctx, `SELECT json(meta) FROM entities WHERE id = ?`, p.ID).Scan(&metaRaw)
		var metaMap map[string]any
		if json.Unmarshal([]byte(metaRaw), &metaMap) == nil {
			if lockedByRaw, ok := metaMap["locked_by"]; ok {
				var missionID int64
				switch n := lockedByRaw.(type) {
				case float64:
					missionID = int64(n)
				case json.Number:
					if i, err := n.Int64(); err == nil {
						missionID = i
					}
				}
				if missionID > 0 {
					// Read mission's assigned_session
					var missionMeta string
					s.db.QueryRowContext(ctx, `SELECT json(meta) FROM entities WHERE id = ?`, missionID).Scan(&missionMeta)
					var mm map[string]any
					if json.Unmarshal([]byte(missionMeta), &mm) == nil {
						if assignedSession, ok := mm["assigned_session"].(string); ok && assignedSession != "" {
							currentSess := s.currentSession()
							if currentSess != assignedSession {
								return mcpToolResult{Content: []mcpContent{{Type: "text", Text: fmt.Sprintf("todo #%d verrouillée par mission #%d (session %s)", p.ID, missionID, assignedSession)}}, IsError: true}
							}
						}
					}
				}
			}
		}
	}

	// Set status + horodatage de la transition dans meta JSONB.
	now := `"` + time.Now().UTC().Format(time.RFC3339) + `"`
	statusVal := `"` + p.Status + `"`

	var query string
	switch p.Status {
	case "in_progress":
		query = `UPDATE entities SET meta = jsonb_set(jsonb_set(meta, '$.status', jsonb(?)), '$.started_at', jsonb(?)), ts_updated = unixepoch() WHERE id = ?`
	case "done":
		query = `UPDATE entities SET meta = jsonb_set(jsonb_set(meta, '$.status', jsonb(?)), '$.completed_at', jsonb(?)), ts_updated = unixepoch() WHERE id = ?`
	default:
		query = `UPDATE entities SET meta = jsonb_set(meta, '$.status', jsonb(?)), ts_updated = unixepoch() WHERE id = ?`
	}

	var res sql.Result
	if p.Status == "in_progress" || p.Status == "done" {
		res, err = s.db.ExecContext(ctx, query, statusVal, now, p.ID)
	} else {
		res, err = s.db.ExecContext(ctx, query, statusVal, p.ID)
	}
	if err != nil {
		return mcpToolResult{Content: []mcpContent{{Type: "text", Text: "update error: " + err.Error()}}, IsError: true}
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return mcpToolResult{Content: []mcpContent{{Type: "text", Text: fmt.Sprintf("todo %d not found (or not a todo/mission)", p.ID)}}, IsError: true}
	}

	// Mission cascading: lock/unlock child todos
	if entityType == "mission" {
		childIDs := s.missionChildIDs(ctx, p.ID)
		switch p.Status {
		case "in_progress":
			// Record assigned_session + lock children
			sess := s.currentSession()
			s.db.ExecContext(ctx,
				`UPDATE entities SET meta = jsonb_set(meta, '$.assigned_session', jsonb(?)) WHERE id = ?`,
				`"`+sess+`"`, p.ID)
			for _, cid := range childIDs {
				s.db.ExecContext(ctx,
					`UPDATE entities SET meta = jsonb_set(meta, '$.locked_by', jsonb(?)) WHERE id = ?`,
					fmt.Sprintf("%d", p.ID), cid)
			}
		case "done":
			// Unlock children: remove locked_by
			for _, cid := range childIDs {
				s.db.ExecContext(ctx,
					`UPDATE entities SET meta = jsonb_remove(meta, '$.locked_by') WHERE id = ?`, cid)
			}
			// Clear assigned_session
			s.db.ExecContext(ctx,
				`UPDATE entities SET meta = jsonb_remove(meta, '$.assigned_session') WHERE id = ?`, p.ID)
		}
	}

	return mcpToolResult{Content: []mcpContent{{Type: "text", Text: fmt.Sprintf("todo %d -> %s", p.ID, p.Status)}}}
}

// missionChildIDs returns the IDs of todos linked to a mission via subtask_of.
func (s *Server) missionChildIDs(ctx context.Context, missionID int64) []int64 {
	rows, err := s.db.QueryContext(ctx,
		`SELECT from_id FROM relations WHERE to_id = ? AND type = 'subtask_of'`, missionID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

func (s *Server) mcpListTodos(args json.RawMessage) mcpToolResult {
	var p struct {
		Namespace   string `json:"namespace"`
		Session     string `json:"session"`
		IncludeDone bool   `json:"include_done"`
	}
	json.Unmarshal(args, &p)

	query := `SELECT e.id, e.label,
		meta->>'$.status' AS status,
		meta->>'$.priority' AS priority,
		meta->>'$.deadline' AS deadline,
		meta->>'$.blob_plus' AS description,
		GROUP_CONCAT(r.to_id) AS blockers,
		datetime(e.ts_created, 'unixepoch') AS created,
		datetime(e.ts_updated, 'unixepoch') AS updated
		FROM entities e
		LEFT JOIN relations r ON r.from_id = e.id AND r.type = 'depends_on'
		WHERE e.type = 'todo'`
	qargs := []any{}

	if !p.IncludeDone {
		query += ` AND (meta->>'$.status' IS NULL OR meta->>'$.status' != 'done')`
	}
	if p.Namespace != "" {
		query += ` AND e.namespace = ?`
		qargs = append(qargs, p.Namespace)
	}
	if p.Session != "" {
		query += ` AND e.session_origin = ?`
		qargs = append(qargs, p.Session)
	}
	query += ` GROUP BY e.id
		ORDER BY
		  CASE meta->>'$.priority' WHEN 'high' THEN 0 WHEN 'normal' THEN 1 ELSE 2 END,
		  meta->>'$.deadline' ASC NULLS LAST,
		  e.ts_updated DESC`

	rows, err := s.db.QueryContext(context.Background(), query, qargs...)
	if err != nil {
		return mcpToolResult{Content: []mcpContent{{Type: "text", Text: "query error: " + err.Error()}}, IsError: true}
	}
	defer rows.Close()

	var sb strings.Builder
	count := 0
	for rows.Next() {
		var id int64
		var label, status, priority, deadline, description, blockers, created, updated sql.NullString
		if rows.Scan(&id, &label, &status, &priority, &deadline, &description, &blockers, &created, &updated) != nil {
			continue
		}
		count++
		st := "open"
		if status.Valid && status.String != "" {
			st = status.String
		}
		pri := ""
		if priority.Valid && priority.String != "" {
			pri = " [" + priority.String + "]"
		}
		dl := ""
		if deadline.Valid && deadline.String != "" {
			dl = " (deadline: " + deadline.String + ")"
		}
		bl := ""
		if blockers.Valid && blockers.String != "" {
			bl = " blocked by: " + blockers.String
		}
		ts := ""
		if created.Valid {
			ts = " (créé " + created.String
			if updated.Valid && updated.String != created.String {
				ts += ", maj " + updated.String
			}
			ts += ")"
		}
		sb.WriteString(fmt.Sprintf("#%d %s%s — %s%s%s%s\n", id, label.String, pri, st, dl, bl, ts))
		if description.Valid && description.String != "" {
			sb.WriteString("   " + description.String + "\n")
		}
		// Show steps if any
		stepRows, stepErr := s.db.QueryContext(context.Background(),
			`SELECT step, done FROM todo_steps WHERE todo_id = ? ORDER BY rowid`, id)
		if stepErr == nil {
			for stepRows.Next() {
				var step string
				var done int
				if stepRows.Scan(&step, &done) == nil {
					mark := "[ ]"
					if done == 1 {
						mark = "[x]"
					}
					sb.WriteString(fmt.Sprintf("   %s %s\n", mark, step))
				}
			}
			stepRows.Close()
		}
	}

	if count == 0 {
		return mcpToolResult{Content: []mcpContent{{Type: "text", Text: "Aucun todo."}}}
	}
	return mcpToolResult{Content: []mcpContent{{Type: "text", Text: sb.String()}}}
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

	validTypes := map[string]bool{"depends_on": true, "blocks": true, "subtask_of": true}
	if !validTypes[p.Type] {
		return mcpToolResult{Content: []mcpContent{{Type: "text", Text: "invalid relation type: " + p.Type}}, IsError: true}
	}

	_, err := s.db.ExecContext(context.Background(),
		`INSERT INTO relations (from_id, to_id, type, ts_created) VALUES (?, ?, ?, unixepoch())`,
		p.FromID, p.ToID, p.Type)
	if err != nil {
		return mcpToolResult{Content: []mcpContent{{Type: "text", Text: "insert error: " + err.Error()}}, IsError: true}
	}
	return mcpToolResult{Content: []mcpContent{{Type: "text", Text: fmt.Sprintf("relation %s: %d -> %d", p.Type, p.FromID, p.ToID)}}}
}

func (s *Server) mcpDeleteEntity(args json.RawMessage) mcpToolResult {
	var p struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return mcpToolResult{Content: []mcpContent{{Type: "text", Text: "invalid params: " + err.Error()}}, IsError: true}
	}

	res, err := s.db.ExecContext(context.Background(),
		`DELETE FROM entities WHERE id = ?`, p.ID)
	if err != nil {
		return mcpToolResult{Content: []mcpContent{{Type: "text", Text: "delete error: " + err.Error()}}, IsError: true}
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return mcpToolResult{Content: []mcpContent{{Type: "text", Text: fmt.Sprintf("entity %d not found", p.ID)}}, IsError: true}
	}
	return mcpToolResult{Content: []mcpContent{{Type: "text", Text: fmt.Sprintf("deleted entity %d", p.ID)}}}
}

// stepKey extracts the dedup key from a step name: prefix before ':' (trimmed), or the full name.
func stepKey(step string) string {
	if i := strings.IndexByte(step, ':'); i > 0 {
		return strings.TrimSpace(step[:i])
	}
	return step
}

func (s *Server) mcpAddSteps(args json.RawMessage) mcpToolResult {
	var p struct {
		TodoID int64    `json:"todo_id"`
		Steps  []string `json:"steps"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return mcpToolResult{Content: []mcpContent{{Type: "text", Text: "invalid params: " + err.Error()}}, IsError: true}
	}
	if len(p.Steps) == 0 {
		return mcpToolResult{Content: []mcpContent{{Type: "text", Text: "steps array is empty"}}, IsError: true}
	}

	// Verify todo exists
	var dummy int
	err := s.db.QueryRowContext(context.Background(),
		`SELECT 1 FROM entities WHERE id = ? AND type = 'todo'`, p.TodoID).Scan(&dummy)
	if err != nil {
		return mcpToolResult{Content: []mcpContent{{Type: "text", Text: fmt.Sprintf("todo %d not found", p.TodoID)}}, IsError: true}
	}

	ctx := context.Background()
	inserted := 0
	for _, step := range p.Steps {
		step = strings.TrimSpace(step)
		if step == "" {
			continue
		}
		key := stepKey(step)
		res, err := s.db.ExecContext(ctx,
			`INSERT OR IGNORE INTO todo_steps (todo_id, step, step_key) VALUES (?, ?, ?)`, p.TodoID, step, key)
		if err != nil {
			return mcpToolResult{Content: []mcpContent{{Type: "text", Text: "insert error: " + err.Error()}}, IsError: true}
		}
		n, _ := res.RowsAffected()
		inserted += int(n)
	}
	return mcpToolResult{Content: []mcpContent{{Type: "text", Text: fmt.Sprintf("todo %d: %d steps added (%d total provided)", p.TodoID, inserted, len(p.Steps))}}}
}

func (s *Server) mcpStepDone(args json.RawMessage) mcpToolResult {
	var p struct {
		TodoID int64  `json:"todo_id"`
		Step   string `json:"step"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return mcpToolResult{Content: []mcpContent{{Type: "text", Text: "invalid params: " + err.Error()}}, IsError: true}
	}

	ctx := context.Background()

	// Mark step done — match on step_key (short prefix before ':')
	key := stepKey(strings.TrimSpace(p.Step))
	res, err := s.db.ExecContext(ctx,
		`UPDATE todo_steps SET done = 1, done_at = unixepoch() WHERE todo_id = ? AND step_key = ? AND done = 0`,
		p.TodoID, key)
	if err != nil {
		return mcpToolResult{Content: []mcpContent{{Type: "text", Text: "update error: " + err.Error()}}, IsError: true}
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Check if step exists but already done
		var exists int
		s.db.QueryRowContext(ctx,
			`SELECT 1 FROM todo_steps WHERE todo_id = ? AND step_key = ?`, p.TodoID, key).Scan(&exists)
		if exists == 1 {
			return mcpToolResult{Content: []mcpContent{{Type: "text", Text: fmt.Sprintf("step already done: %s", p.Step)}}}
		}
		return mcpToolResult{Content: []mcpContent{{Type: "text", Text: fmt.Sprintf("step not found: %s (todo %d)", p.Step, p.TodoID)}}, IsError: true}
	}

	// Check if all required steps are done → auto-transition todo to done
	var remaining int
	s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM todo_steps WHERE todo_id = ? AND required = 1 AND done = 0`,
		p.TodoID).Scan(&remaining)

	if remaining == 0 {
		// Auto-transition
		now := `"` + time.Now().UTC().Format(time.RFC3339) + `"`
		s.db.ExecContext(ctx,
			`UPDATE entities SET meta = jsonb_set(jsonb_set(meta, '$.status', jsonb('"done"')), '$.completed_at', jsonb(?)), ts_updated = unixepoch() WHERE id = ? AND type = 'todo'`,
			now, p.TodoID)
		return mcpToolResult{Content: []mcpContent{{Type: "text", Text: fmt.Sprintf("step done: %s — todo %d auto-transitioned to done (all required steps complete)", p.Step, p.TodoID)}}}
	}

	return mcpToolResult{Content: []mcpContent{{Type: "text", Text: fmt.Sprintf("step done: %s — %d required steps remaining", p.Step, remaining)}}}
}

func (s *Server) mcpGetEntity(args json.RawMessage) mcpToolResult {
	var p struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return mcpToolResult{Content: []mcpContent{{Type: "text", Text: "invalid params: " + err.Error()}}, IsError: true}
	}

	ctx := context.Background()

	var ns, typ, label, sessionOrigin sql.NullString
	var sensitivity int
	var meta sql.NullString
	var created, updated sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT namespace, type, label, sensitivity, json(meta),
			datetime(ts_created, 'unixepoch'), datetime(ts_updated, 'unixepoch'),
			session_origin
		FROM entities WHERE id = ?`, p.ID).Scan(
		&ns, &typ, &label, &sensitivity, &meta, &created, &updated, &sessionOrigin)
	if err != nil {
		return mcpToolResult{Content: []mcpContent{{Type: "text", Text: fmt.Sprintf("entity %d not found", p.ID)}}, IsError: true}
	}

	result := map[string]any{"id": p.ID}
	if ns.Valid {
		result["namespace"] = ns.String
	}
	if typ.Valid {
		result["type"] = typ.String
	}
	if label.Valid {
		result["label"] = label.String
	}
	result["sensitivity"] = sensitivity
	if meta.Valid {
		var m any
		if json.Unmarshal([]byte(meta.String), &m) == nil {
			result["meta"] = m
		}
	}
	if created.Valid {
		result["created"] = created.String
	}
	if updated.Valid {
		result["updated"] = updated.String
	}
	if sessionOrigin.Valid {
		result["session_origin"] = sessionOrigin.String
	}

	// Relations
	relRows, err := s.db.QueryContext(ctx,
		`SELECT type, from_id, to_id FROM relations WHERE from_id = ? OR to_id = ?`, p.ID, p.ID)
	if err == nil {
		var rels []map[string]any
		for relRows.Next() {
			var rType string
			var fromID, toID int64
			if relRows.Scan(&rType, &fromID, &toID) == nil {
				rels = append(rels, map[string]any{"type": rType, "from_id": fromID, "to_id": toID})
			}
		}
		relRows.Close()
		if len(rels) > 0 {
			result["relations"] = rels
		}
	}

	// Steps
	stepRows, err := s.db.QueryContext(ctx,
		`SELECT step, done, required FROM todo_steps WHERE todo_id = ? ORDER BY rowid`, p.ID)
	if err == nil {
		var steps []map[string]any
		for stepRows.Next() {
			var step string
			var done, required int
			if stepRows.Scan(&step, &done, &required) == nil {
				steps = append(steps, map[string]any{"step": step, "done": done == 1, "required": required == 1})
			}
		}
		stepRows.Close()
		if len(steps) > 0 {
			result["steps"] = steps
		}
	}

	b, _ := json.MarshalIndent(result, "", "  ")
	return mcpToolResult{Content: []mcpContent{{Type: "text", Text: string(b)}}}
}
