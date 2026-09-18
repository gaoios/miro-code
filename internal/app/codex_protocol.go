package app

import "encoding/json"

// ── Codex app-server JSON-RPC protocol (Round 2) ──
//
// This file holds the Go translation of the typed schema codex's
// `app-server-protocol` crate exposes (codex-rs/app-server-protocol/src/protocol/).
// The wire format is JSON-RPC 2.0 with the "jsonrpc": "2.0" header omitted, framed
// as newline-delimited JSON (NDJSON) on stdio. See ~/code/codex/codex-rs/app-server/
// README.md for the authoritative spec.
//
// We don't try to mirror every field — only the subset weiran's codex backend
// (Round 3) needs to drive a thread end-to-end:
//
//   - Lifecycle: initialize / initialized / thread/start / thread/resume /
//     thread/unsubscribe / thread/loaded/list
//   - Conversation: turn/start / turn/interrupt
//   - Streaming notifications: thread/started, thread/status/changed,
//     turn/started, turn/completed, item/started, item/completed,
//     item/agentMessage/delta, item/reasoning/summaryTextDelta,
//     item/commandExecution/outputDelta, turn/diff/updated, turn/plan/updated,
//     thread/tokenUsage/updated, thread/closed, error
//   - Server-initiated approvals: item/commandExecution/requestApproval,
//     item/fileChange/requestApproval, item/permissions/requestApproval
//
// Naming follows Go convention (PascalCase) and the json tags follow codex's
// camelCase wire serialization (Rust serde rename_all = "camelCase").
//
// Mapping notes for Round 3 (codex_backend.go) translate Codex notifications
// into UnifiedEvent (defined in unified_events.go):
//
//   thread/started        → backend records thread.id, no UnifiedEvent
//   turn/started          → UEvtTurnStarted   (TurnID = turn.id)
//   turn/completed        → UEvtTurnCompleted (Status from TurnStatus)
//   item/started          → UEvtItemStarted   (ItemKind from ThreadItem.Type)
//   item/agentMessage/    → UEvtItemDelta     (DeltaType = "text")
//      delta
//   item/reasoning/       → UEvtItemDelta     (DeltaType = "text",
//      summaryTextDelta                       ItemKind = UItemReasoning)
//   item/commandExecution/→ UEvtItemDelta     (DeltaType = "output")
//      outputDelta
//   item/completed        → UEvtItemCompleted (final ThreadItem in payload)
//   error                 → UEvtBackendError
//   */requestApproval     → UEvtApproval      (Subtype based on method)
//
// Updated: 2026-04-27, sourced from codex-cli 0.125.0 protocol crate.

// ── JSON-RPC method names ──
//
// String constants for every method this backend cares about. Using constants
// keeps the spelling (and the punctuation — note "thread/loaded/list", not
// "thread/loadedList") in one place. Round 3's backend dispatches on these.

