package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// pipePair builds a duplex byte stream out of two os pipes — the natural
// fit for a JSON-RPC client/server pair living in the same process.
//
// Returns (clientReader, clientWriter, serverReader, serverWriter): the
// client reads what the server writes, and vice-versa.
func pipePair() (io.Reader, io.Writer, io.Reader, io.Writer, func()) {
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	closer := func() {
		_ = cr.Close()
		_ = cw.Close()
		_ = sr.Close()
		_ = sw.Close()
	}
	return cr, cw, sr, sw, closer
}

// fakeServer reads NDJSON frames from sr and writes responses to sw under a
// caller-provided handler. It exists so tests can drive the client without
// dragging in a real codex binary.
type fakeServer struct {
	reader  io.Reader
	writer  io.Writer
	handler func(env JSONRPCEnvelope) []JSONRPCEnvelope
	stop    chan struct{}
}

func (s *fakeServer) run(t *testing.T) {
	t.Helper()
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		select {
		case <-s.stop:
			return
		default:
		}
		n, err := s.reader.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			for {
				idx := -1
				for i, b := range buf {
					if b == '\n' {
						idx = i
						break
					}
				}
				if idx < 0 {
					break
				}
				line := buf[:idx]
				buf = buf[idx+1:]
				if len(line) == 0 {
					continue
				}
				var env JSONRPCEnvelope
				if jerr := json.Unmarshal(line, &env); jerr != nil {
					t.Logf("fakeServer: malformed frame: %v", jerr)
					continue
				}
				for _, out := range s.handler(env) {
					b, err := json.Marshal(out)
					if err != nil {
						t.Logf("fakeServer: marshal: %v", err)
						continue
					}
					if _, err := s.writer.Write(append(b, '\n')); err != nil {
						return
					}
				}
			}
		}
		if err != nil {
			return
		}
	}
}

