package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// ── JSON-RPC 2.0 client (Round 2) ──
//
// Generic client wired to talk to codex's `codex app-server`. Codex omits the
// `"jsonrpc": "2.0"` field on the wire and frames messages as NDJSON
// (newline-delimited JSON) over whatever transport is chosen at spawn time
// (stdio / unix / ws). This file deals only with the framing + dispatch
// layer; the typed protocol structs are in codex_protocol.go.
//
// Design constraints carried over from Round 1's plan:
//
//   - Round 2 lands the client *and* the protocol structs but does NOT yet
//     instantiate either inside server_session.go — Round 3 wires the
//     codex backend up. So the API here has to be self-sufficient and
//     friendly to mocking from tests.
//
//   - Bidirectional. The server is both responder (to our Call) and
//     originator (sends */requestApproval as a request *to us*). Notifications
//     also flow both directions.
//
//   - Bounded. Slow consumers must not pile up unbounded memory. Outbound
//     queue is 256 messages; oldest is dropped on overflow with a metric
//     bump (drops counter). Inbound dispatch is goroutine-per-message so a
//     wedged handler doesn't stall the read loop.
//
//   - Concurrency-safe. Multiple goroutines can call Call/Notify/Respond
//     concurrently without synchronisation by the caller.

// ── envelope ──
//
// JSONRPCEnvelope is the wire-level shape for any direction. Codex's own
// ser/de uses an untagged enum; we accept the superset (id, method, params,
// result, error) and dispatch based on which fields are present.

// JSONRPCEnvelope is a permissive parse of any JSON-RPC message.
//
// Disambiguation rules (in order):
//   1. id present + result OR error present → Response (success or error)
//   2. id present + method present          → Server-initiated Request
//   3. method present, no id                → Notification
//   4. anything else                        → malformed, dropped with warning
//
// We keep raw json.RawMessage for ID so we can echo it back without losing
// integer-vs-string fidelity (codex's protocol allows both per JSON-RPC).
type JSONRPCEnvelope struct {
	ID     json.RawMessage  `json:"id,omitempty"`
	Method string           `json:"method,omitempty"`
	Params json.RawMessage  `json:"params,omitempty"`
	Result json.RawMessage  `json:"result,omitempty"`
	Error  *JSONRPCErrorObj `json:"error,omitempty"`
}

// JSONRPCErrorObj carries server-side error detail.
type JSONRPCErrorObj struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Error formats the JSON-RPC error for logging / wrapping.
func (e *JSONRPCErrorObj) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message)
}

// ── handlers ──

// JSONRPCRequestHandler answers an incoming server request. Returning a
// non-nil error causes the client to send a JSON-RPC error response with
// code -32603 (internal error); typed JSON-RPC errors should be returned by
// constructing a *JSONRPCErrorObj wrapper.
type JSONRPCRequestHandler func(params json.RawMessage) (any, error)

// JSONRPCNotificationHandler consumes a fire-and-forget notification.
// Returning is its own ack; errors are logged but not surfaced.
type JSONRPCNotificationHandler func(params json.RawMessage)

// ── pending response slot ──

// jsonrpcPending is a single in-flight client request awaiting reply.
type jsonrpcPending struct {
	resp chan jsonrpcReply
}

// jsonrpcReply carries either a successful result or a server error.
type jsonrpcReply struct {
	result json.RawMessage
	err    *JSONRPCErrorObj
}

// ── client ──