const (
	// Client → Server requests (we issue these and wait for responses).
	MethodInitialize         = "initialize"
	MethodThreadStart        = "thread/start"
	MethodThreadResume       = "thread/resume"
	MethodThreadUnsubscribe  = "thread/unsubscribe"
	MethodThreadLoadedList   = "thread/loaded/list"
	MethodTurnStart          = "turn/start"
	MethodTurnInterrupt      = "turn/interrupt"
	MethodThreadInjectItems  = "thread/inject_items"

	// Client → Server notifications (no response).
	MethodInitialized = "initialized"

	// Server → Client notifications (we receive these, no response).
	NotifThreadStarted        = "thread/started"
	NotifThreadStatusChanged  = "thread/status/changed"
	NotifThreadClosed         = "thread/closed"
	NotifThreadNameUpdated    = "thread/name/updated"
	NotifThreadTokenUsage     = "thread/tokenUsage/updated"
	NotifTurnStarted          = "turn/started"
	NotifTurnCompleted        = "turn/completed"
	NotifTurnDiffUpdated      = "turn/diff/updated"
	NotifTurnPlanUpdated      = "turn/plan/updated"
	NotifItemStarted          = "item/started"
	NotifItemCompleted        = "item/completed"
	NotifAgentMessageDelta    = "item/agentMessage/delta"
	NotifPlanDelta            = "item/plan/delta"
	NotifReasoningSummaryDelta = "item/reasoning/summaryTextDelta"
	NotifReasoningTextDelta   = "item/reasoning/textDelta"
	NotifCommandOutputDelta   = "item/commandExecution/outputDelta"
	NotifFileChangeOutputDelta = "item/fileChange/outputDelta"
	NotifFileChangePatchUpdated = "item/fileChange/patchUpdated"
	NotifError                = "error"

	// Server → Client requests (we receive these and must reply).
	ServerReqCommandExecApproval = "item/commandExecution/requestApproval"
	ServerReqFileChangeApproval  = "item/fileChange/requestApproval"
	ServerReqPermissionsApproval = "item/permissions/requestApproval"
	ServerReqToolUserInput       = "item/tool/requestUserInput"
)

// ── initialize ──
// Source: app-server-protocol/src/protocol/v1.rs InitializeParams/Response.

// CodexInitializeParams is the body of the very first JSON-RPC request sent
// after spawning `codex app-server`. Until the server replies and we send the
// `initialized` notification, every other call returns "Not initialized".
type CodexInitializeParams struct {
	ClientInfo   CodexClientInfo            `json:"clientInfo"`
	Capabilities *CodexInitializeCapabilities `json:"capabilities,omitempty"`
}

// CodexClientInfo identifies the calling client to the server. Codex echoes
// this back in tracing logs and uses `name` for some compatibility checks.
type CodexClientInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

// CodexInitializeCapabilities lets the client opt into experimental APIs and
// silence specific noisy notifications. weiran always sets
// ExperimentalAPI=false and may opt out of agentMessage delta if the
// frontend doesn't need streaming text.
type CodexInitializeCapabilities struct {
	ExperimentalAPI            bool     `json:"experimentalApi,omitempty"`
	OptOutNotificationMethods []string `json:"optOutNotificationMethods,omitempty"`
}

// CodexInitializeResponse is the server's response to `initialize`. The
// fields tell us where the server keeps its state and which platform it's
// running on — useful for telemetry but not required for normal operation.
type CodexInitializeResponse struct {
	UserAgent      string `json:"userAgent"`
	CodexHome      string `json:"codexHome"`
	PlatformFamily string `json:"platformFamily"`
	PlatformOS     string `json:"platformOs"`
}

// ── Thread lifecycle ──
// Source: app-server-protocol/src/protocol/v2.rs Thread*Params/Response/Status.

// CodexThreadStartParams kicks off a fresh thread. Required fields are
// minimal — we only need Cwd and Model — because every other knob has a
// reasonable server-side default. weiran sets ApprovalPolicy="never" and
// Sandbox="workspace-write" so the hook layer can govern approvals.
//
// Sandbox is a SandboxMode enum ("read-only" | "workspace-write" |
// "danger-full-access"). It replaced the old PermissionProfile object in
// codex 0.153.x; passing permissionProfile now fails with
// -32602 "no longer supported for thread/start".
type CodexThreadStartParams struct {
	Model              string                 `json:"model,omitempty"`
	ModelProvider      string                 `json:"modelProvider,omitempty"`
	Cwd                string                 `json:"cwd,omitempty"`
	ApprovalPolicy     string                 `json:"approvalPolicy,omitempty"`
	Sandbox            string                 `json:"sandbox,omitempty"`
	BaseInstructions   string                 `json:"baseInstructions,omitempty"`
	DeveloperInstructions string              `json:"developerInstructions,omitempty"`
	Config             map[string]any         `json:"config,omitempty"`
	ServiceName        string                 `json:"serviceName,omitempty"`
	Ephemeral          bool                   `json:"ephemeral,omitempty"`
	SessionStartSource string                 `json:"sessionStartSource,omitempty"`
}