// TestCodexJSONRPCCallSuccess: vanilla request/response round-trip.
func TestCodexJSONRPCCallSuccess(t *testing.T) {
	cr, cw, sr, sw, closer := pipePair()
	defer closer()

	server := &fakeServer{
		reader: sr, writer: sw, stop: make(chan struct{}),
		handler: func(env JSONRPCEnvelope) []JSONRPCEnvelope {
			if env.Method != "thread/start" {
				t.Errorf("unexpected method %s", env.Method)
			}
			result := json.RawMessage(`{"thread":{"id":"thr_abc","modelProvider":"openai","createdAt":1,"updatedAt":2,"status":{"type":"idle"},"cwd":"/tmp"},"model":"gpt","modelProvider":"openai","cwd":"/tmp","approvalPolicy":"never","approvalsReviewer":"local"}`)
			return []JSONRPCEnvelope{{ID: env.ID, Result: result}}
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := NewCodexJSONRPCClient(ctx, cr, cw)
	go server.run(t)
	go client.Run()
	defer client.Close()

	res, err := client.Call(ctx, "thread/start", &CodexThreadStartParams{Model: "gpt", Cwd: "/tmp"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var resp CodexThreadStartResponse
	if err := json.Unmarshal(res, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Thread.ID != "thr_abc" {
		t.Errorf("got thread id %q, want thr_abc", resp.Thread.ID)
	}
	if resp.Model != "gpt" {
		t.Errorf("got model %q, want gpt", resp.Model)
	}
}

// TestCodexJSONRPCCallError: server returns a JSON-RPC error.
func TestCodexJSONRPCCallError(t *testing.T) {
	cr, cw, sr, sw, closer := pipePair()
	defer closer()

	server := &fakeServer{
		reader: sr, writer: sw, stop: make(chan struct{}),
		handler: func(env JSONRPCEnvelope) []JSONRPCEnvelope {
			return []JSONRPCEnvelope{{
				ID: env.ID,
				Error: &JSONRPCErrorObj{
					Code: CodexJSONRPCErrServerOverloaded, Message: "Server overloaded",
				},
			}}
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := NewCodexJSONRPCClient(ctx, cr, cw)
	go server.run(t)
	go client.Run()
	defer client.Close()

	_, err := client.Call(ctx, "thread/start", nil)
	if err == nil {
		t.Fatalf("expected error")
	}
	var jerr *JSONRPCErrorObj
	if !errors.As(err, &jerr) {
		t.Fatalf("expected *JSONRPCErrorObj, got %T (%v)", err, err)
	}
	if jerr.Code != CodexJSONRPCErrServerOverloaded {
		t.Errorf("got code %d, want %d", jerr.Code, CodexJSONRPCErrServerOverloaded)
	}
}

// TestCodexJSONRPCNotification: client→server fire-and-forget.
func TestCodexJSONRPCNotification(t *testing.T) {
	cr, cw, sr, sw, closer := pipePair()
	defer closer()

	var got atomic.Pointer[JSONRPCEnvelope]
	server := &fakeServer{
		reader: sr, writer: sw, stop: make(chan struct{}),
		handler: func(env JSONRPCEnvelope) []JSONRPCEnvelope {
			cp := env
			got.Store(&cp)
			return nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := NewCodexJSONRPCClient(ctx, cr, cw)
	go server.run(t)
	go client.Run()
	defer client.Close()

	if err := client.Notify("initialized", nil); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	// Wait for the frame to round-trip.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if got.Load() != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	g := got.Load()
	if g == nil {
		t.Fatalf("server didn't observe notification")
	}
	if g.Method != "initialized" {
		t.Errorf("got method %q, want initialized", g.Method)
	}
	if len(g.ID) > 0 && string(g.ID) != "null" {
		t.Errorf("notification carried an id: %s", string(g.ID))
	}
}

// TestCodexJSONRPCServerRequest: server→client request, client replies.
func TestCodexJSONRPCServerRequest(t *testing.T) {
	cr, cw, sr, sw, closer := pipePair()
	defer closer()

	// We need a 2-way conversation: server pushes a request out of the
	// blue and waits for the client's response.
	respCh := make(chan JSONRPCEnvelope, 1)
	server := &fakeServer{
		reader: sr, writer: sw, stop: make(chan struct{}),
		handler: func(env JSONRPCEnvelope) []JSONRPCEnvelope {
			respCh <- env
			return nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := NewCodexJSONRPCClient(ctx, cr, cw)
	client.OnRequest(ServerReqCommandExecApproval, func(params json.RawMessage) (any, error) {
		var p CodexCommandExecApprovalParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		if p.Command != "rm -rf /tmp/foo" {
			t.Errorf("got command %q", p.Command)
		}
		return &CodexCommandExecApprovalResponse{Decision: CodexCommandExecApprovalDecline}, nil
	})

	go server.run(t)
	go client.Run()
	defer client.Close()

	// Push a server request.
	out := JSONRPCEnvelope{
		ID:     json.RawMessage(`"srv-req-1"`),
		Method: ServerReqCommandExecApproval,
		Params: json.RawMessage(`{"threadId":"t","turnId":"u","itemId":"i","command":"rm -rf /tmp/foo"}`),
	}
	b, _ := json.Marshal(out)
	if _, err := sw.Write(append(b, '\n')); err != nil {
		t.Fatalf("server write: %v", err)
	}

	select {
	case got := <-respCh:
		if string(got.ID) != `"srv-req-1"` {
			t.Errorf("response id mismatch: %s", string(got.ID))
		}
		var r CodexCommandExecApprovalResponse
		if err := json.Unmarshal(got.Result, &r); err != nil {
			t.Fatalf("unmarshal response: %v", err)
		}
		if r.Decision != CodexCommandExecApprovalDecline {
			t.Errorf("got decision %q", r.Decision)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for server-request response")
	}
}

// TestCodexJSONRPCServerRequestUnhandled: no handler → method-not-found error.
func TestCodexJSONRPCServerRequestUnhandled(t *testing.T) {
	cr, cw, sr, sw, closer := pipePair()
	defer closer()

	respCh := make(chan JSONRPCEnvelope, 1)
	server := &fakeServer{
		reader: sr, writer: sw, stop: make(chan struct{}),
		handler: func(env JSONRPCEnvelope) []JSONRPCEnvelope {
			respCh <- env
			return nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := NewCodexJSONRPCClient(ctx, cr, cw)
	go server.run(t)
	go client.Run()
	defer client.Close()

	out := JSONRPCEnvelope{
		ID:     json.RawMessage(`42`),
		Method: "unknown/method",
	}
	b, _ := json.Marshal(out)
	_, _ = sw.Write(append(b, '\n'))

	select {
	case got := <-respCh:
		if got.Error == nil {
			t.Fatal("expected error response")
		}
		if got.Error.Code != CodexJSONRPCErrMethodNotFound {
			t.Errorf("got code %d, want %d", got.Error.Code, CodexJSONRPCErrMethodNotFound)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
	}
}

// TestCodexJSONRPCNotificationDispatch: server pushes notification, client
// handler fires.
func TestCodexJSONRPCNotificationDispatch(t *testing.T) {
	cr, cw, _, sw, closer := pipePair()
	defer closer()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := NewCodexJSONRPCClient(ctx, cr, cw)
	gotCh := make(chan json.RawMessage, 1)
	client.OnNotification(NotifTurnCompleted, func(params json.RawMessage) {
		gotCh <- params
	})
	go client.Run()
	defer client.Close()

	out := JSONRPCEnvelope{
		Method: NotifTurnCompleted,
		Params: json.RawMessage(`{"threadId":"t","turn":{"id":"u","status":"completed"}}`),
	}
	b, _ := json.Marshal(out)
	_, _ = sw.Write(append(b, '\n'))

	select {
	case raw := <-gotCh:
		var n CodexTurnCompletedNotification
		if err := json.Unmarshal(raw, &n); err != nil {
			t.Fatalf("unmarshal notif: %v", err)
		}
		if n.Turn.ID != "u" || n.Turn.Status != CodexTurnStatusCompleted {
			t.Errorf("got %+v", n.Turn)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("notification not delivered")
	}
}

// TestCodexJSONRPCConcurrentCalls: many parallel callers, all matched by id.
func TestCodexJSONRPCConcurrentCalls(t *testing.T) {
	cr, cw, sr, sw, closer := pipePair()
	defer closer()

	server := &fakeServer{
		reader: sr, writer: sw, stop: make(chan struct{}),
		handler: func(env JSONRPCEnvelope) []JSONRPCEnvelope {
			// Echo the params back as the result.
			return []JSONRPCEnvelope{{ID: env.ID, Result: env.Params}}
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := NewCodexJSONRPCClient(ctx, cr, cw)
	go server.run(t)
	go client.Run()
	defer client.Close()

	const N = 32
	var wg sync.WaitGroup
	wg.Add(N)
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			payload := fmt.Sprintf(`{"i":%d}`, i)
			res, err := client.Call(ctx, "echo", json.RawMessage(payload))
			if err != nil {
				errs <- err
				return
			}
			if string(res) != payload {
				errs <- fmt.Errorf("got %s, want %s", res, payload)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// TestCodexJSONRPCContextCancel: Call returns ctx.Err on cancel.
func TestCodexJSONRPCContextCancel(t *testing.T) {
	cr, cw, _, _, closer := pipePair()
	defer closer()

	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := NewCodexJSONRPCClient(parent, cr, cw)
	go client.Run()
	defer client.Close()

	ctx, callCancel := context.WithCancel(context.Background())
	doneCh := make(chan error, 1)
	go func() {
		_, err := client.Call(ctx, "never/responds", nil)
		doneCh <- err
	}()
	time.Sleep(50 * time.Millisecond)
	callCancel()

	select {
	case err := <-doneCh:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("got %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Call didn't unblock on ctx cancel")
	}
}

// TestCodexJSONRPCDropOldest: bounded queue overflow drops oldest.
func TestCodexJSONRPCDropOldest(t *testing.T) {
	// Use a writer that blocks indefinitely so the writer goroutine can't
	// drain the queue. With the queue at capacity, every additional send
	// must drop something.
	pr, pw := io.Pipe()
	defer pr.Close()
	defer pw.Close()

	// Reader is irrelevant; we block on writer.
	cr, _ := io.Pipe()
	defer cr.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := NewCodexJSONRPCClient(ctx, cr, blockingWriter{})
	// Don't start Run — we want the writer side stuck on the unstarted
	// goroutine. Instead, exercise enqueue() directly.
	for i := 0; i < cap(client.outbound)+10; i++ {
		_ = client.Notify("noop", json.RawMessage(`{}`))
	}
	stats := client.Stats()
	if stats.OutboundDrops < 10 {
		t.Errorf("expected >=10 drops, got %d", stats.OutboundDrops)
	}
}

type blockingWriter struct{}

func (blockingWriter) Write(p []byte) (int, error) {
	// Not actually called in TestCodexJSONRPCDropOldest because we never
	// call client.Run(). Defensive.
	select {}
}

// TestCodexJSONRPCMalformedFrameIgnored: malformed line doesn't kill the loop.
func TestCodexJSONRPCMalformedFrameIgnored(t *testing.T) {
	cr, cw, _, sw, closer := pipePair()
	defer closer()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var logs strings.Builder
	var logMu sync.Mutex
	client := NewCodexJSONRPCClient(ctx, cr, cw, WithJSONRPCLogger(func(format string, args ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		fmt.Fprintf(&logs, format+"\n", args...)
	}))
	gotCh := make(chan json.RawMessage, 1)
	client.OnNotification("ping", func(params json.RawMessage) { gotCh <- params })
	go client.Run()
	defer client.Close()

	// Garbage line, then a valid notification.
	_, _ = sw.Write([]byte("not-json\n"))
	out := JSONRPCEnvelope{Method: "ping", Params: json.RawMessage(`{"ok":true}`)}
	b, _ := json.Marshal(out)
	_, _ = sw.Write(append(b, '\n'))

	select {
	case <-gotCh:
		// Good — we recovered from the malformed line.
	case <-time.After(2 * time.Second):
		t.Fatal("client died on malformed frame")
	}
	logMu.Lock()
	defer logMu.Unlock()
	if !strings.Contains(logs.String(), "malformed") {
		t.Errorf("expected malformed log, got %q", logs.String())
	}
}

// TestCodexJSONRPCTransportClose: pending calls fail with a useful error.
func TestCodexJSONRPCTransportClose(t *testing.T) {
	cr, cw, sr, sw, closer := pipePair()
	// Defer closer so the unused server-side reader (sr) and the client
	// writer (cw) get cleaned up even if the test fails before reaching
	// the explicit close calls below. closeWriter remains the explicit
	// "close just the producer side" hook used by the test.
	defer closer()
	_ = sr // unused — server side never reads in this test
	closeWriter := func() {
		if pw, ok := sw.(io.Closer); ok {
			_ = pw.Close()
		}
		if pr, ok := cr.(io.Closer); ok {
			_ = pr.Close()
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := NewCodexJSONRPCClient(ctx, cr, cw)
	go client.Run()

	doneCh := make(chan error, 1)
	go func() {
		_, err := client.Call(ctx, "never/responds", nil)
		doneCh <- err
	}()
	time.Sleep(50 * time.Millisecond)
	closeWriter()
	client.Close()

	select {
	case err := <-doneCh:
		if err == nil {
			t.Fatal("expected error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Call didn't unblock on transport close")
	}
}
