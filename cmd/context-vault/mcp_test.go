package main

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// testServer creates a Server with an in-memory SQLite DB and the full schema applied.
func testServer(t *testing.T) *Server {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &Server{db: db, rings: make(map[string]*RingDumper)}
}

// call is a shorthand to call an MCP handler with JSON args and return the text result.
func call(t *testing.T, fn func(json.RawMessage) mcpToolResult, args string) mcpToolResult {
	t.Helper()
	return fn(json.RawMessage(args))
}

func mustNotError(t *testing.T, r mcpToolResult) string {
	t.Helper()
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.Content[0].Text)
	}
	return r.Content[0].Text
}

// ── Test 1-2: json_patch merge ────────────────────────────────────────────────

func TestUpsertEntity_MergeMeta(t *testing.T) {
	s := testServer(t)

	// Create entity with meta {a:1, b:2}
	r := call(t, s.mcpUpsertEntity, `{"namespace":"test","type":"decision","label":"merge test","meta":{"a":1,"b":2}}`)
	text := mustNotError(t, r)
	if !strings.Contains(text, "created entity") {
		t.Fatalf("expected created, got: %s", text)
	}

	// Update with meta {b:3, c:4} — should merge, not replace
	r = call(t, s.mcpUpsertEntity, `{"id":1,"label":"merge test","meta":{"b":3,"c":4}}`)
	mustNotError(t, r)

	// Read back via get_entity
	r = call(t, s.mcpGetEntity, `{"id":1}`)
	text = mustNotError(t, r)

	var result map[string]any
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		t.Fatal(err)
	}
	meta := result["meta"].(map[string]any)
	if meta["a"] != float64(1) {
		t.Errorf("expected a=1, got %v", meta["a"])
	}
	if meta["b"] != float64(3) {
		t.Errorf("expected b=3 (overwritten), got %v", meta["b"])
	}
	if meta["c"] != float64(4) {
		t.Errorf("expected c=4 (added), got %v", meta["c"])
	}
}

func TestUpsertEntity_MergeMetaNullDeletesKey(t *testing.T) {
	s := testServer(t)

	// Create with meta {x:1, y:2}
	call(t, s.mcpUpsertEntity, `{"namespace":"test","type":"decision","label":"null test","meta":{"x":1,"y":2}}`)

	// Update with meta {x:null} — json_patch RFC 7396: null deletes the key
	r := call(t, s.mcpUpsertEntity, `{"id":1,"label":"null test","meta":{"x":null}}`)
	mustNotError(t, r)

	r = call(t, s.mcpGetEntity, `{"id":1}`)
	text := mustNotError(t, r)

	var result map[string]any
	json.Unmarshal([]byte(text), &result)
	meta := result["meta"].(map[string]any)
	if _, exists := meta["x"]; exists {
		t.Errorf("expected x to be deleted by null patch, but it exists: %v", meta["x"])
	}
	if meta["y"] != float64(2) {
		t.Errorf("expected y=2 preserved, got %v", meta["y"])
	}
}

// ── Test 3-5: checkpoint visibility in get_context ────────────────────────────

func TestGetContext_CheckpointVisible(t *testing.T) {
	s := testServer(t)

	// Create a checkpoint WITHOUT answer
	call(t, s.mcpUpsertEntity, `{"namespace":"test","type":"checkpoint","label":"CP pending","meta":{"question":"is this ok?","blocking":true}}`)

	r := call(t, s.mcpGetContext, `{"namespace":"test"}`)
	text := mustNotError(t, r)

	if !strings.Contains(text, "CHECKPOINTS EN ATTENTE") {
		t.Errorf("expected CHECKPOINTS EN ATTENTE section, got:\n%s", text)
	}
	if !strings.Contains(text, "CP pending") {
		t.Errorf("expected checkpoint label, got:\n%s", text)
	}
	if !strings.Contains(text, "Question : is this ok?") {
		t.Errorf("expected question field, got:\n%s", text)
	}
}

func TestGetContext_AnsweredCheckpointHidden(t *testing.T) {
	s := testServer(t)

	// Create a checkpoint WITH answer
	call(t, s.mcpUpsertEntity, `{"namespace":"test","type":"checkpoint","label":"CP answered","meta":{"question":"ok?","answer":"yes"}}`)

	r := call(t, s.mcpGetContext, `{"namespace":"test"}`)
	text := mustNotError(t, r)

	if strings.Contains(text, "CP answered") {
		t.Errorf("answered checkpoint should be hidden, got:\n%s", text)
	}
}