// CodexThreadStartResponse carries the freshly-minted thread metadata. The
// nested Thread struct is what subsequent turn calls reference via thread.id.
type CodexThreadStartResponse struct {
	Thread             CodexThread       `json:"thread"`
	Model              string            `json:"model"`
	ModelProvider      string            `json:"modelProvider"`
	Cwd                string            `json:"cwd"`
	ApprovalPolicy     string            `json:"approvalPolicy"`
	ApprovalsReviewer  string            `json:"approvalsReviewer"`
	InstructionSources []string          `json:"instructionSources,omitempty"`
	ReasoningEffort    string            `json:"reasoningEffort,omitempty"`
}

// CodexThreadResumeParams reactivates a previously-persisted thread. The
// server can locate it by id, history, or path — weiran always uses ThreadID
// because that's the only path that survives across restarts. ExcludeTurns is
// set true so the response stays light; a follow-up `thread/turns/list` can
// fetch the history if the UI needs it.
type CodexThreadResumeParams struct {
	ThreadID            string         `json:"threadId"`
	Model               string         `json:"model,omitempty"`
	ModelProvider       string         `json:"modelProvider,omitempty"`
	Cwd                 string         `json:"cwd,omitempty"`
	ApprovalPolicy      string         `json:"approvalPolicy,omitempty"`
	Sandbox             string         `json:"sandbox,omitempty"`
	BaseInstructions    string         `json:"baseInstructions,omitempty"`
	DeveloperInstructions string       `json:"developerInstructions,omitempty"`
	Config              map[string]any `json:"config,omitempty"`
	ExcludeTurns        bool           `json:"excludeTurns,omitempty"`
}

// CodexThreadResumeResponse mirrors ThreadStartResponse with the addition of
// the previously-recorded turns, when ExcludeTurns is false.
type CodexThreadResumeResponse struct {
	Thread          CodexThread `json:"thread"`
	Model           string      `json:"model"`
	ModelProvider   string      `json:"modelProvider"`
	Cwd             string      `json:"cwd"`
	ApprovalPolicy  string      `json:"approvalPolicy"`
	ReasoningEffort string      `json:"reasoningEffort,omitempty"`
}

// CodexThreadUnsubscribeParams releases the client's subscription to a
// thread's notification stream. Other clients (or the same client after a
// `thread/resume`) can still drive the thread; this only stops *us* from
// hearing about it. weiran calls this on session destroy.
type CodexThreadUnsubscribeParams struct {
	ThreadID string `json:"threadId"`
}

// CodexThreadUnsubscribeResponse is empty — codex returns {} on success.
type CodexThreadUnsubscribeResponse struct{}

// CodexThreadLoadedListParams asks the server which threads are currently
// loaded into memory. weiran uses this as a heartbeat: a successful round
// trip proves the JSON-RPC channel is still healthy.
type CodexThreadLoadedListParams struct{}

// CodexThreadLoadedListResponse lists the currently-resident threads. We
// only inspect the count for liveness; the contents aren't surfaced to the
// UI.
type CodexThreadLoadedListResponse struct {
	Threads []CodexThread `json:"threads"`
}

// CodexThread is the canonical thread descriptor returned by start / resume
// / loaded-list. Most fields are informational; ID is the only one we
// actually pin down to drive subsequent turns.
type CodexThread struct {
	ID            string             `json:"id"`
	ForkedFromID  string             `json:"forkedFromId,omitempty"`
	Preview       string             `json:"preview,omitempty"`
	Ephemeral     bool               `json:"ephemeral,omitempty"`
	ModelProvider string             `json:"modelProvider"`
	CreatedAt     int64              `json:"createdAt"`
	UpdatedAt     int64              `json:"updatedAt"`
	Status        CodexThreadStatus  `json:"status"`
	Path          string             `json:"path,omitempty"`
	Cwd           string             `json:"cwd"`
	CliVersion    string             `json:"cliVersion,omitempty"`
	Source        json.RawMessage    `json:"source,omitempty"`
	Name          string             `json:"name,omitempty"`
	Turns         []CodexTurn        `json:"turns,omitempty"`
	GitInfo       json.RawMessage    `json:"gitInfo,omitempty"`
}