// CodexJSONRPCClient is a minimal, bidirectional JSON-RPC 2.0 client.
//
// Lifecycle:
//
//	c, err := NewCodexJSONRPCClient(ctx, reader, writer)
//	c.OnNotification("turn/started", func(params json.RawMessage) { ... })
//	c.OnRequest("item/commandExecution/requestApproval",
//	    func(params json.RawMessage) (any, error) { return &resp, nil })
//	go c.Run() // blocks until ctx cancel or transport EOF
//	res, err := c.Call(ctx, "thread/start", &CodexThreadStartParams{...})
//	c.Notify("initialized", nil)
//	c.Close()
//
// The client never logs to stderr by itself; embedders can attach a logger
// via WithLogger if they want telemetry on dropped messages or unmatched
// responses.
type CodexJSONRPCClient struct {
	ctx    context.Context
	cancel context.CancelFunc

	r io.Reader
	w io.Writer

	// id is monotonically incremented for outbound Calls.
	id atomic.Int64

	// outbound is the bounded send queue (drop-oldest on overflow). Sized
	// at 256 to absorb bursts without backpressuring the call sites; the
	// writer goroutine drains it serially so the underlying io.Writer is
	// only touched from one goroutine.
	outbound chan []byte

	// drops counts how many outbound messages were discarded due to
	// overflow. Sampled by tests; production code can read it via Stats.
	drops atomic.Int64

	// pending maps outstanding Call ids to their reply channels. Keyed by
	// the integer id we minted (not the wire form) so we don't have to
	// re-marshal to compare.
	pendingMu sync.Mutex
	pending   map[int64]*jsonrpcPending

	// reqHandlers / notifHandlers are dispatch tables registered before
	// Run() starts. Concurrent registration after Run() is allowed but
	// subject to a brief mutex.
	handlersMu     sync.RWMutex
	reqHandlers    map[string]JSONRPCRequestHandler
	notifHandlers  map[string]JSONRPCNotificationHandler
	defaultRequest JSONRPCRequestHandler
	defaultNotif   JSONRPCNotificationHandler

	// logger is optional — left nil by default to avoid noisy production
	// output; tests can install a verbose logger.
	logger func(format string, args ...any)

	// closed is set after Close() to short-circuit further sends.
	closedFlag atomic.Bool
	// done is closed after the read loop exits, so callers can wait for
	// the client to fully drain.
	done chan struct{}
}

// CodexJSONRPCClientOption configures a CodexJSONRPCClient.
type CodexJSONRPCClientOption func(*CodexJSONRPCClient)

// WithJSONRPCLogger attaches a logger for diagnostics (drops, malformed
// frames, unmatched responses). The logger may be invoked from the read or
// write goroutines.
func WithJSONRPCLogger(fn func(format string, args ...any)) CodexJSONRPCClientOption {
	return func(c *CodexJSONRPCClient) {
		if fn != nil {
			c.logger = fn
		}
	}
}

