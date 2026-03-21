// CLAUDE:SUMMARY Vault core — all vault business logic extracted from mcp.go. MCP-independent.
// CLAUDE:DEPENDS schema.go, types.go
// CLAUDE:EXPORTS Vault, New, StepKey
package vault

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Vault encapsulates all vault business logic over a *sql.DB.
// SessionFn returns the current session ID (may change over time).
type Vault struct {
	DB        *sql.DB
	SessionFn func() string
}

// New creates a Vault. sessionFn may be nil (returns "").
func New(db *sql.DB, sessionFn func() string) *Vault {
	if sessionFn == nil {
		sessionFn = func() string { return "" }
	}
	return &Vault{DB: db, SessionFn: sessionFn}
}

// StepKey extracts the dedup key from a step name: prefix before ':' (trimmed), or the full name.
func StepKey(step string) string {
	if i := strings.IndexByte(step, ':'); i > 0 {
		return strings.TrimSpace(step[:i])
	}
	return step
}

// ── GetContext ────────────────────────────────────────────────────────────────

func (v *Vault) GetContext(namespace, session string) Result {
	query := `SELECT type, label, meta->>'$.priority', meta->>'$.blob_plus',
		  meta->>'$.question', meta->>'$.blocking',
		  datetime(ts_created, 'unixepoch'), datetime(ts_updated, 'unixepoch')
		FROM entities
		WHERE type IN ('todo','decision','constraint','checkpoint')
		  AND (type != 'todo' OR meta->>'$.status' != 'done')
		  AND (type != 'checkpoint' OR meta->>'$.answer' IS NULL)
		  AND sensitivity < 2`
	qargs := []any{}

	if namespace != "" {
		query += ` AND namespace = ?`
		qargs = append(qargs, namespace)
	}
	if session != "" {
		query += ` AND session_origin = ?`
		qargs = append(qargs, session)
	}

	query += ` ORDER BY
		CASE type WHEN 'checkpoint' THEN -1 WHEN 'todo' THEN 0 WHEN 'decision' THEN 1 WHEN 'constraint' THEN 2 END,
		CASE meta->>'$.priority' WHEN 'high' THEN 0 WHEN 'normal' THEN 1 ELSE 2 END,
		ts_updated DESC
		LIMIT 25`

	rows, err := v.DB.QueryContext(context.Background(), query, qargs...)
	if err != nil {
		return ErrorResult("query error: " + err.Error())
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
	return TextResult(sb.String())
}

// ── SearchEntities ───────────────────────────────────────────────────────────

func (v *Vault) SearchEntities(typ, namespace, query, session string, limit int) Result {
	if limit <= 0 || limit > 50 {
		limit = 20
	}

	q := `SELECT id, namespace, type, label, sensitivity,
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

	if typ != "" {
		q += ` AND type = ?`
		qargs = append(qargs, typ)
	}
	if namespace != "" {
		q += ` AND namespace = ?`
		qargs = append(qargs, namespace)
	}
	if query != "" {
		q += ` AND label LIKE '%' || ? || '%'`
		qargs = append(qargs, query)
	}
	if session != "" {
		q += ` AND session_origin = ?`
		qargs = append(qargs, session)
	}
	q += ` ORDER BY ts_updated DESC LIMIT ?`
	qargs = append(qargs, limit)

	rows, err := v.DB.QueryContext(context.Background(), q, qargs...)
	if err != nil {
		return ErrorResult("query error: " + err.Error())
	}
	defer rows.Close()

	var results []map[string]any
	for rows.Next() {
		var id int64
		var ns, etyp, lbl, status, priority, blobPlus, targetFile, created, updated sql.NullString
		var question, options, blocking, answer sql.NullString
		var sensitivity int
		if rows.Scan(&id, &ns, &etyp, &lbl, &sensitivity, &status, &priority, &blobPlus, &targetFile, &created, &updated, &question, &options, &blocking, &answer) != nil {
			continue
		}
		row := map[string]any{"id": id}
		if ns.Valid {
			row["namespace"] = ns.String
		}
		if etyp.Valid {
			row["type"] = etyp.String
		}
		if lbl.Valid {
			row["label"] = lbl.String
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
		return TextResult("Aucun resultat.")
	}
	out, _ := json.MarshalIndent(results, "", "  ")
	return TextResult(string(out))
}

// ── UpsertEntity ─────────────────────────────────────────────────────────────

func (v *Vault) UpsertEntity(id *int64, namespace, typ, label string, sensitivity int, meta json.RawMessage) Result {
	if label == "" {
		return ErrorResult("label is required")
	}

	// Security: reject credential values
	if typ == "credential" && meta != nil {
		var metaMap map[string]any
		if json.Unmarshal(meta, &metaMap) == nil {
			if _, hasValue := metaMap["value"]; hasValue {
				return ErrorResult("credential entities must not contain a 'value' key in meta")
			}
		}
	}

	metaBlob := []byte("{}")
	if meta != nil {
		metaBlob = meta
	}

	ctx := context.Background()

	if id != nil {
		// Update — merge meta via json_patch (RFC 7396).
		query := `UPDATE entities SET label = ?, meta = jsonb(json_patch(json(meta), ?)),`
		qargs := []any{label, string(metaBlob)}
		if namespace != "" {
			query += ` namespace = ?,`
			qargs = append(qargs, namespace)
		}
		if typ != "" {
			query += ` type = ?,`
			qargs = append(qargs, typ)
		}
		if sensitivity > 0 {
			query += ` sensitivity = ?,`
			qargs = append(qargs, sensitivity)
		}
		query += ` ts_updated = unixepoch() WHERE id = ?`
		qargs = append(qargs, *id)
		res, err := v.DB.ExecContext(ctx, query, qargs...)
		if err != nil {
			return ErrorResult("update error: " + err.Error())
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return ErrorResult(fmt.Sprintf("entity %d not found", *id))
		}
		return TextResult(fmt.Sprintf("updated entity %d", *id))
	}

	// Create
	if namespace == "" || typ == "" {
		return ErrorResult("namespace and type are required for create")
	}

	// Auto-inject created_at timestamp for todos and missions
	if typ == "todo" || typ == "mission" {
		var metaMap map[string]any
		if json.Unmarshal(metaBlob, &metaMap) == nil {
			if _, exists := metaMap["created_at"]; !exists {
				metaMap["created_at"] = time.Now().UTC().Format(time.RFC3339)
				metaBlob, _ = json.Marshal(metaMap)
			}
		}
	}

	res, err := v.DB.ExecContext(ctx,
		`INSERT INTO entities (namespace, type, label, sensitivity, ts_created, ts_updated, session_origin, meta)
		 VALUES (?, ?, ?, ?, unixepoch(), unixepoch(), ?, jsonb(?))`,
		namespace, typ, label, sensitivity, v.SessionFn(), string(metaBlob))
	if err != nil {
		return ErrorResult("insert error: " + err.Error())
	}
	entityID, _ := res.LastInsertId()

	// Mission: auto-create subtask_of relations for each todo in meta.todos
	if typ == "mission" {
		var metaMap map[string]any
		if json.Unmarshal(metaBlob, &metaMap) == nil {
			if todosRaw, ok := metaMap["todos"]; ok {
				if todosSlice, ok := todosRaw.([]any); ok {
					for _, val := range todosSlice {
						var todoID int64
						switch n := val.(type) {
						case float64:
							todoID = int64(n)
						case json.Number:
							if i, err := n.Int64(); err == nil {
								todoID = i
							}
						}
						if todoID > 0 {
							v.DB.ExecContext(ctx,
								`INSERT INTO relations (from_id, to_id, type, ts_created) VALUES (?, ?, 'subtask_of', unixepoch())`,
								todoID, entityID)
						}
					}
				}
			}
		}
	}

	return TextResult(fmt.Sprintf("created entity %d", entityID))
}

// ── TodoTransition ───────────────────────────────────────────────────────────

func (v *Vault) TodoTransition(entityID int64, status string) Result {
	validStatuses := map[string]bool{"open": true, "in_progress": true, "blocked": true, "done": true}
	if !validStatuses[status] {
		return ErrorResult("invalid status: " + status)
	}

	ctx := context.Background()

	// Determine entity type (todo or mission)
	var entityType string
	err := v.DB.QueryRowContext(ctx, `SELECT type FROM entities WHERE id = ? AND type IN ('todo','mission')`, entityID).Scan(&entityType)
	if err != nil {
		return ErrorResult(fmt.Sprintf("todo %d not found (or not a todo/mission)", entityID))
	}

	// Lock check: if this todo has meta.locked_by, verify session matches the mission's session
	if entityType == "todo" && status == "in_progress" {
		var metaRaw string
		v.DB.QueryRowContext(ctx, `SELECT json(meta) FROM entities WHERE id = ?`, entityID).Scan(&metaRaw)
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
					var missionMeta string
					v.DB.QueryRowContext(ctx, `SELECT json(meta) FROM entities WHERE id = ?`, missionID).Scan(&missionMeta)
					var mm map[string]any
					if json.Unmarshal([]byte(missionMeta), &mm) == nil {
						if assignedSession, ok := mm["assigned_session"].(string); ok && assignedSession != "" {
							currentSess := v.SessionFn()
							if currentSess != assignedSession {
								return ErrorResult(fmt.Sprintf("todo #%d verrouillée par mission #%d (session %s)", entityID, missionID, assignedSession))
							}
						}
					}
				}
			}
		}
	}

	// Set status + horodatage
	now := `"` + time.Now().UTC().Format(time.RFC3339) + `"`
	statusVal := `"` + status + `"`

	var query string
	switch status {
	case "in_progress":
		query = `UPDATE entities SET meta = jsonb_set(jsonb_set(meta, '$.status', jsonb(?)), '$.started_at', jsonb(?)), ts_updated = unixepoch() WHERE id = ?`
	case "done":
		query = `UPDATE entities SET meta = jsonb_set(jsonb_set(meta, '$.status', jsonb(?)), '$.completed_at', jsonb(?)), ts_updated = unixepoch() WHERE id = ?`
	default:
		query = `UPDATE entities SET meta = jsonb_set(meta, '$.status', jsonb(?)), ts_updated = unixepoch() WHERE id = ?`
	}

	var res sql.Result
	if status == "in_progress" || status == "done" {
		res, err = v.DB.ExecContext(ctx, query, statusVal, now, entityID)
	} else {
		res, err = v.DB.ExecContext(ctx, query, statusVal, entityID)
	}
	if err != nil {
		return ErrorResult("update error: " + err.Error())
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrorResult(fmt.Sprintf("todo %d not found (or not a todo/mission)", entityID))
	}

	// Mission cascading: lock/unlock child todos
	if entityType == "mission" {
		childIDs := v.missionChildIDs(ctx, entityID)
		switch status {
		case "in_progress":
			sess := v.SessionFn()
			v.DB.ExecContext(ctx,
				`UPDATE entities SET meta = jsonb_set(meta, '$.assigned_session', jsonb(?)) WHERE id = ?`,
				`"`+sess+`"`, entityID)
			for _, cid := range childIDs {
				v.DB.ExecContext(ctx,
					`UPDATE entities SET meta = jsonb_set(meta, '$.locked_by', jsonb(?)) WHERE id = ?`,
					fmt.Sprintf("%d", entityID), cid)
			}
		case "done":
			for _, cid := range childIDs {
				v.DB.ExecContext(ctx,
					`UPDATE entities SET meta = jsonb_remove(meta, '$.locked_by') WHERE id = ?`, cid)
			}
			v.DB.ExecContext(ctx,
				`UPDATE entities SET meta = jsonb_remove(meta, '$.assigned_session') WHERE id = ?`, entityID)
		}
	}

	return TextResult(fmt.Sprintf("todo %d -> %s", entityID, status))
}

func (v *Vault) missionChildIDs(ctx context.Context, missionID int64) []int64 {
	rows, err := v.DB.QueryContext(ctx,
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

// ── ListTodos ────────────────────────────────────────────────────────────────

func (v *Vault) ListTodos(namespace, session string, includeDone bool) Result {
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

	if !includeDone {
		query += ` AND (meta->>'$.status' IS NULL OR meta->>'$.status' != 'done')`
	}
	if namespace != "" {
		query += ` AND e.namespace = ?`
		qargs = append(qargs, namespace)
	}
	if session != "" {
		query += ` AND e.session_origin = ?`
		qargs = append(qargs, session)
	}
	query += ` GROUP BY e.id
		ORDER BY
		  CASE meta->>'$.priority' WHEN 'high' THEN 0 WHEN 'normal' THEN 1 ELSE 2 END,
		  meta->>'$.deadline' ASC NULLS LAST,
		  e.ts_updated DESC`

	rows, err := v.DB.QueryContext(context.Background(), query, qargs...)
	if err != nil {
		return ErrorResult("query error: " + err.Error())
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
		// Show steps
		stepRows, stepErr := v.DB.QueryContext(context.Background(),
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
		return TextResult("Aucun todo.")
	}
	return TextResult(sb.String())
}

// ── CreateRelation ───────────────────────────────────────────────────────────

func (v *Vault) CreateRelation(fromID, toID int64, relType string) Result {
	validTypes := map[string]bool{"depends_on": true, "blocks": true, "subtask_of": true}
	if !validTypes[relType] {
		return ErrorResult("invalid relation type: " + relType)
	}

	_, err := v.DB.ExecContext(context.Background(),
		`INSERT INTO relations (from_id, to_id, type, ts_created) VALUES (?, ?, ?, unixepoch())`,
		fromID, toID, relType)
	if err != nil {
		return ErrorResult("insert error: " + err.Error())
	}
	return TextResult(fmt.Sprintf("relation %s: %d -> %d", relType, fromID, toID))
}

// ── DeleteEntity ─────────────────────────────────────────────────────────────

func (v *Vault) DeleteEntity(entityID int64) Result {
	res, err := v.DB.ExecContext(context.Background(),
		`DELETE FROM entities WHERE id = ?`, entityID)
	if err != nil {
		return ErrorResult("delete error: " + err.Error())
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrorResult(fmt.Sprintf("entity %d not found", entityID))
	}
	return TextResult(fmt.Sprintf("deleted entity %d", entityID))
}

// ── AddSteps ─────────────────────────────────────────────────────────────────

func (v *Vault) AddSteps(todoID int64, steps []string) Result {
	if len(steps) == 0 {
		return ErrorResult("steps array is empty")
	}

	// Verify todo exists
	var dummy int
	err := v.DB.QueryRowContext(context.Background(),
		`SELECT 1 FROM entities WHERE id = ? AND type = 'todo'`, todoID).Scan(&dummy)
	if err != nil {
		return ErrorResult(fmt.Sprintf("todo %d not found", todoID))
	}

	ctx := context.Background()
	inserted := 0
	for _, step := range steps {
		step = strings.TrimSpace(step)
		if step == "" {
			continue
		}
		key := StepKey(step)
		res, err := v.DB.ExecContext(ctx,
			`INSERT OR IGNORE INTO todo_steps (todo_id, step, step_key) VALUES (?, ?, ?)`, todoID, step, key)
		if err != nil {
			return ErrorResult("insert error: " + err.Error())
		}
		n, _ := res.RowsAffected()
		inserted += int(n)
	}
	return TextResult(fmt.Sprintf("todo %d: %d steps added (%d total provided)", todoID, inserted, len(steps)))
}

// ── StepDone ─────────────────────────────────────────────────────────────────

func (v *Vault) StepDone(todoID int64, step string) Result {
	ctx := context.Background()

	// Match on step_key
	key := StepKey(strings.TrimSpace(step))
	res, err := v.DB.ExecContext(ctx,
		`UPDATE todo_steps SET done = 1, done_at = unixepoch() WHERE todo_id = ? AND step_key = ? AND done = 0`,
		todoID, key)
	if err != nil {
		return ErrorResult("update error: " + err.Error())
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		var exists int
		v.DB.QueryRowContext(ctx,
			`SELECT 1 FROM todo_steps WHERE todo_id = ? AND step_key = ?`, todoID, key).Scan(&exists)
		if exists == 1 {
			return TextResult(fmt.Sprintf("step already done: %s", step))
		}
		return ErrorResult(fmt.Sprintf("step not found: %s (todo %d)", step, todoID))
	}

	// Check if all required steps are done → auto-transition
	var remaining int
	v.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM todo_steps WHERE todo_id = ? AND required = 1 AND done = 0`,
		todoID).Scan(&remaining)

	if remaining == 0 {
		now := `"` + time.Now().UTC().Format(time.RFC3339) + `"`
		v.DB.ExecContext(ctx,
			`UPDATE entities SET meta = jsonb_set(jsonb_set(meta, '$.status', jsonb('"done"')), '$.completed_at', jsonb(?)), ts_updated = unixepoch() WHERE id = ? AND type = 'todo'`,
			now, todoID)
		return TextResult(fmt.Sprintf("step done: %s — todo %d auto-transitioned to done (all required steps complete)", step, todoID))
	}

	return TextResult(fmt.Sprintf("step done: %s — %d required steps remaining", step, remaining))
}