// CodexThreadStatus is a tagged-union ("type" discriminator). The Active
// variant carries flags telling us why the thread is busy (waiting on
// approval / waiting on user). NotLoaded / Idle / SystemError have no extra
// fields.
type CodexThreadStatus struct {
	Type        string                  `json:"type"`
	ActiveFlags []string                `json:"activeFlags,omitempty"`
}

// ── Turns ──

// CodexTurnStartParams starts a new turn on an existing thread by feeding
// the model a fresh user input. Most fields override thread-level defaults
// for just this turn.
//
// There is no permission/sandbox field here on purpose: a turn inherits the
// thread-level Sandbox set at thread/start. TurnStartParams does still expose
// a deprecated `sandboxPolicy` object, but we never send it.
type CodexTurnStartParams struct {
	ThreadID         string           `json:"threadId"`
	Input            []CodexUserInput `json:"input"`
	Cwd              string           `json:"cwd,omitempty"`
	ApprovalPolicy   string           `json:"approvalPolicy,omitempty"`
	Model            string           `json:"model,omitempty"`
	Effort           string           `json:"effort,omitempty"`
	Summary          string           `json:"summary,omitempty"`
	OutputSchema     json.RawMessage  `json:"outputSchema,omitempty"`
}

// CodexTurnStartResponse echoes the freshly-allocated turn descriptor. The
// Items field is empty at start — content arrives via item/* notifications.
type CodexTurnStartResponse struct {
	Turn CodexTurn `json:"turn"`
}

// CodexTurnInterruptParams forces an in-flight turn to abort. Codex returns
// success even if the turn already finished; the matching turn/completed
// notification will still arrive (with status="interrupted" if cancellation
// raced the natural completion).
type CodexTurnInterruptParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
}

// CodexTurnInterruptResponse is empty.
type CodexTurnInterruptResponse struct{}

// CodexTurn is the turn descriptor. Rather than driving the UI from this
// struct we listen to the streaming item/* notifications; the only field we
// inspect is Status during turn/completed.
type CodexTurn struct {
	ID          string          `json:"id"`
	Items       []CodexThreadItem `json:"items,omitempty"`
	Status      string          `json:"status"`
	Error       *CodexTurnError `json:"error,omitempty"`
	StartedAt   *int64          `json:"startedAt,omitempty"`
	CompletedAt *int64          `json:"completedAt,omitempty"`
	DurationMs  *int64          `json:"durationMs,omitempty"`
}

// CodexTurnError carries failure detail when Turn.Status is "failed". The
// CodexErrorInfo enum is what we need for translating to UnifiedEvent
// backend_error reason codes.
type CodexTurnError struct {
	Message           string          `json:"message"`
	CodexErrorInfo    json.RawMessage `json:"codexErrorInfo,omitempty"`
	AdditionalDetails string          `json:"additionalDetails,omitempty"`
}

// Turn status constants. Source: TurnStatus enum in v2.rs.
const (
	CodexTurnStatusInProgress  = "inProgress"
	CodexTurnStatusCompleted   = "completed"
	CodexTurnStatusInterrupted = "interrupted"
	CodexTurnStatusFailed      = "failed"
)

// ── User Input ──
// Source: UserInput enum in v2.rs (tagged union with "type" discriminator).
//
// Codex accepts heterogeneous input: text + images + skill / mention
// references. weiran's CC backend currently only sends text + image URLs;
// LocalImage / Skill / Mention show up later when we wire selfie-skill flow
// through codex.

