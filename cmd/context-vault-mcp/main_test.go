package main

import (
	"bufio"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"
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
	resp := handleRequest(nil, req, true)
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
	resp := handleRequest(nil, req, false)
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

	resp := handleRequest(dc, req, false)
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