// NewCodexJSONRPCClient constructs a client over a duplex byte stream. The
// client does not own the reader/writer — close them externally to end the
// session — but it does Cancel its internal ctx on Close().
func NewCodexJSONRPCClient(ctx context.Context, r io.Reader, w io.Writer, opts ...CodexJSONRPCClientOption) *CodexJSONRPCClient {
	if ctx == nil {
		ctx = context.Background()
	}
	cctx, cancel := context.WithCancel(ctx)
	c := &CodexJSONRPCClient{
		ctx:           cctx,
		cancel:        cancel,
		r:             r,
		w:             w,
		outbound:      make(chan []byte, 256),
		pending:       make(map[int64]*jsonrpcPending),
		reqHandlers:   make(map[string]JSONRPCRequestHandler),
		notifHandlers: make(map[string]JSONRPCNotificationHandler),
		done:          make(chan struct{}),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// OnRequest registers a handler for incoming server-initiated requests
// matching method. Re-registering replaces.
func (c *CodexJSONRPCClient) OnRequest(method string, h JSONRPCRequestHandler) {
	c.handlersMu.Lock()
	defer c.handlersMu.Unlock()
	c.reqHandlers[method] = h
}

// OnNotification registers a handler for incoming server-initiated
// notifications matching method.
func (c *CodexJSONRPCClient) OnNotification(method string, h JSONRPCNotificationHandler) {
	c.handlersMu.Lock()
	defer c.handlersMu.Unlock()
	c.notifHandlers[method] = h
}

// SetDefaultRequestHandler installs a fallback handler invoked when no
// per-method handler is registered. Useful for the codex backend: it can
// log unsupported approvals and decline by default.
func (c *CodexJSONRPCClient) SetDefaultRequestHandler(h JSONRPCRequestHandler) {
	c.handlersMu.Lock()
	defer c.handlersMu.Unlock()
	c.defaultRequest = h
}

// SetDefaultNotificationHandler installs a fallback handler for
// notifications without a registered handler.
func (c *CodexJSONRPCClient) SetDefaultNotificationHandler(h JSONRPCNotificationHandler) {
	c.handlersMu.Lock()
	defer c.handlersMu.Unlock()
	c.defaultNotif = h
}

// Stats exposes a snapshot of internal counters (drops + pending count) for
// tests and metrics.
type CodexJSONRPCClientStats struct {
	OutboundDrops int64
	PendingCalls  int
}

// Stats returns counters useful for tests / metrics.
func (c *CodexJSONRPCClient) Stats() CodexJSONRPCClientStats {
	c.pendingMu.Lock()
	pending := len(c.pending)
	c.pendingMu.Unlock()
	return CodexJSONRPCClientStats{
		OutboundDrops: c.drops.Load(),
		PendingCalls:  pending,
	}
}

// Notify sends a fire-and-forget notification.
func (c *CodexJSONRPCClient) Notify(method string, params any) error {
	if c.closedFlag.Load() {
		return errors.New("jsonrpc client closed")
	}
	env := JSONRPCEnvelope{Method: method}
	if params != nil {
		raw, err := marshalParams(params)
		if err != nil {
			return fmt.Errorf("marshal notification params: %w", err)
		}
		env.Params = raw
	}
	buf, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal notification: %w", err)
	}
	c.enqueue(buf)
	return nil
}

// Call sends a request and blocks until a response is received, ctx is
// cancelled, or the transport closes. Returns the raw result bytes (caller
// unmarshals into the appropriate response struct from codex_protocol.go).
//
// Round 5: per-call duration is recorded in codex_metrics if the call
// completes (success or app-level error). Transport-level failures
// (closed client, ctx cancel, EOF) skip the histogram so we don't
// poison the latency distribution with sub-millisecond stub samples.
func (c *CodexJSONRPCClient) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if c.closedFlag.Load() {
		return nil, errors.New("jsonrpc client closed")
	}
	if ctx == nil {
		ctx = c.ctx
	}
	startedAt := time.Now()
	id := c.id.Add(1)
	idJSON, err := json.Marshal(id)
	if err != nil {
		return nil, fmt.Errorf("marshal id: %w", err)
	}
	env := JSONRPCEnvelope{
		ID:     idJSON,
		Method: method,
	}
	if params != nil {
		raw, err := marshalParams(params)
		if err != nil {
			return nil, fmt.Errorf("marshal params: %w", err)
		}
		env.Params = raw
	}
	buf, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	// Register the pending slot *before* enqueuing the bytes so a fast
	// reader can't deliver the response before we're ready for it.
	pending := &jsonrpcPending{resp: make(chan jsonrpcReply, 1)}
	c.pendingMu.Lock()
	c.pending[id] = pending
	c.pendingMu.Unlock()

	cleanup := func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
	}

	c.enqueue(buf)

	select {
	case reply := <-pending.resp:
		cleanup()
		recordCodexJSONRPCDuration(time.Since(startedAt))
		if reply.err != nil {
			return nil, reply.err
		}
		return reply.result, nil
	case <-ctx.Done():
		cleanup()
		return nil, ctx.Err()
	case <-c.ctx.Done():
		cleanup()
		return nil, errors.New("jsonrpc client shutdown")
	}
}

// Respond sends a successful response to an incoming server request. The
// reqID is whatever shape (int / string) the server used; we echo it back
// verbatim. Round 3's request handler typically returns the response body;
// Respond is exposed for callers that need to reply asynchronously after
// the handler has returned (e.g. waiting on user approval via Telegram).
func (c *CodexJSONRPCClient) Respond(reqID json.RawMessage, result any) error {
	if c.closedFlag.Load() {
		return errors.New("jsonrpc client closed")
	}
	raw, err := marshalParams(result)
	if err != nil {
		return fmt.Errorf("marshal response: %w", err)
	}
	env := JSONRPCEnvelope{
		ID:     reqID,
		Result: raw,
	}
	buf, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal response envelope: %w", err)
	}
	c.enqueue(buf)
	return nil
}

// RespondError sends an error response to an incoming server request.
func (c *CodexJSONRPCClient) RespondError(reqID json.RawMessage, code int, message string, data any) error {
	if c.closedFlag.Load() {
		return errors.New("jsonrpc client closed")
	}
	errObj := &JSONRPCErrorObj{Code: code, Message: message}
	if data != nil {
		raw, err := marshalParams(data)
		if err != nil {
			return fmt.Errorf("marshal error data: %w", err)
		}
		errObj.Data = raw
	}
	env := JSONRPCEnvelope{
		ID:    reqID,
		Error: errObj,
	}
	buf, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal error envelope: %w", err)
	}
	c.enqueue(buf)
	return nil
}