// CodexUserInputType discriminates the variant.
type CodexUserInputType string

const (
	CodexUserInputText       CodexUserInputType = "text"
	CodexUserInputImage      CodexUserInputType = "image"
	CodexUserInputLocalImage CodexUserInputType = "localImage"
	CodexUserInputSkill      CodexUserInputType = "skill"
	CodexUserInputMention    CodexUserInputType = "mention"
)

// CodexUserInput is a single input fragment. Use the type constants above
// to discriminate. Only the fields matching the variant should be populated.
//
// The Rust schema uses an `untagged` enum on the wire, but it's actually
// internally tagged via a "type" field at the camelCase level. Since codex
// historically accepted both shapes, we go with the typed form which is
// what the current server emits.
type CodexUserInput struct {
	Type         CodexUserInputType `json:"type"`
	Text         string             `json:"text,omitempty"`
	TextElements []json.RawMessage  `json:"textElements,omitempty"`
	URL          string             `json:"url,omitempty"`  // Image variant
	Path         string             `json:"path,omitempty"` // LocalImage / Mention variant
	Name         string             `json:"name,omitempty"` // Skill / Mention variant
}

// ── Thread Items ──
// Source: ThreadItem enum in v2.rs.
//
// The schema is a tagged union with "type" as the discriminator. Rather
// than hand-rolling 16 separate Go types and a custom unmarshal, weiran
// keeps the raw bytes around and lets the backend translate based on the
// Type field. The fields below are the ones the codex backend actually
// reads when emitting UnifiedEvents (ID for routing, Type for ItemKind,
// the type-specific payloads for the structured ItemPayload).

// CodexItemType is the discriminator value carried by ThreadItem.type.
type CodexItemType string

const (
	CodexItemUserMessage         CodexItemType = "userMessage"
	CodexItemHookPrompt          CodexItemType = "hookPrompt"
	CodexItemAgentMessage        CodexItemType = "agentMessage"
	CodexItemPlan                CodexItemType = "plan"
	CodexItemReasoning           CodexItemType = "reasoning"
	CodexItemCommandExecution    CodexItemType = "commandExecution"
	CodexItemFileChange          CodexItemType = "fileChange"
	CodexItemMcpToolCall         CodexItemType = "mcpToolCall"
	CodexItemDynamicToolCall     CodexItemType = "dynamicToolCall"
	CodexItemCollabAgentToolCall CodexItemType = "collabAgentToolCall"
	CodexItemWebSearch           CodexItemType = "webSearch"
	CodexItemImageView           CodexItemType = "imageView"
	CodexItemImageGeneration     CodexItemType = "imageGeneration"
	CodexItemEnteredReviewMode   CodexItemType = "enteredReviewMode"
	CodexItemExitedReviewMode    CodexItemType = "exitedReviewMode"
	CodexItemContextCompaction   CodexItemType = "contextCompaction"
)

// CodexThreadItem is the partial deserialization of any ThreadItem. The
// full item-specific payload (e.g. CommandExecution.aggregatedOutput) lives
// in Raw and is parsed lazily by the backend's translator. Adding more
// typed fields here is fine — but the principle is "decode what we route
// on, defer the rest" so the schema stays narrow.
type CodexThreadItem struct {
	Type CodexItemType `json:"type"`
	ID   string        `json:"id"`

	// agentMessage / plan: text body
	Text string `json:"text,omitempty"`

	// reasoning: dual-array body
	Summary []string `json:"summary,omitempty"`
	Content []string `json:"content,omitempty"`

	// commandExecution / fileChange / dynamicToolCall: tool name surrogates
	Command          string          `json:"command,omitempty"`
	Cwd              string          `json:"cwd,omitempty"`
	Status           string          `json:"status,omitempty"`
	AggregatedOutput string          `json:"aggregatedOutput,omitempty"`
	ExitCode         *int            `json:"exitCode,omitempty"`
	Tool             string          `json:"tool,omitempty"`
	Server           string          `json:"server,omitempty"`
	Arguments        json.RawMessage `json:"arguments,omitempty"`
	Changes          json.RawMessage `json:"changes,omitempty"`
	DurationMs       *int64          `json:"durationMs,omitempty"`

	// Preserves the verbatim payload for fields we haven't typed yet.
	Raw json.RawMessage `json:"-"`
}

