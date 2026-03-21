package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hazyhaar/context-vault/internal/vault"
	_ "modernc.org/sqlite"
)

func TestToolNameToMethod(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"vault_get_context", "vault/get_context"},
		{"vault_upsert_entity", "vault/upsert_entity"},
		{"vault_todo_transition", "vault/todo_transition"},
		{"unknown", "unknown"},
	}
	for _, tt := range tests {
		got := toolNameToMethod(tt.in)
		if got != tt.want {
			t.Errorf("toolNameToMethod(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestHandleRequest_Initialize(t *testing.T) {
	req := &jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  "initialize",
	}
	resp := handleRequest(nil, nil, req, true)
	if resp == nil {
		t.Fatal("expected response")
	}
	b, _ := json.Marshal(resp.Result)
	if !strings.Contains(string(b), "context-vault") {
		t.Fatalf("expected server info, got: %s", string(b))
	}
	if !strings.Contains(string(b), "claude/channel") {
		t.Fatalf("expected channel capability, got: %s", string(b))
	}
}

func TestHandleRequest_ToolsList(t *testing.T) {
	req := &jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`2`),
		Method:  "tools/list",
	}
	resp := handleRequest(nil, nil, req, false)
	if resp == nil {
		t.Fatal("expected response")
	}
	b, _ := json.Marshal(resp.Result)
	if !strings.Contains(string(b), "vault_get_context") {
		t.Fatalf("expected tool definitions, got: %s", string(b))
	}
}

// TestHandleRequest_ToolsCallForward tests the full path: thin client → daemon → response.
func TestHandleRequest_ToolsCallForward(t *testing.T) {
	// Start a mock daemon that echoes back a fixed response
	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		scanner := bufio.NewScanner(conn)
		scanner.Buffer(make([]byte, 1<<20), 1<<20)
		enc := json.NewEncoder(conn)

		for scanner.Scan() {
			var req jsonrpcRequest
			json.Unmarshal(scanner.Bytes(), &req)

			// Respond with a mock vault result
			resp := jsonrpcResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result: map[string]any{
					"content":  []map[string]string{{"type": "text", "text": "mock result for " + req.Method}},
					"isError":  false,
				},
			}
			enc.Encode(resp)
		}
	}()

	// Dial the mock daemon
	conn, err := net.DialTimeout("tcp", ln.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	dc := &daemonConn{
		conn:    conn,
		enc:     json.NewEncoder(conn),
		scanner: bufio.NewScanner(conn),
	}
	dc.scanner.Buffer(make([]byte, 1<<20), 1<<20)

	// Simulate tools/call
	params, _ := json.Marshal(map[string]any{
		"name":      "vault_get_context",
		"arguments": map[string]any{"namespace": "test"},
	})
	req := &jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`42`),
		Method:  "tools/call",
		Params:  params,
	}

	resp := handleRequest(dc, nil, req, false)
	if resp == nil {
		t.Fatal("expected response")
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %s", resp.Error.Message)
	}

	b, _ := json.Marshal(resp.Result)
	if !strings.Contains(string(b), "mock result for vault/get_context") {
		t.Fatalf("expected forwarded result, got: %s", string(b))
	}
}

func TestHandleRequest_FallbackLocal(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(vault.Schema); err != nil {
		t.Fatal(err)
	}
	v := vault.New(db, func() string { return "" })

	// Create entity via fallback
	params, _ := json.Marshal(map[string]any{
		"name":      "vault_upsert_entity",
		"arguments": map[string]any{"namespace": "test", "type": "decision", "label": "fallback test", "meta": map[string]any{}},
	})
	req := &jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`10`),
		Method:  "tools/call",
		Params:  params,
	}
	resp := handleRequest(nil, v, req, false)
	if resp == nil {
		t.Fatal("expected response")
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %s", resp.Error.Message)
	}
	b, _ := json.Marshal(resp.Result)
	if !strings.Contains(string(b), "created entity") {
		t.Fatalf("expected 'created entity', got: %s", string(b))
	}
}

// ── Mux tests ─────────────────────────────────────────────────────────────────

// fakeDaemon accepts one connection, reads a register + one request,
// and optionally sends a notification BEFORE the response.
func fakeDaemon(t *testing.T, sendNotifBeforeResp bool) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		scanner := bufio.NewScanner(conn)
		scanner.Buffer(make([]byte, 1<<20), 1<<20)
		enc := json.NewEncoder(conn)

		// Read register call
		if !scanner.Scan() {
			return
		}
		// Respond to register
		enc.Encode(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(`1`), "result": "ok"})

		// Read the actual vault call
		if !scanner.Scan() {
			return
		}
		var req jsonrpcRequest
		json.Unmarshal(scanner.Bytes(), &req)

		if sendNotifBeforeResp {
			// Send a notification BEFORE the response — this is what the daemon does
			enc.Encode(map[string]any{
				"jsonrpc": "2.0",
				"method":  "notify/checkpoint_created",
				"params": map[string]any{
					"checkpoint_id": 99,
					"label":         "CHECKPOINT test notification",
					"question":      "Approve?",
				},
			})
		}

		// Now send the actual response
		enc.Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result":  map[string]any{"text": "success"},
		})
	}()
	return ln
}