// Run drives the I/O goroutines and blocks until ctx cancels or the reader
// hits EOF. Callers typically `go c.Run()` and use c.Done() to wait.
func (c *CodexJSONRPCClient) Run() {
	var wg sync.WaitGroup
	wg.Add(2)
	writerDone := make(chan struct{})
	go func() {
		defer wg.Done()
		defer close(writerDone)
		c.writeLoop()
	}()
	go func() {
		defer wg.Done()
		c.readLoop()
		// reader stopped — cancel our ctx so the writer drains and exits
		c.cancel()
	}()
	wg.Wait()
	// Signal external waiters that we've fully shut down. After done is
	// closed, any in-flight Call() will observe ctx.Done().
	close(c.done)
	// Fail any pending callers that didn't get a response before EOF.
	c.pendingMu.Lock()
	for id, p := range c.pending {
		select {
		case p.resp <- jsonrpcReply{err: &JSONRPCErrorObj{
			Code: CodexJSONRPCErrInternalError, Message: "transport closed before response",
		}}:
		default:
		}
		delete(c.pending, id)
	}
	c.pendingMu.Unlock()
}

// Done returns a channel closed after Run() has finished. Useful for tests
// that want to block until both I/O goroutines exit.
func (c *CodexJSONRPCClient) Done() <-chan struct{} { return c.done }

// Close cancels the context and signals the loops to exit. Safe to call
// multiple times.
func (c *CodexJSONRPCClient) Close() {
	if c.closedFlag.Swap(true) {
		return
	}
	c.cancel()
}

// enqueue appends a single framed message to the outbound queue. If the
// queue is full, the OLDEST message is dropped to make room for the new
// one — that bounds memory usage at the cost of losing in-flight requests
// in pathological backpressure cases. The drops counter is bumped so tests
// can detect.
func (c *CodexJSONRPCClient) enqueue(buf []byte) {
	// Append a newline so the writer produces NDJSON without an extra
	// allocation in the hot path.
	framed := make([]byte, len(buf)+1)
	copy(framed, buf)
	framed[len(buf)] = '\n'

	for {
		select {
		case c.outbound <- framed:
			return
		case <-c.ctx.Done():
			return
		default:
			// Full — drop the oldest message and retry. We use a
			// non-blocking pop so a concurrent enqueue doesn't deadlock
			// us; if the buffer drains before we get a chance, the
			// receive simply fails and we go back to the send path.
			select {
			case <-c.outbound:
				c.drops.Add(1)
				if c.logger != nil {
					c.logger("jsonrpc: outbound queue overflow, dropped oldest")
				}
			default:
				// Someone else just drained — try again.
			}
		}
	}
}

// writeLoop drains the outbound queue and writes framed bytes to the
// underlying writer. Exits when ctx cancels or the writer returns an error.
func (c *CodexJSONRPCClient) writeLoop() {
	for {
		select {
		case <-c.ctx.Done():
			// Best-effort flush of anything already queued before
			// returning, since the reader goroutine may still want to
			// observe a final response we wrote in response to its last
			// frame. Bounded by the queue capacity so we can't loop forever.
			for i := 0; i < cap(c.outbound); i++ {
				select {
				case buf := <-c.outbound:
					_, _ = c.w.Write(buf)
				default:
					goto flushAndExit
				}
			}
		flushAndExit:
			// If the underlying writer is a buffered type (bufio.Writer or
			// any wrapper exposing Flush), make sure queued bytes actually
			// hit the wire before we return. Today c.w is an os.File / pipe
			// which doesn't need this — but adding a buffered writer in the
			// future shouldn't silently drop frames.
			if f, ok := c.w.(interface{ Flush() error }); ok {
				_ = f.Flush()
			}
			return
		case buf := <-c.outbound:
			if _, err := c.w.Write(buf); err != nil {
				if c.logger != nil {
					c.logger("jsonrpc: write error: %v", err)
				}
				return
			}
		}
	}
}