// ── Notifications ──
// Source: ServerNotification enum in app-server-protocol/src/protocol/common.rs
// (line 1035) and the matching V2 notification structs (line ~6319 onwards).

// CodexThreadStartedNotification fires when a freshly-started thread is
// fully initialized and ready to accept turns.
type CodexThreadStartedNotification struct {
	Thread CodexThread `json:"thread"`
}

// CodexThreadStatusChangedNotification reports thread state transitions
// (idle → active(waitingOnApproval/waitingOnUserInput) → idle, or → systemError).
type CodexThreadStatusChangedNotification struct {
	ThreadID string            `json:"threadId"`
	Status   CodexThreadStatus `json:"status"`
}

// CodexThreadClosedNotification arrives when the server retires a thread.
// After this fires, the thread id is no longer routable.
type CodexThreadClosedNotification struct {
	ThreadID string `json:"threadId"`
}

// CodexThreadNameUpdatedNotification fires when a thread is renamed (either
// by user action or auto-naming).
type CodexThreadNameUpdatedNotification struct {
	ThreadID   string `json:"threadId"`
	ThreadName string `json:"threadName,omitempty"`
}

// CodexThreadTokenUsageUpdatedNotification carries running token usage
// deltas. weiran maps these to UnifiedEvent payload usage fields.
type CodexThreadTokenUsageUpdatedNotification struct {
	ThreadID   string                 `json:"threadId"`
	TurnID     string                 `json:"turnId"`
	TokenUsage CodexThreadTokenUsage  `json:"tokenUsage"`
}

// CodexThreadTokenUsage is the cumulative + per-turn breakdown.
type CodexThreadTokenUsage struct {
	Total              CodexTokenUsageBreakdown `json:"total"`
	Last               CodexTokenUsageBreakdown `json:"last"`
	ModelContextWindow *int64                   `json:"modelContextWindow,omitempty"`
}

// CodexTokenUsageBreakdown decomposes a usage snapshot.
type CodexTokenUsageBreakdown struct {
	TotalTokens           int64 `json:"totalTokens"`
	InputTokens           int64 `json:"inputTokens"`
	CachedInputTokens     int64 `json:"cachedInputTokens"`
	OutputTokens          int64 `json:"outputTokens"`
	ReasoningOutputTokens int64 `json:"reasoningOutputTokens"`
}

// CodexTurnStartedNotification arrives shortly after `turn/start` — the
// turn id here is what subsequent item-level notifications reference.
type CodexTurnStartedNotification struct {
	ThreadID string    `json:"threadId"`
	Turn     CodexTurn `json:"turn"`
}

// CodexTurnCompletedNotification is the authoritative end-of-turn signal.
// Items in the embedded turn are empty per the protocol; consumers must
// have collected items via item/* notifications.
type CodexTurnCompletedNotification struct {
	ThreadID string    `json:"threadId"`
	Turn     CodexTurn `json:"turn"`
}

// CodexTurnDiffUpdatedNotification carries the latest aggregate unified-diff
// across all FileChange items in the current turn.
type CodexTurnDiffUpdatedNotification struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	Diff     string `json:"diff"`
}

// CodexTurnPlanUpdatedNotification streams plan updates (analogous to CC's
// ExitPlanMode flow). The plan is the authoritative ordered list; PlanDelta
// notifications stream items as they're written.
type CodexTurnPlanUpdatedNotification struct {
	ThreadID    string                 `json:"threadId"`
	TurnID      string                 `json:"turnId"`
	Explanation string                 `json:"explanation,omitempty"`
	Plan        []CodexTurnPlanStep    `json:"plan"`
}