func TestGetContext_CheckpointsBeforeTodos(t *testing.T) {
	s := testServer(t)

	// Create a todo first (lower ID)
	call(t, s.mcpUpsertEntity, `{"namespace":"test","type":"todo","label":"some todo","meta":{"status":"open","priority":"high"}}`)
	// Then a checkpoint
	call(t, s.mcpUpsertEntity, `{"namespace":"test","type":"checkpoint","label":"urgent CP","meta":{"question":"help?","blocking":true}}`)

	r := call(t, s.mcpGetContext, `{"namespace":"test"}`)
	text := mustNotError(t, r)

	cpIdx := strings.Index(text, "CHECKPOINTS EN ATTENTE")
	todoIdx := strings.Index(text, "Todos actifs")
	if cpIdx < 0 || todoIdx < 0 {
		t.Fatalf("expected both sections, got:\n%s", text)
	}
	if cpIdx > todoIdx {
		t.Errorf("checkpoints should appear BEFORE todos")
	}
}

// ── Test 6: search returns checkpoint fields ──────────────────────────────────

func TestSearchEntities_CheckpointFields(t *testing.T) {
	s := testServer(t)

	call(t, s.mcpUpsertEntity, `{"namespace":"test","type":"checkpoint","label":"search CP","meta":{"question":"what?","options":["a","b"],"blocking":true,"answer":"approved"}}`)

	r := call(t, s.mcpSearchEntities, `{"type":"checkpoint","namespace":"test"}`)
	text := mustNotError(t, r)

	var results []map[string]any
	if err := json.Unmarshal([]byte(text), &results); err != nil {
		t.Fatalf("expected JSON array, got: %s", text)
	}
	if len(results) == 0 {
		t.Fatal("expected at least one result")
	}
	row := results[0]
	if row["question"] != "what?" {
		t.Errorf("expected question='what?', got %v", row["question"])
	}
	if row["blocking"] == nil {
		t.Error("expected blocking field")
	}
	if row["answer"] != "approved" {
		t.Errorf("expected answer='approved', got %v", row["answer"])
	}
}

// ── Test 7: get_entity ────────────────────────────────────────────────────────

func TestGetEntity_FullRead(t *testing.T) {
	s := testServer(t)

	// Create entity
	call(t, s.mcpUpsertEntity, `{"namespace":"ns1","type":"todo","label":"full read","meta":{"status":"open","priority":"high","blob_plus":"details"}}`)

	// Add steps
	call(t, s.mcpAddSteps, `{"todo_id":1,"steps":["code","test","build"]}`)

	// Mark one step done
	call(t, s.mcpStepDone, `{"todo_id":1,"step":"code"}`)

	r := call(t, s.mcpGetEntity, `{"id":1}`)
	text := mustNotError(t, r)

	var result map[string]any
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		t.Fatal(err)
	}

	if result["namespace"] != "ns1" {
		t.Errorf("expected namespace=ns1, got %v", result["namespace"])
	}
	if result["type"] != "todo" {
		t.Errorf("expected type=todo, got %v", result["type"])
	}
	if result["created"] == nil {
		t.Error("expected created timestamp")
	}
	if result["updated"] == nil {
		t.Error("expected updated timestamp")
	}

	steps := result["steps"].([]any)
	if len(steps) != 3 {
		t.Fatalf("expected 3 steps, got %d", len(steps))
	}
	// First step should be done
	step0 := steps[0].(map[string]any)
	if step0["step"] != "code" || step0["done"] != true {
		t.Errorf("expected step 'code' done=true, got %v", step0)
	}
}

// ── Test 8: timestamps in list_todos and search ───────────────────────────────

func TestListTodos_HasTimestamps(t *testing.T) {
	s := testServer(t)

	call(t, s.mcpUpsertEntity, `{"namespace":"test","type":"todo","label":"ts todo","meta":{"status":"open","priority":"normal"}}`)

	r := call(t, s.mcpListTodos, `{"namespace":"test"}`)
	text := mustNotError(t, r)

	if !strings.Contains(text, "(créé ") {
		t.Errorf("expected '(créé ...' timestamp in output, got:\n%s", text)
	}
}

// ── Steps idempotence on short key (before colon) ─────────────────────────