// readLoop reads NDJSON frames from the underlying reader, dispatches each
// frame to the appropriate handler, and exits on EOF or unrecoverable
// parse error.
//
// We use bufio.Scanner with a generously sized buffer (1 MiB) to handle
// occasional large items (e.g. a CommandExecution.aggregatedOutput holding
// kilobytes of stdout). If a single frame ever exceeds 1 MiB, scanning
// returns an error and we exit — the alternative (silently dropping the
// frame) would corrupt the protocol state.
func (c *CodexJSONRPCClient) readLoop() {
	scanner := bufio.NewScanner(c.r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		// Bail early if the embedder cancelled.
		select {
		case <-c.ctx.Done():
			return
		default:
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		// Copy the bytes — Scanner.Bytes is only valid until the next Scan.
		buf := make([]byte, len(line))
		copy(buf, line)
		c.dispatch(buf)
	}
	if err := scanner.Err(); err != nil && c.logger != nil {
		c.logger("jsonrpc: read error: %v", err)
	}
}

// dispatch parses one envelope and routes to handler / pending Call.
func (c *CodexJSONRPCClient) dispatch(line []byte) {
	var env JSONRPCEnvelope
	if err := json.Unmarshal(line, &env); err != nil {
		if c.logger != nil {
			c.logger("jsonrpc: dropping malformed frame: %v", err)
		}
		return
	}

	hasID := len(env.ID) > 0 && string(env.ID) != "null"
	hasMethod := env.Method != ""
	hasResult := len(env.Result) > 0 || env.Error != nil

	switch {
	case hasID && hasResult && !hasMethod:
		c.deliverResponse(env)
	case hasID && hasMethod:
		// Server-initiated request (must reply).
		go c.handleServerRequest(env)
	case hasMethod:
		// Notification.
		go c.handleNotification(env)
	default:
		if c.logger != nil {
			c.logger("jsonrpc: ignoring frame with no id/method/result")
		}
	}
}

func (c *CodexJSONRPCClient) deliverResponse(env JSONRPCEnvelope) {
	// We minted IDs as integers; codex echoes them verbatim.
	var id int64
	if err := json.Unmarshal(env.ID, &id); err != nil {
		if c.logger != nil {
			c.logger("jsonrpc: response with non-integer id %s", string(env.ID))
		}
		return
	}
	c.pendingMu.Lock()
	pending, ok := c.pending[id]
	if ok {
		delete(c.pending, id)
	}
	c.pendingMu.Unlock()
	if !ok {
		if c.logger != nil {
			c.logger("jsonrpc: unmatched response for id %d", id)
		}
		return
	}
	reply := jsonrpcReply{}
	if env.Error != nil {
		reply.err = env.Error
	} else {
		reply.result = env.Result
	}
	// Slot is buffered (size 1); this never blocks.
	pending.resp <- reply
}

func (c *CodexJSONRPCClient) handleServerRequest(env JSONRPCEnvelope) {
	c.handlersMu.RLock()
	h, ok := c.reqHandlers[env.Method]
	if !ok {
		h = c.defaultRequest
	}
	c.handlersMu.RUnlock()
	if h == nil {
		_ = c.RespondError(env.ID, CodexJSONRPCErrMethodNotFound,
			fmt.Sprintf("no handler for %s", env.Method), nil)
		return
	}
	result, err := h(env.Params)
	if err != nil {
		// Allow handlers to surface a typed JSONRPCErrorObj.
		var jerr *JSONRPCErrorObj
		if errors.As(err, &jerr) {
			_ = c.RespondError(env.ID, jerr.Code, jerr.Message, jerr.Data)
			return
		}
		_ = c.RespondError(env.ID, CodexJSONRPCErrInternalError, err.Error(), nil)
		return
	}
	if err := c.Respond(env.ID, result); err != nil && c.logger != nil {
		c.logger("jsonrpc: respond error: %v", err)
	}
}

func (c *CodexJSONRPCClient) handleNotification(env JSONRPCEnvelope) {
	c.handlersMu.RLock()
	h, ok := c.notifHandlers[env.Method]
	if !ok {
		h = c.defaultNotif
	}
	c.handlersMu.RUnlock()
	if h == nil {
		// Silently ignore unknown notifications — codex emits many we
		// don't care about (e.g. mcpServer/* status updates).
		return
	}
	h(env.Params)
}

// marshalParams handles the "already-encoded" optimisation: if the caller
// passes a json.RawMessage we skip the encode round-trip. Same for raw
// []byte (assumed to be a valid JSON value).
func marshalParams(v any) (json.RawMessage, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case json.RawMessage:
		return x, nil
	case []byte:
		return json.RawMessage(x), nil
	default:
		return json.Marshal(v)
	}
}