// CodexTurnPlanStep is one step in a plan. Status uses Pending / InProgress /
// Completed (camelCase).
type CodexTurnPlanStep struct {
	Step   string `json:"step"`
	Status string `json:"status"`
}

// CodexItemStartedNotification announces a new item appearing inside a turn.
// The Item carries the initial state (e.g. command line, file path, agent
// message id) — most kinds finish with a matching item/completed.
type CodexItemStartedNotification struct {
	Item     CodexThreadItem `json:"item"`
	ThreadID string          `json:"threadId"`
	TurnID   string          `json:"turnId"`
}

// CodexItemCompletedNotification carries the final, authoritative state of
// an item. If you only listen to one item-level event, listen to this — the
// streaming deltas are best-effort.
type CodexItemCompletedNotification struct {
	Item     CodexThreadItem `json:"item"`
	ThreadID string          `json:"threadId"`
	TurnID   string          `json:"turnId"`
}

// CodexAgentMessageDeltaNotification streams text chunks for an in-flight
// agent message. The deltas should be concatenated; the final text in
// item/completed is canonical and may differ slightly from the concatenation.
type CodexAgentMessageDeltaNotification struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	ItemID   string `json:"itemId"`
	Delta    string `json:"delta"`
}

// CodexPlanDeltaNotification streams plan items. Unlike agent message deltas,
// these don't reliably concatenate to the final plan — use the completed
// item for ground truth.
type CodexPlanDeltaNotification struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	ItemID   string `json:"itemId"`
	Delta    string `json:"delta"`
}

// CodexReasoningSummaryDeltaNotification streams reasoning summary text.
// SummaryIndex disambiguates parts when reasoning is split.
type CodexReasoningSummaryDeltaNotification struct {
	ThreadID     string `json:"threadId"`
	TurnID       string `json:"turnId"`
	ItemID       string `json:"itemId"`
	Delta        string `json:"delta"`
	SummaryIndex int64  `json:"summaryIndex"`
}

// CodexReasoningTextDeltaNotification streams raw reasoning text (vs
// summary). ContentIndex disambiguates content parts.
type CodexReasoningTextDeltaNotification struct {
	ThreadID     string `json:"threadId"`
	TurnID       string `json:"turnId"`
	ItemID       string `json:"itemId"`
	Delta        string `json:"delta"`
	ContentIndex int64  `json:"contentIndex"`
}

// CodexCommandOutputDeltaNotification streams stdout/stderr from an
// in-flight commandExecution item.
type CodexCommandOutputDeltaNotification struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	ItemID   string `json:"itemId"`
	Delta    string `json:"delta"`
}

// CodexFileChangeOutputDeltaNotification streams progress for a fileChange
// item (e.g. patch application status).
type CodexFileChangeOutputDeltaNotification struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	ItemID   string `json:"itemId"`
	Delta    string `json:"delta"`
}

// CodexFileChangePatchUpdatedNotification carries the updated set of file
// changes (canonical view; deltas are illustrative).
type CodexFileChangePatchUpdatedNotification struct {
	ThreadID string          `json:"threadId"`
	TurnID   string          `json:"turnId"`
	ItemID   string          `json:"itemId"`
	Changes  json.RawMessage `json:"changes"`
}

// CodexErrorNotification fires for transport-level / turn-level errors.
// WillRetry=true means the server is retrying transparently; no UI action
// needed beyond logging.
type CodexErrorNotification struct {
	Error     CodexTurnError `json:"error"`
	WillRetry bool           `json:"willRetry"`
	ThreadID  string         `json:"threadId"`
	TurnID    string         `json:"turnId"`
}

// ── Server-initiated approvals ──
// These arrive as JSON-RPC *requests* (with id) — we must reply.

