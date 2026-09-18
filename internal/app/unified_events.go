package app

import "encoding/json"

// ── Unified Event Bus (Round 1 skeleton) ──
//
// This file defines the backend-agnostic event envelope produced by every
// Backend implementation and (eventually) consumed by the SSE / WebSocket /
// Telegram bridges. The schema is intentionally close to codex's
// thread/turn/item model because that's the more typed of the two
// protocols; CC's stream-json gets translated into it.
//
// Round 1 only declares the schema; the CC bridge still emits raw
// stream-json events directly into the existing sseBroadcaster. Round 2/3
// wire codex through this envelope, then refactor the CC bridge to
// translate stream-json into UnifiedEvent so the SSE layer has a single
// consumer-side schema.

// UnifiedEventKind tags the high-level kind of a unified event.
type UnifiedEventKind string

const (
	// UEvtTurnStarted fires when a backend begins processing a user turn
	// (CC: stream-json system/init for the turn; codex: turn/started notif).
	UEvtTurnStarted UnifiedEventKind = "turn_started"
	// UEvtTurnCompleted fires when a turn finishes — successfully or with
	// error. Payload carries the final status and aggregate cost/usage.
	UEvtTurnCompleted UnifiedEventKind = "turn_completed"
	// UEvtItemStarted fires when a new item appears inside the turn. The
	// ItemKind field discriminates agent_message / tool_call / etc.
	UEvtItemStarted UnifiedEventKind = "item_started"
	// UEvtItemDelta carries an incremental update for an in-flight item:
	// streaming text, tool output line, plan summary, etc.
	UEvtItemDelta UnifiedEventKind = "item_delta"
	// UEvtItemCompleted fires when an item reaches its terminal state.
	// Payload carries the item's final content / result / error.
	UEvtItemCompleted UnifiedEventKind = "item_completed"
	// UEvtApproval is server-initiated: the backend is asking weiran (and
	// through it the human or hook chain) to approve a tool call. CC routes
	// these through can_use_tool; codex through */requestApproval.
	UEvtApproval UnifiedEventKind = "approval"
	// UEvtBackendError carries a transport-level error from the backend
	// (JSON-RPC parse error, stream-json corruption, unexpected exit).
	UEvtBackendError UnifiedEventKind = "backend_error"
)

// UnifiedItemKind tags the kind of an item inside a turn.
type UnifiedItemKind string

const (
	// UItemAgentMessage is text the model is producing (assistant text block).
	UItemAgentMessage UnifiedItemKind = "agent_message"
	// UItemCommandExec is a shell command execution (CC: Bash tool_use;
	// codex: exec_command item).
	UItemCommandExec UnifiedItemKind = "command_exec"
	// UItemToolCall is any other tool invocation (Read, Edit, Grep, MCP, …).
	UItemToolCall UnifiedItemKind = "tool_call"
	// UItemFileChange is a file-mutating tool (Write/Edit/NotebookEdit) —
	// surfaced as a separate kind so the UI can render diffs.
	UItemFileChange UnifiedItemKind = "file_change"
	// UItemReasoning is the model's internal chain-of-thought text (CC:
	// thinking content block; codex: reasoning item).
	UItemReasoning UnifiedItemKind = "reasoning"
)