func TestAddSteps_ShortKeyDedup(t *testing.T) {
	s := testServer(t)

	// Create a todo
	r := call(t, s.mcpUpsertEntity, `{"namespace":"test","type":"todo","label":"TEST step dedup","meta":{"status":"open","priority":"normal"}}`)
	mustNotError(t, r)

	// Add step with long description
	r = call(t, s.mcpAddSteps, `{"todo_id":1,"steps":["env-var: ajouter TRACQLITE_CATALOG_DB dans wrapper script"]}`)
	mustNotError(t, r)
	if !strings.Contains(r.Content[0].Text, "1 steps added") {
		t.Fatalf("expected 1 step added, got: %s", r.Content[0].Text)
	}

	// Add step with short name only — same key, should be ignored
	r = call(t, s.mcpAddSteps, `{"todo_id":1,"steps":["env-var"]}`)
	mustNotError(t, r)
	if !strings.Contains(r.Content[0].Text, "0 steps added") {
		t.Fatalf("expected 0 steps added (dedup), got: %s", r.Content[0].Text)
	}
}

func TestStepDone_MatchesShortKey(t *testing.T) {
	s := testServer(t)

	// Create a todo
	call(t, s.mcpUpsertEntity, `{"namespace":"test","type":"todo","label":"TEST step match","meta":{"status":"in_progress","priority":"normal"}}`)

	// Add step with long description
	call(t, s.mcpAddSteps, `{"todo_id":1,"steps":["deploy: push binary to KS-5-B"]}`)

	// Mark done with short key
	r := call(t, s.mcpStepDone, `{"todo_id":1,"step":"deploy"}`)
	text := mustNotError(t, r)
	if !strings.Contains(text, "step done") {
		t.Fatalf("expected step done, got: %s", text)
	}
}

func TestAddSteps_NoColonStillWorks(t *testing.T) {
	s := testServer(t)

	call(t, s.mcpUpsertEntity, `{"namespace":"test","type":"todo","label":"TEST no colon","meta":{"status":"open","priority":"normal"}}`)

	// Add step without colon
	r := call(t, s.mcpAddSteps, `{"todo_id":1,"steps":["build"]}`)
	mustNotError(t, r)

	// Same step again — should dedup
	r = call(t, s.mcpAddSteps, `{"todo_id":1,"steps":["build"]}`)
	mustNotError(t, r)
	if !strings.Contains(r.Content[0].Text, "0 steps added") {
		t.Fatalf("expected dedup, got: %s", r.Content[0].Text)
	}

	// step_done with exact name
	r = call(t, s.mcpStepDone, `{"todo_id":1,"step":"build"}`)
	text := mustNotError(t, r)
	if !strings.Contains(text, "step done") {
		t.Fatalf("expected step done, got: %s", text)
	}
}

// ── Type mission + verrouillage cascadé ───────────────────────────────────

func TestUpsertMission_CreatesRelations(t *testing.T) {
	s := testServer(t)

	// Create two todos first
	call(t, s.mcpUpsertEntity, `{"namespace":"test","type":"todo","label":"TODO alpha","meta":{"status":"open","priority":"normal"}}`)
	call(t, s.mcpUpsertEntity, `{"namespace":"test","type":"todo","label":"TODO beta","meta":{"status":"open","priority":"normal"}}`)

	// Create a mission referencing both todos
	r := call(t, s.mcpUpsertEntity, `{"namespace":"test","type":"mission","label":"MISSION test batch","meta":{"priority":"high","todos":[1,2]}}`)
	text := mustNotError(t, r)
	if !strings.Contains(text, "created entity 3") {
		t.Fatalf("expected created entity 3, got: %s", text)
	}

	// Verify relations were auto-created (subtask_of: todo -> mission)
	var count int
	s.db.QueryRow(`SELECT COUNT(*) FROM relations WHERE to_id = 3 AND type = 'subtask_of'`).Scan(&count)
	if count != 2 {
		t.Fatalf("expected 2 subtask_of relations, got %d", count)
	}
}