// TestMux_NotificationBeforeResponse verifies that when the daemon sends a
// notification before a response, the mux correctly separates them.
func TestMux_NotificationBeforeResponse(t *testing.T) {
	ln := fakeDaemon(t, true)

	dc, err := dialDaemon(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer dc.close()

	// Register (pre-mux, synchronous)
	regParams, _ := json.Marshal(map[string]string{"session_id": "test-1", "role": "worker"})
	if _, err := dc.callDaemon("register", regParams); err != nil {
		t.Fatal("register failed:", err)
	}

	// Capture notifications via the mux
	notifCh := make(chan jsonrpcNotification, 10)
	dc.startMux(func(notif jsonrpcNotification) {
		notifCh <- notif
	})

	// Call a vault method — daemon will send notification THEN response
	params, _ := json.Marshal(map[string]any{"namespace": "test"})
	resp, err := dc.callDaemon("vault/get_context", params)
	if err != nil {
		t.Fatal("callDaemon failed:", err)
	}

	// The response should be the actual result, not the notification
	resultBytes, _ := json.Marshal(resp.Result)
	if !strings.Contains(string(resultBytes), "success") {
		t.Fatalf("expected 'success' in response, got: %s", string(resultBytes))
	}

	// The notification should have been captured
	select {
	case notif := <-notifCh:
		if notif.Method != "notify/checkpoint_created" {
			t.Fatalf("expected notify/checkpoint_created, got: %s", notif.Method)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("notification was not received by mux")
	}
}

// TestMux_NoNotification verifies normal request/response works with mux active.
func TestMux_NoNotification(t *testing.T) {
	ln := fakeDaemon(t, false)

	dc, err := dialDaemon(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer dc.close()

	regParams, _ := json.Marshal(map[string]string{"session_id": "test-2", "role": "worker"})
	if _, err := dc.callDaemon("register", regParams); err != nil {
		t.Fatal("register failed:", err)
	}

	dc.startMux(func(notif jsonrpcNotification) {
		t.Error("unexpected notification received")
	})

	params, _ := json.Marshal(map[string]any{"namespace": "test"})
	resp, err := dc.callDaemon("vault/get_context", params)
	if err != nil {
		t.Fatal("callDaemon failed:", err)
	}

	resultBytes, _ := json.Marshal(resp.Result)
	if !strings.Contains(string(resultBytes), "success") {
		t.Fatalf("expected 'success' in response, got: %s", string(resultBytes))
	}
}

// TestMux_DaemonClose verifies graceful handling when daemon closes connection.
func TestMux_DaemonClose(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	// Accept, respond to register, then close
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		scanner := bufio.NewScanner(conn)
		scanner.Buffer(make([]byte, 1<<20), 1<<20)
		enc := json.NewEncoder(conn)
		if scanner.Scan() {
			enc.Encode(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(`1`), "result": "ok"})
		}
		// Close immediately after register
		conn.Close()
	}()

	dc, err := dialDaemon(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer dc.close()

	regParams, _ := json.Marshal(map[string]string{"session_id": "test-3", "role": "worker"})
	if _, err := dc.callDaemon("register", regParams); err != nil {
		t.Fatal("register failed:", err)
	}

	dc.startMux(func(notif jsonrpcNotification) {})

	// The mux read goroutine should detect the closed connection
	select {
	case <-dc.closed:
		// OK — mux detected connection close
	case <-time.After(2 * time.Second):
		t.Fatal("mux did not detect closed connection")
	}
}

// TestForwardNotification_ChannelFormat verifies that forwardNotification emits
// the notifications/claude/channel format (content+meta), not notifications/message.
func TestForwardNotification_ChannelFormat(t *testing.T) {
	// Capture stdout via a pipe-backed stdoutWriter
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	out := &stdoutWriter{enc: json.NewEncoder(pw)}

	notif := jsonrpcNotification{
		JSONRPC: "2.0",
		Method:  "notify/checkpoint_answered",
		Params: map[string]any{
			"checkpoint_id": float64(42),
			"label":         "Approve design",
			"answer":        "approuver",
		},
	}

	forwardNotification(out, notif)
	pw.Close()

	scanner := bufio.NewScanner(pr)
	if !scanner.Scan() {
		t.Fatal("no output from forwardNotification")
	}

	var got map[string]any
	if err := json.Unmarshal(scanner.Bytes(), &got); err != nil {
		t.Fatal("invalid JSON:", err)
	}

	// Must be notifications/claude/channel, NOT notifications/message
	method, _ := got["method"].(string)
	if method != "notifications/claude/channel" {
		t.Fatalf("expected method notifications/claude/channel, got %q", method)
	}

	params, ok := got["params"].(map[string]any)
	if !ok {
		t.Fatal("params is not an object")
	}

	// Must have "content" string field
	content, ok := params["content"].(string)
	if !ok || content == "" {
		t.Fatalf("expected non-empty content string, got %v", params["content"])
	}

	// Must have "meta" object with event and checkpoint_id
	meta, ok := params["meta"].(map[string]any)
	if !ok {
		t.Fatal("meta is not an object")
	}
	if meta["event"] != "checkpoint_answered" {
		t.Fatalf("expected event=checkpoint_answered, got %v", meta["event"])
	}
	if meta["checkpoint_id"] == nil {
		t.Fatal("meta missing checkpoint_id")
	}

	// Must NOT have "level" or "data" (old notifications/message format)
	if params["level"] != nil {
		t.Fatal("params should not have 'level' (old format)")
	}
	if params["data"] != nil {
		t.Fatal("params should not have 'data' (old format)")
	}
}

// TestForwardNotification_CheckpointCreated verifies checkpoint_created notification format.
func TestForwardNotification_CheckpointCreated(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	out := &stdoutWriter{enc: json.NewEncoder(pw)}

	notif := jsonrpcNotification{
		JSONRPC: "2.0",
		Method:  "notify/checkpoint_created",
		Params: map[string]any{
			"checkpoint_id": float64(99),
			"label":         "Deploy approval",
			"question":      "Proceed with deploy?",
		},
	}

	forwardNotification(out, notif)
	pw.Close()

	scanner := bufio.NewScanner(pr)
	if !scanner.Scan() {
		t.Fatal("no output")
	}

	var got map[string]any
	if err := json.Unmarshal(scanner.Bytes(), &got); err != nil {
		t.Fatal("invalid JSON:", err)
	}

	if got["method"] != "notifications/claude/channel" {
		t.Fatalf("wrong method: %v", got["method"])
	}

	params := got["params"].(map[string]any)
	meta := params["meta"].(map[string]any)
	if meta["event"] != "checkpoint_created" {
		t.Fatalf("wrong event: %v", meta["event"])
	}
	content := params["content"].(string)
	if !strings.Contains(content, "Deploy approval") {
		t.Fatalf("content should mention the label, got: %s", content)
	}
}

// TestMuxE2E_DaemonPushToStdout verifies the full chain: daemon push → mux → forwardNotification → stdout.
func TestMuxE2E_DaemonPushToStdout(t *testing.T) {
	ln := fakeDaemon(t, true) // sends notify/checkpoint_created before response

	dc, err := dialDaemon(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer dc.close()

	regParams, _ := json.Marshal(map[string]string{"session_id": "e2e-1", "role": "supervisor"})
	if _, err := dc.callDaemon("register", regParams); err != nil {
		t.Fatal("register:", err)
	}

	// Pipe to capture stdout-like output
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	out := &stdoutWriter{enc: json.NewEncoder(pw)}

	// Start mux with forwardNotification as the callback
	dc.startMux(func(notif jsonrpcNotification) {
		forwardNotification(out, notif)
	})

	// Trigger a call — daemon sends notification then response
	params, _ := json.Marshal(map[string]any{"namespace": "test"})
	resp, err := dc.callDaemon("vault/get_context", params)
	if err != nil {
		t.Fatal("callDaemon:", err)
	}
	resultBytes, _ := json.Marshal(resp.Result)
	if !strings.Contains(string(resultBytes), "success") {
		t.Fatalf("expected success response, got: %s", resultBytes)
	}

	// Give mux time to forward the notification
	time.Sleep(100 * time.Millisecond)
	pw.Close()

	// Read the channel event from the pipe
	scanner := bufio.NewScanner(pr)
	if !scanner.Scan() {
		t.Fatal("no channel event on stdout")
	}

	var got map[string]any
	if err := json.Unmarshal(scanner.Bytes(), &got); err != nil {
		t.Fatal("invalid JSON on stdout:", err)
	}

	if got["method"] != "notifications/claude/channel" {
		t.Fatalf("expected notifications/claude/channel, got %v", got["method"])
	}

	params2 := got["params"].(map[string]any)
	if params2["content"] == nil || params2["content"] == "" {
		t.Fatal("missing content field")
	}
	meta := params2["meta"].(map[string]any)
	if meta["event"] != "checkpoint_created" {
		t.Fatalf("expected checkpoint_created event, got %v", meta["event"])
	}
}
