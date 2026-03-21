package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"context"

	"github.com/hazyhaar/context-vault/internal/vault"
	_ "modernc.org/sqlite"
)

func testDaemon(t *testing.T) (*daemon, net.Listener) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(vault.Schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	d := newDaemon(db)

	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go d.handleConn(ctx, conn)
		}
	}()

	return d, ln
}

func dial(t *testing.T, ln net.Listener) (net.Conn, *bufio.Scanner) {
	t.Helper()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	return conn, scanner
}

func rpcCall(t *testing.T, conn net.Conn, scanner *bufio.Scanner, method string, params any) jsonrpcResponse {
	t.Helper()
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  params,
	}
	b, _ := json.Marshal(req)
	b = append(b, '\n')
	conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write(b); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if !scanner.Scan() {
		t.Fatalf("no response: %v", scanner.Err())
	}
	var resp jsonrpcResponse
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestDaemon_Register(t *testing.T) {
	_, ln := testDaemon(t)
	conn, scanner := dial(t, ln)

	resp := rpcCall(t, conn, scanner, "register", map[string]string{"session_id": "s1", "role": "worker"})
	if resp.Error != nil {
		t.Fatalf("unexpected error: %s", resp.Error.Message)
	}
}

func TestDaemon_UpsertAndGet(t *testing.T) {
	_, ln := testDaemon(t)
	conn, scanner := dial(t, ln)

	// Create entity
	resp := rpcCall(t, conn, scanner, "vault/upsert_entity", map[string]any{
		"namespace": "test", "type": "decision", "label": "daemon test", "meta": map[string]any{},
	})
	if resp.Error != nil {
		t.Fatalf("upsert error: %s", resp.Error.Message)
	}
	// Result is a vault.Result JSON — check it contains "created entity"
	resultBytes, _ := json.Marshal(resp.Result)
	if !strings.Contains(string(resultBytes), "created entity") {
		t.Fatalf("expected 'created entity', got: %s", string(resultBytes))
	}

	// Get entity
	resp = rpcCall(t, conn, scanner, "vault/get_entity", map[string]any{"id": 1})
	if resp.Error != nil {
		t.Fatalf("get error: %s", resp.Error.Message)
	}
	resultBytes, _ = json.Marshal(resp.Result)
	if !strings.Contains(string(resultBytes), "daemon test") {
		t.Fatalf("expected 'daemon test', got: %s", string(resultBytes))
	}
}

func TestDaemon_UnknownMethod(t *testing.T) {
	_, ln := testDaemon(t)
	conn, scanner := dial(t, ln)

	resp := rpcCall(t, conn, scanner, "vault/nonexistent", map[string]any{})
	if resp.Error == nil {
		t.Fatal("expected error for unknown method")
	}
	if resp.Error.Code != -32601 {
		t.Fatalf("expected -32601, got %d", resp.Error.Code)
	}
}

// ── E2E: daemon + worker + supervisor ────────────────────────────────────────

func TestE2E_CheckpointRouting(t *testing.T) {
	d, ln := testDaemon(t)

	// Start checkpoint watcher
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go d.watchCheckpoints(ctx)

	// Connect worker
	workerConn, workerScanner := dial(t, ln)
	rpcCall(t, workerConn, workerScanner, "register", map[string]string{"session_id": "worker-1", "role": "worker"})

	// Connect supervisor
	supConn, supScanner := dial(t, ln)
	rpcCall(t, supConn, supScanner, "register", map[string]string{"session_id": "sup-1", "role": "supervisor"})

	// Worker creates a blocking checkpoint (session_origin set manually since vault.SessionFn returns "")
	// We need to set session_origin on the entity — use direct DB for that
	resp := rpcCall(t, workerConn, workerScanner, "vault/upsert_entity", map[string]any{
		"namespace": "test",
		"type":      "checkpoint",
		"label":     "CHECKPOINT e2e test",
		"meta": map[string]any{
			"question": "Approve?",
			"blocking": true,
		},
	})
	resultBytes, _ := json.Marshal(resp.Result)
	if !strings.Contains(string(resultBytes), "created entity") {
		t.Fatalf("expected created entity, got: %s", string(resultBytes))
	}

	// Set session_origin on the checkpoint to "worker-1" (the daemon's SessionFn returns "")
	d.db.Exec(`UPDATE entities SET session_origin = 'worker-1' WHERE id = 1`)

	// Wait for watcher to detect the new checkpoint and push to supervisor
	supConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if !supScanner.Scan() {
		t.Fatal("supervisor did not receive checkpoint notification")
	}
	notifLine := supScanner.Text()
	if !strings.Contains(notifLine, "checkpoint_created") {
		t.Fatalf("expected checkpoint_created notification, got: %s", notifLine)
	}
	if !strings.Contains(notifLine, "CHECKPOINT e2e test") {
		t.Fatalf("expected checkpoint label in notification, got: %s", notifLine)
	}

	// Supervisor answers the checkpoint
	rpcCall(t, supConn, supScanner, "vault/upsert_entity", map[string]any{
		"id":    1,
		"label": "CHECKPOINT e2e test",
		"meta":  map[string]any{"answer": "Approved"},
	})

	// Wait for watcher to detect the answer and push to worker
	workerConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if !workerScanner.Scan() {
		t.Fatal("worker did not receive checkpoint_answered notification")
	}
	answerLine := workerScanner.Text()
	if !strings.Contains(answerLine, "checkpoint_answered") {
		t.Fatalf("expected checkpoint_answered notification, got: %s", answerLine)
	}
	if !strings.Contains(answerLine, "Approved") {
		t.Fatalf("expected answer 'Approved' in notification, got: %s", answerLine)
	}
}