func TestMissionTransition_LocksTodos(t *testing.T) {
	s := testServer(t)
	s.lastSessionID = "session-A"

	// Create todos + mission
	call(t, s.mcpUpsertEntity, `{"namespace":"test","type":"todo","label":"TODO one","meta":{"status":"open","priority":"normal"}}`)
	call(t, s.mcpUpsertEntity, `{"namespace":"test","type":"todo","label":"TODO two","meta":{"status":"open","priority":"normal"}}`)
	call(t, s.mcpUpsertEntity, `{"namespace":"test","type":"mission","label":"MISSION lock test","meta":{"priority":"high","todos":[1,2]}}`)

	// Transition mission to in_progress — should lock child todos
	r := call(t, s.mcpTodoTransition, `{"id":3,"status":"in_progress"}`)
	text := mustNotError(t, r)
	if !strings.Contains(text, "in_progress") {
		t.Fatalf("expected in_progress, got: %s", text)
	}

	// Verify child todos have locked_by in meta
	var meta string
	s.db.QueryRow(`SELECT json(meta) FROM entities WHERE id = 1`).Scan(&meta)
	if !strings.Contains(meta, `"locked_by":3`) {
		t.Fatalf("expected locked_by:3 in todo 1 meta, got: %s", meta)
	}
}

func TestLockedTodo_RejectsOtherSession(t *testing.T) {
	s := testServer(t)
	s.lastSessionID = "session-A"

	// Create todos + mission, lock
	call(t, s.mcpUpsertEntity, `{"namespace":"test","type":"todo","label":"TODO guarded","meta":{"status":"open","priority":"normal"}}`)
	call(t, s.mcpUpsertEntity, `{"namespace":"test","type":"mission","label":"MISSION guard","meta":{"priority":"high","todos":[1]}}`)
	call(t, s.mcpTodoTransition, `{"id":2,"status":"in_progress"}`)

	// Different session tries to work on the locked todo
	s.lastSessionID = "session-B"
	r := call(t, s.mcpTodoTransition, `{"id":1,"status":"in_progress"}`)
	if !r.IsError {
		t.Fatal("expected error for locked todo from different session")
	}
	if !strings.Contains(r.Content[0].Text, "verrouillée") && !strings.Contains(r.Content[0].Text, "locked") {
		t.Fatalf("expected lock error message, got: %s", r.Content[0].Text)
	}
}

func TestLockedTodo_AllowsSameSession(t *testing.T) {
	s := testServer(t)
	s.lastSessionID = "session-A"

	call(t, s.mcpUpsertEntity, `{"namespace":"test","type":"todo","label":"TODO allowed","meta":{"status":"open","priority":"normal"}}`)
	call(t, s.mcpUpsertEntity, `{"namespace":"test","type":"mission","label":"MISSION allow","meta":{"priority":"high","todos":[1]}}`)
	call(t, s.mcpTodoTransition, `{"id":2,"status":"in_progress"}`)

	// Same session should be allowed
	r := call(t, s.mcpTodoTransition, `{"id":1,"status":"in_progress"}`)
	if r.IsError {
		t.Fatalf("expected success for same session, got error: %s", r.Content[0].Text)
	}
}

func TestMissionDone_UnlocksTodos(t *testing.T) {
	s := testServer(t)
	s.lastSessionID = "session-A"

	call(t, s.mcpUpsertEntity, `{"namespace":"test","type":"todo","label":"TODO unlock","meta":{"status":"open","priority":"normal"}}`)
	call(t, s.mcpUpsertEntity, `{"namespace":"test","type":"mission","label":"MISSION unlock","meta":{"priority":"high","todos":[1]}}`)

	// Lock
	call(t, s.mcpTodoTransition, `{"id":2,"status":"in_progress"}`)
	// Complete child todo
	call(t, s.mcpTodoTransition, `{"id":1,"status":"in_progress"}`)
	call(t, s.mcpTodoTransition, `{"id":1,"status":"done"}`)
	// Complete mission
	r := call(t, s.mcpTodoTransition, `{"id":2,"status":"done"}`)
	text := mustNotError(t, r)
	if !strings.Contains(text, "done") {
		t.Fatalf("expected done, got: %s", text)
	}

	// Verify lock removed
	var meta string
	s.db.QueryRow(`SELECT json(meta) FROM entities WHERE id = 1`).Scan(&meta)
	if strings.Contains(meta, `"locked_by"`) {
		t.Fatalf("expected locked_by removed, still present: %s", meta)
	}
}

func TestSearchEntities_HasCreatedTimestamp(t *testing.T) {
	s := testServer(t)

	call(t, s.mcpUpsertEntity, `{"namespace":"test","type":"decision","label":"ts decision","meta":{}}`)

	r := call(t, s.mcpSearchEntities, `{"namespace":"test"}`)
	text := mustNotError(t, r)

	var results []map[string]any
	json.Unmarshal([]byte(text), &results)
	if len(results) == 0 {
		t.Fatal("expected results")
	}
	if results[0]["created"] == nil {
		t.Error("expected 'created' field in search results")
	}
}
