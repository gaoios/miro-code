package app

import (
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// ── Codex backend metrics (Round 5) ──
//
// Lightweight in-process counters + a coarse latency histogram, exposed
// via GET /api/codex/metrics so operators have something to point at when
// asking "is the codex backend healthy?". Deliberately tiny — no
// prometheus dep, no exporter — but the JSON shape is stable so we can
// later proxy it into n9e / grafana.
//
// Counters use sync/atomic so increments are lock-free on the hot path
// (jsonrpc Call, approval handler). The latency buckets use a coarse
// power-of-2 ladder (50ms / 200ms / 1s / 5s / 30s / +inf) which is more
// than precise enough for "is codex slow today" eyeballing.
//
// Anything that needs better fidelity should land a real exporter; this
// file is intentionally the smallest thing that lets us answer:
//
//   - how many sessions ran on each backend?
//   - how many codex handshakes failed?
//   - how often does the hook chain auto-decide approvals?
//   - what does codex round-trip latency look like in p50/p95?
//
// All counters reset only on process restart. The HTTP endpoint also
// reports server uptime so consumers can compute rates client-side.

var (
	// Per-backend session counters. backendKindMap is keyed by
	// BackendKind ("cc" | "codex") so we don't have to enumerate.
	backendSessionCount sync.Map // map[BackendKind]*int64

	// Codex handshake counters (initialize → initialized → thread/start).
	codexHandshakeOKTotal       atomic.Int64
	codexHandshakeFailuresTotal atomic.Int64

	// Codex approval decisions, keyed by decision string.
	codexApprovalAllowTotal   atomic.Int64
	codexApprovalDenyTotal    atomic.Int64
	codexApprovalAskTotal     atomic.Int64
	codexApprovalTimeoutTotal atomic.Int64
	codexApprovalDefaultTotal atomic.Int64 // no rule matched

	// JSON-RPC latency buckets (Call duration in milliseconds). Buckets
	// are upper-bound inclusive: a 75ms call lands in bucket "200ms".
	codexJSONRPCBuckets = [...]int64{50, 200, 1000, 5000, 30000}
	codexJSONRPCCounts  [6]atomic.Int64 // 5 buckets + +inf
	codexJSONRPCTotal   atomic.Int64
	codexJSONRPCSumMs   atomic.Int64

	// Event bus drops. Bumped from emit() when the bounded events channel
	// (capacity 256) is full; we drop the oldest queued event and continue.
	// A non-zero count usually means a consumer (the bridge goroutine) is
	// stalled or hasn't attached yet — useful as an "is the bridge stuck?"
	// signal independent of any latency metric.
	codexEventsDroppedTotal atomic.Int64

	// Server-process start time for uptime reporting.
	codexMetricsStart = time.Now()
)

// recordBackendStart bumps the session counter for kind. Called once per
// successful spawn from createSessionWithOpts.
func recordBackendStart(kind BackendKind) {
	v, _ := backendSessionCount.LoadOrStore(kind, new(int64))
	atomic.AddInt64(v.(*int64), 1)
}

// recordCodexHandshake records the outcome of a handshake (true=success).
func recordCodexHandshake(ok bool) {
	if ok {
		codexHandshakeOKTotal.Add(1)
	} else {
		codexHandshakeFailuresTotal.Add(1)
	}
}

// recordCodexApprovalDecision bumps the appropriate counter for a hook
// chain decision. Unknown decisions land in the "default" bucket so we
// don't silently lose data.
func recordCodexApprovalDecision(decision string) {
	switch decision {
	case "allow":
		codexApprovalAllowTotal.Add(1)
	case "deny":
		codexApprovalDenyTotal.Add(1)
	case "ask":
		codexApprovalAskTotal.Add(1)
	case "timeout":
		codexApprovalTimeoutTotal.Add(1)
	default:
		codexApprovalDefaultTotal.Add(1)
	}
}

// recordCodexJSONRPCDuration records a Call duration. Hot path — keep
// it branch-free past the bucket lookup.
func recordCodexJSONRPCDuration(d time.Duration) {
	ms := d.Milliseconds()
	codexJSONRPCTotal.Add(1)
	codexJSONRPCSumMs.Add(ms)
	for i, upper := range codexJSONRPCBuckets {
		if ms <= upper {
			codexJSONRPCCounts[i].Add(1)
			return
		}
	}
	codexJSONRPCCounts[len(codexJSONRPCBuckets)].Add(1)
}

// codexMetricsSnapshot returns a JSON-serializable view of every counter.
// Stable enough to consume from a dashboard or alert rule.
type codexMetricsSnapshot struct {
	UptimeSeconds      int64                 `json:"uptime_seconds"`
	Backend            map[string]int64      `json:"backend_sessions_total"`
	Handshake          codexHandshakeMetrics `json:"codex_handshake"`
	Approvals          codexApprovalMetrics  `json:"codex_approvals"`
	JSONRPC            codexJSONRPCMetrics   `json:"codex_jsonrpc"`
	EventsDroppedTotal int64                 `json:"codex_events_dropped_total"`
	Generated          string                `json:"generated_at"`
}

type codexHandshakeMetrics struct {
	OKTotal       int64 `json:"ok_total"`
	FailuresTotal int64 `json:"failures_total"`
}

type codexApprovalMetrics struct {
	AllowTotal   int64 `json:"allow_total"`
	DenyTotal    int64 `json:"deny_total"`
	AskTotal     int64 `json:"ask_total"`
	TimeoutTotal int64 `json:"timeout_total"`
	DefaultTotal int64 `json:"default_total"`
}

type codexJSONRPCMetrics struct {
	CallsTotal      int64                     `json:"calls_total"`
	SumMillis       int64                     `json:"sum_ms"`
	MeanMillis      float64                   `json:"mean_ms"`
	BucketsMillisLE map[string]int64          `json:"buckets_le_ms"`
}

func snapshotCodexMetrics() codexMetricsSnapshot {
	out := codexMetricsSnapshot{
		UptimeSeconds: int64(time.Since(codexMetricsStart).Seconds()),
		Backend:       map[string]int64{},
		Handshake: codexHandshakeMetrics{
			OKTotal:       codexHandshakeOKTotal.Load(),
			FailuresTotal: codexHandshakeFailuresTotal.Load(),
		},
		Approvals: codexApprovalMetrics{
			AllowTotal:   codexApprovalAllowTotal.Load(),
			DenyTotal:    codexApprovalDenyTotal.Load(),
			AskTotal:     codexApprovalAskTotal.Load(),
			TimeoutTotal: codexApprovalTimeoutTotal.Load(),
			DefaultTotal: codexApprovalDefaultTotal.Load(),
		},
		EventsDroppedTotal: codexEventsDroppedTotal.Load(),
		Generated:          time.Now().UTC().Format(time.RFC3339),
	}
	backendSessionCount.Range(func(k, v any) bool {
		kind, _ := k.(BackendKind)
		ptr, _ := v.(*int64)
		if ptr != nil {
			out.Backend[string(kind)] = atomic.LoadInt64(ptr)
		}
		return true
	})

	calls := codexJSONRPCTotal.Load()
	sum := codexJSONRPCSumMs.Load()
	mean := 0.0
	if calls > 0 {
		mean = float64(sum) / float64(calls)
	}
	buckets := map[string]int64{}
	for i, upper := range codexJSONRPCBuckets {
		buckets[formatLE(upper)] = codexJSONRPCCounts[i].Load()
	}
	buckets["+Inf"] = codexJSONRPCCounts[len(codexJSONRPCBuckets)].Load()
	out.JSONRPC = codexJSONRPCMetrics{
		CallsTotal:      calls,
		SumMillis:       sum,
		MeanMillis:      mean,
		BucketsMillisLE: buckets,
	}
	return out
}

func formatLE(ms int64) string {
	switch ms {
	case 50:
		return "50"
	case 200:
		return "200"
	case 1000:
		return "1000"
	case 5000:
		return "5000"
	case 30000:
		return "30000"
	default:
		return ""
	}
}

// handleCodexMetrics is registered on /api/codex/metrics. Public (no
// auth) so heartbeat hooks and prom proxies can scrape without
// shipping a token.
func handleCodexMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(snapshotCodexMetrics())
}
