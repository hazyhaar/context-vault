// CLAUDE:SUMMARY MCP stdio server — JSON-RPC 2.0 over stdin/stdout, 7 vault tools for entity CRUD.
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

func runMCP(srv *Server) {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	enc := json.NewEncoder(os.Stdout)

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
			if err := enc.Encode(resp); err != nil {
				slog.Error("mcp: encode response", "err", err)
				return
			}
		}
	}
	if err := scanner.Err(); err != nil {
		slog.Error("mcp: scanner", "err", err)
	}
}

// ── Router ───────────────────────────────────────────────────────────────────

func handleMCPRequest(srv *Server, req *jsonrpcRequest) *jsonrpcResponse {
	switch req.Method {
	case "initialize":
		return &jsonrpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":   map[string]any{"tools": map[string]any{}},
				"serverInfo": map[string]any{
					"name":    "context-vault",
					"version": "1.1.0",
				},
			},
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
			Description: "Retourne les todos, decisions et contraintes actives du vault. Equivalent de buildStartContext.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"namespace": map[string]any{"type": "string", "description": "Filtrer par namespace (optionnel)"},
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
					"type":        map[string]any{"type": "string", "description": "Type d'entite (requis pour create)", "enum": []string{"function", "type", "package", "file", "decision", "constraint", "todo", "api", "dependency", "pattern", "credential"}},
					"label":       map[string]any{"type": "string", "description": "Label de l'entite"},
					"sensitivity": map[string]any{"type": "number", "description": "0=public, 1=internal, 2=secret (defaut 0)"},
					"meta":        map[string]any{"type": "object", "description": "Metadonnees (blob_plus, blob_minus, status, priority, target_file, etc.)"},
				},
			},
		},
		{
			Name:        "vault_todo_transition",
			Description: "Change le statut d'un todo existant.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"id", "status"},
				"properties": map[string]any{
					"id":     map[string]any{"type": "number", "description": "ID du todo"},
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
	default:
		return mcpToolResult{Content: []mcpContent{{Type: "text", Text: "unknown tool: " + name}}, IsError: true}
	}
}

// ── Tool handlers ────────────────────────────────────────────────────────────

func (s *Server) mcpGetContext(args json.RawMessage) mcpToolResult {
	var p struct {
		Namespace string `json:"namespace"`
	}
	json.Unmarshal(args, &p)

	query := `SELECT type, label, meta->>'$.priority', meta->>'$.blob_plus'
		FROM entities
		WHERE type IN ('todo','decision','constraint')
		  AND (type != 'todo' OR meta->>'$.status' != 'done')
		  AND sensitivity < 2`
	qargs := []any{}

	if p.Namespace != "" {
		query += ` AND namespace = ?`
		qargs = append(qargs, p.Namespace)
	}

	query += ` ORDER BY
		CASE type WHEN 'todo' THEN 0 WHEN 'decision' THEN 1 WHEN 'constraint' THEN 2 END,
		CASE meta->>'$.priority' WHEN 'high' THEN 0 WHEN 'normal' THEN 1 ELSE 2 END,
		ts_updated DESC
		LIMIT 25`

	rows, err := s.db.QueryContext(context.Background(), query, qargs...)
	if err != nil {
		return mcpToolResult{Content: []mcpContent{{Type: "text", Text: "query error: " + err.Error()}}, IsError: true}
	}
	defer rows.Close()

	var todos, decisions, constraints []string
	for rows.Next() {
		var typ, label, priority, blob string
		if rows.Scan(&typ, &label, &priority, &blob) != nil {
			continue
		}
		line := label
		if priority != "" {
			line = "[" + priority + "] " + line
		}
		if blob != "" {
			line += " — " + blob
		}
		switch typ {
		case "todo":
			todos = append(todos, line)
		case "decision":
			decisions = append(decisions, line)
		case "constraint":
			constraints = append(constraints, line)
		}
	}

	var sb strings.Builder
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
		datetime(ts_updated, 'unixepoch') AS updated
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
		var ns, typ, label, status, priority, blobPlus, targetFile, updated sql.NullString
		var sensitivity int
		if rows.Scan(&id, &ns, &typ, &label, &sensitivity, &status, &priority, &blobPlus, &targetFile, &updated) != nil {
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
		if updated.Valid {
			row["updated"] = updated.String
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
		// Update
		res, err := s.db.ExecContext(ctx,
			`UPDATE entities SET label = ?, meta = jsonb(?), ts_updated = unixepoch() WHERE id = ?`,
			p.Label, string(metaBlob), *p.ID)
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

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO entities (namespace, type, label, sensitivity, ts_created, ts_updated, meta)
		 VALUES (?, ?, ?, ?, unixepoch(), unixepoch(), jsonb(?))`,
		p.Namespace, p.Type, p.Label, p.Sensitivity, string(metaBlob))
	if err != nil {
		return mcpToolResult{Content: []mcpContent{{Type: "text", Text: "insert error: " + err.Error()}}, IsError: true}
	}
	id, _ := res.LastInsertId()
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

	res, err := s.db.ExecContext(context.Background(),
		`UPDATE entities SET meta = jsonb_set(meta, '$.status', jsonb(?)), ts_updated = unixepoch()
		 WHERE id = ? AND type = 'todo'`,
		`"`+p.Status+`"`, p.ID)
	if err != nil {
		return mcpToolResult{Content: []mcpContent{{Type: "text", Text: "update error: " + err.Error()}}, IsError: true}
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return mcpToolResult{Content: []mcpContent{{Type: "text", Text: fmt.Sprintf("todo %d not found (or not a todo)", p.ID)}}, IsError: true}
	}
	return mcpToolResult{Content: []mcpContent{{Type: "text", Text: fmt.Sprintf("todo %d -> %s", p.ID, p.Status)}}}
}

func (s *Server) mcpListTodos(args json.RawMessage) mcpToolResult {
	var p struct {
		Namespace   string `json:"namespace"`
		IncludeDone bool   `json:"include_done"`
	}
	json.Unmarshal(args, &p)

	query := `SELECT e.id, e.label,
		meta->>'$.status' AS status,
		meta->>'$.priority' AS priority,
		meta->>'$.deadline' AS deadline,
		meta->>'$.blob_plus' AS description,
		GROUP_CONCAT(r.to_id) AS blockers
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
		var label, status, priority, deadline, description, blockers sql.NullString
		if rows.Scan(&id, &label, &status, &priority, &deadline, &description, &blockers) != nil {
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
		sb.WriteString(fmt.Sprintf("#%d %s%s — %s%s%s\n", id, label.String, pri, st, dl, bl))
		if description.Valid && description.String != "" {
			sb.WriteString("   " + description.String + "\n")
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