// ── GetEntity ────────────────────────────────────────────────────────────────

func (v *Vault) GetEntity(entityID int64) Result {
	ctx := context.Background()

	var ns, typ, label, sessionOrigin sql.NullString
	var sensitivity int
	var meta sql.NullString
	var created, updated sql.NullString
	err := v.DB.QueryRowContext(ctx,
		`SELECT namespace, type, label, sensitivity, json(meta),
			datetime(ts_created, 'unixepoch'), datetime(ts_updated, 'unixepoch'),
			session_origin
		FROM entities WHERE id = ?`, entityID).Scan(
		&ns, &typ, &label, &sensitivity, &meta, &created, &updated, &sessionOrigin)
	if err != nil {
		return ErrorResult(fmt.Sprintf("entity %d not found", entityID))
	}

	result := map[string]any{"id": entityID}
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
	relRows, err := v.DB.QueryContext(ctx,
		`SELECT type, from_id, to_id FROM relations WHERE from_id = ? OR to_id = ?`, entityID, entityID)
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
	stepRows, err := v.DB.QueryContext(ctx,
		`SELECT step, done, required FROM todo_steps WHERE todo_id = ? ORDER BY rowid`, entityID)
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
	return TextResult(string(b))
}

// ── PollAnsweredCheckpoints ──────────────────────────────────────────────────