// UnifiedEvent is the backend-agnostic event envelope. Backends emit a
// stream of these into a per-session channel; consumers (SSE bridge,
// Telegram relay, IPC peers) read from that channel and never see the
// underlying stream-json or JSON-RPC payload.
//
// Round 1 places this here as a forward-compatible shape; today's CC
// bridge does NOT yet emit through it. The Raw field exists so the
// transitional period (Rounds 2-5) can keep replaying raw stream-json
// to the existing SSE schema while the structured Payload migration
// happens in parallel.
type UnifiedEvent struct {
	// Kind is the high-level event kind.
	Kind UnifiedEventKind `json:"kind"`
	// TurnID is the backend-native turn id. Empty for events outside a turn.
	TurnID string `json:"turn_id,omitempty"`
	// ItemID identifies the item this event belongs to. Empty for turn-level
	// events (TurnStarted/TurnCompleted) and for backend-level errors.
	ItemID string `json:"item_id,omitempty"`
	// ItemKind tags the kind of item for ItemStarted events. ItemDelta and
	// ItemCompleted inherit the kind from their matching ItemStarted; the
	// SSE bridge keeps a per-turn id→kind map if it needs to emit it
	// downstream.
	ItemKind UnifiedItemKind `json:"item_kind,omitempty"`
	// DeltaType discriminates ItemDelta payloads: "text" (streaming model
	// text), "output" (tool stdout/stderr), "summary" (plan/diff summary),
	// "plan" (ExitPlanMode plan body).
	DeltaType string `json:"delta_type,omitempty"`
	// Payload is the kind-specific structured data — see UnifiedTurnPayload,
	// UnifiedItemPayload, UnifiedDeltaPayload, UnifiedApproval below for
	// the shapes consumers should expect.
	Payload json.RawMessage `json:"payload,omitempty"`
	// Raw preserves the original backend message verbatim. The SSE bridge
	// uses this during the gradual migration to keep emitting the legacy
	// stream-json schema to the Web UI even after the internal pipeline
	// switches to UnifiedEvent. Removed once both backends emit through
	// Payload end-to-end.
	Raw json.RawMessage `json:"raw,omitempty"`
}

// UnifiedTurnPayload is the Payload shape for UEvtTurnStarted / UEvtTurnCompleted.
type UnifiedTurnPayload struct {
	// Status is "running" for TurnStarted, "ok" / "error" / "cancelled" for
	// TurnCompleted.
	Status string `json:"status"`
	// Error carries a short error message when Status="error".
	Error string `json:"error,omitempty"`
	// CostUSD is the aggregate spend for this turn (only set on Completed).
	CostUSD float64 `json:"cost_usd,omitempty"`
	// InputTokens / OutputTokens are aggregated usage (only set on Completed).
	InputTokens  int64 `json:"input_tokens,omitempty"`
	OutputTokens int64 `json:"output_tokens,omitempty"`
}

// UnifiedItemPayload is the Payload shape for UEvtItemStarted / UEvtItemCompleted.
type UnifiedItemPayload struct {
	// Name is the tool name for tool_call/file_change/command_exec items
	// (e.g. "Bash", "Edit"). Empty for agent_message / reasoning.
	Name string `json:"name,omitempty"`
	// Input is the tool's input arguments as a JSON object (only set on
	// ItemStarted for tool kinds).
	Input json.RawMessage `json:"input,omitempty"`
	// Result is the final result text / structured output (only set on
	// ItemCompleted).
	Result json.RawMessage `json:"result,omitempty"`
	// IsError is true when Status reflects a tool error (only set on
	// ItemCompleted for tool kinds).
	IsError bool `json:"is_error,omitempty"`
}

// UnifiedDeltaPayload is the Payload shape for UEvtItemDelta. Different
// DeltaType values populate different fields.
type UnifiedDeltaPayload struct {
	// Text is the incremental text for DeltaType="text" / "summary" / "plan".
	Text string `json:"text,omitempty"`
	// Output is one stdout/stderr line for DeltaType="output". Stream is
	// "stdout" or "stderr".
	Output string `json:"output,omitempty"`
	Stream string `json:"stream,omitempty"`
}

// UnifiedApproval is the Payload shape for UEvtApproval. The backend keeps
// the in-flight approval blocked until the consumer calls
// Backend.SendPermissionDecision with the matching RequestID.
type UnifiedApproval struct {
	// RequestID identifies this approval — must be echoed back to
	// Backend.SendPermissionDecision.
	RequestID string `json:"request_id"`
	// ToolName is the tool whose invocation needs approval.
	ToolName string `json:"tool_name,omitempty"`
	// ToolUseID is the upstream tool_use id (for UI dedupe across reloads).
	ToolUseID string `json:"tool_use_id,omitempty"`
	// Input carries the tool's input so the consumer can show a preview.
	Input json.RawMessage `json:"input,omitempty"`
	// Subtype distinguishes approval shapes: "auq" (AskUserQuestion),
	// "permission" (generic Yes/No), "plan_approval" (ExitPlanMode).
	Subtype string `json:"subtype,omitempty"`
}