// CodexCommandExecApprovalParams asks the user to approve a shell command.
// Round 4 routes these through the existing tool-hook PreToolUse pipeline.
type CodexCommandExecApprovalParams struct {
	ThreadID                  string          `json:"threadId"`
	TurnID                    string          `json:"turnId"`
	ItemID                    string          `json:"itemId"`
	ApprovalID                string          `json:"approvalId,omitempty"`
	Reason                    string          `json:"reason,omitempty"`
	NetworkApprovalContext    json.RawMessage `json:"networkApprovalContext,omitempty"`
	Command                   string          `json:"command,omitempty"`
	Cwd                       string          `json:"cwd,omitempty"`
	CommandActions            json.RawMessage `json:"commandActions,omitempty"`
	ProposedExecpolicyAmendment json.RawMessage `json:"proposedExecpolicyAmendment,omitempty"`
	AvailableDecisions        []string        `json:"availableDecisions,omitempty"`
}

// CodexCommandExecApprovalResponse is the reply. Decision must be one of
// the CodexCommandExecApprovalDecision* values below.
type CodexCommandExecApprovalResponse struct {
	Decision string `json:"decision"`
}

// CommandExecution approval decisions.
const (
	CodexCommandExecApprovalAccept           = "accept"
	CodexCommandExecApprovalAcceptForSession = "acceptForSession"
	CodexCommandExecApprovalDecline          = "decline"
	CodexCommandExecApprovalCancel           = "cancel"
)

// CodexFileChangeApprovalParams asks the user to approve writes / patches.
type CodexFileChangeApprovalParams struct {
	ThreadID   string `json:"threadId"`
	TurnID     string `json:"turnId"`
	ItemID     string `json:"itemId"`
	Reason     string `json:"reason,omitempty"`
	GrantRoot  string `json:"grantRoot,omitempty"`
}

// CodexFileChangeApprovalResponse — Decision is one of the
// CodexFileChangeApproval* constants.
type CodexFileChangeApprovalResponse struct {
	Decision string `json:"decision"`
}

// FileChange approval decisions.
const (
	CodexFileChangeApprovalAccept           = "accept"
	CodexFileChangeApprovalAcceptForSession = "acceptForSession"
	CodexFileChangeApprovalDecline          = "decline"
	CodexFileChangeApprovalCancel           = "cancel"
)

// CodexPermissionsApprovalParams asks the user to grant additional file /
// network / sandbox permissions for the rest of this turn or session.
type CodexPermissionsApprovalParams struct {
	ThreadID    string          `json:"threadId"`
	TurnID      string          `json:"turnId"`
	ItemID      string          `json:"itemId"`
	Cwd         string          `json:"cwd"`
	Reason      string          `json:"reason,omitempty"`
	Permissions json.RawMessage `json:"permissions"`
}

// CodexPermissionsApprovalResponse — Scope is "turn" or "session".
type CodexPermissionsApprovalResponse struct {
	Permissions      json.RawMessage `json:"permissions"`
	Scope            string          `json:"scope,omitempty"`
	StrictAutoReview *bool           `json:"strictAutoReview,omitempty"`
}

// Permissions grant scope.
const (
	CodexPermissionsScopeTurn    = "turn"
	CodexPermissionsScopeSession = "session"
)

// ── Standard JSON-RPC error codes (non-exhaustive) ──
//
// Codex returns the JSON-RPC standard `-32xxx` codes plus its own enum
// inside `error.data` for the typed CodexErrorInfo. Round 3's backend
// distinguishes only "server overloaded" (for retry-with-backoff) from the
// rest (surfaced as UEvtBackendError).

const (
	CodexJSONRPCErrParseError     = -32700
	CodexJSONRPCErrInvalidRequest = -32600
	CodexJSONRPCErrMethodNotFound = -32601
	CodexJSONRPCErrInvalidParams  = -32602
	CodexJSONRPCErrInternalError  = -32603

	// Codex-specific: server is overloaded; retry with exponential backoff.
	CodexJSONRPCErrServerOverloaded = -32001
)