// AnsweredCheckpoint represents a checkpoint that received an answer.
type AnsweredCheckpoint struct {
	ID            int64
	Label         string
	Answer        string
	SessionOrigin string
	TsUpdated     int64
}

// NewCheckpoint represents a newly created blocking checkpoint (no answer yet).
type NewCheckpoint struct {
	ID            int64
	Label         string
	Question      string
	SessionOrigin string
	TsCreated     int64
}

// PollAnsweredCheckpoints returns checkpoints updated after watermark that have an answer.
func (v *Vault) PollAnsweredCheckpoints(watermark int64) ([]AnsweredCheckpoint, error) {
	const query = `SELECT id, label, json_extract(meta, '$.answer'), COALESCE(session_origin, ''), ts_updated
		FROM entities
		WHERE type = 'checkpoint'
		  AND json_extract(meta, '$.blocking') = 1
		  AND json_extract(meta, '$.answer') IS NOT NULL
		  AND ts_updated > ?
		ORDER BY ts_updated ASC`

	rows, err := v.DB.QueryContext(context.Background(), query, watermark)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []AnsweredCheckpoint
	for rows.Next() {
		var cp AnsweredCheckpoint
		if rows.Scan(&cp.ID, &cp.Label, &cp.Answer, &cp.SessionOrigin, &cp.TsUpdated) == nil {
			results = append(results, cp)
		}
	}
	return results, rows.Err()
}

// PollNewCheckpoints returns blocking checkpoints created after watermark that have no answer yet.
func (v *Vault) PollNewCheckpoints(watermark int64) ([]NewCheckpoint, error) {
	const query = `SELECT id, label, COALESCE(json_extract(meta, '$.question'), ''), COALESCE(session_origin, ''), ts_created
		FROM entities
		WHERE type = 'checkpoint'
		  AND json_extract(meta, '$.blocking') = 1
		  AND json_extract(meta, '$.answer') IS NULL
		  AND ts_created > ?
		ORDER BY ts_created ASC`

	rows, err := v.DB.QueryContext(context.Background(), query, watermark)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []NewCheckpoint
	for rows.Next() {
		var cp NewCheckpoint
		if rows.Scan(&cp.ID, &cp.Label, &cp.Question, &cp.SessionOrigin, &cp.TsCreated) == nil {
			results = append(results, cp)
		}
	}
	return results, rows.Err()
}
