package app

// server_soul_patch.go — Phase D Runtime Soul Patch Protocol injector.
//
// The frozen protocol (workspace/soul/core/00-protocol.md) lets the server
// append per-message persona augmentations to a user turn by wrapping content
// in `<<soul-patch>>...<</soul-patch>>` blocks that ride along with the next
// real user message. The model is told (by 00-protocol.md, which is itself
// loaded as a fragment) to absorb patches as system prompt extensions and
// reply only to the real user content.
//
// This file owns:
//   - Sanitization of user input (strip literal protocol markers — clients
//     must not be able to spoof patches).
//   - Wrapping fragment bodies into the on-wire patch shape.
//   - Diffing the desired fragment set against what's already loaded into
//     the session and producing the patch payload.
//   - Stripping patch blocks back out for UI rendering / history replay.
//
// All state lives on serverSession (LoadedFragments / PendingPatches /
// CurrentSoulMode); this file is otherwise pure helpers.
//
// Cumulative semantics:
//
//   Once a fragment is loaded into a session's persona it stays loaded for
//   the life of that session — soul-cli never "unloads" persona content
//   because the model has already cached / acted on it. detectSoulMode() is
//   used as a *trigger* for which fragments to consider loading, not as a
//   reset of the loaded set. This keeps prompt-cache prefixes monotonic
//   inside a session and matches how persona drift actually feels in
//   practice (the warmer mode never forgets the technical context that
//   came before it).

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// soulPatchOpen and soulPatchClose are the on-wire boundary markers from
// the frozen protocol. Clients are forbidden from sending these literally;
// any user-supplied text containing them is sanitized at intake.
const (
	soulPatchOpen  = "<<soul-patch>>"
	soulPatchClose = "<</soul-patch>>"
)

// soulPatchBlockRegex matches one full `<<soul-patch>>...<</soul-patch>>`
// block including any leading whitespace and the trailing newline (so we
// can strip cleanly from prefixed messages without leaving blank lines).
// We use a non-greedy body match so multiple back-to-back patches in one
// message are stripped one at a time.
var soulPatchBlockRegex = regexp.MustCompile(`(?s)\s*<<soul-patch>>.*?<</soul-patch>>\n?`)

// soulPatchOpenLiteralRegex / soulPatchCloseLiteralRegex match raw markers
// in untrusted text. We strip the markers themselves but keep any
// surrounding text — protocol violation should not silently delete user
// content, just disarm the markers.
var (
	soulPatchOpenLiteralRegex  = regexp.MustCompile(`<<\s*soul-patch\s*(?::[\w.-]+)?\s*>>`)
	soulPatchCloseLiteralRegex = regexp.MustCompile(`<</\s*soul-patch\s*>>`)
)

// sanitizeSoulPatchInput disarms any literal soul-patch markers in
// untrusted text by replacing them with bracketed sentinels. We choose
// replacement (rather than deletion) so a malicious user cannot make
// content disappear silently and so the resulting text is obviously
// modified if it ever shows up in logs / replay.
//
// Returns the cleaned text and a bool indicating whether anything was
// stripped (callers may want to log / metric on protocol abuse attempts).
func sanitizeSoulPatchInput(s string) (string, bool) {
	if !strings.Contains(s, "<<") && !strings.Contains(s, "<</") {
		return s, false
	}
	original := s
	s = soulPatchOpenLiteralRegex.ReplaceAllString(s, "[stripped:soul-patch-open]")
	s = soulPatchCloseLiteralRegex.ReplaceAllString(s, "[stripped:soul-patch-close]")
	return s, s != original
}

// stripSoulPatchBlocks removes every full `<<soul-patch>>...<</soul-patch>>`
// block from s, leaving the rest of the message intact. Used by:
//   - history replay / `/api/history/{id}/messages` so the Web UI never
//     surfaces patch payloads to the human reader.
//   - any audit / search indexing path that should index the user's real
//     message, not the persona augmentation.
//
// Trims trailing whitespace from the result so a message that was *only*
// patches followed by real text doesn't end up with a leading blank line.
func stripSoulPatchBlocks(s string) string {
	if !strings.Contains(s, soulPatchOpen) {
		return s
	}
	out := soulPatchBlockRegex.ReplaceAllString(s, "")
	return strings.TrimLeft(out, "\n")
}

// wrapSoulPatchBlock wraps a fragment body into the on-wire patch shape.
// Body should already have its YAML frontmatter stripped (callers use
// stripFragmentFrontmatter from prompt_routing.go).
func wrapSoulPatchBlock(body string) string {
	body = strings.TrimRight(body, "\n")
	// Reject whitespace-only bodies — wrapping them would emit an empty
	// patch block, which the model has no instruction to absorb and which
	// just bloats the prefix.
	if strings.TrimSpace(body) == "" {
		return ""
	}
	return soulPatchOpen + "\n" + body + "\n" + soulPatchClose
}

// computeNewFragments returns desired \\ already, preserving desired's order.
// Pure function so it's trivial to unit-test independently of session state.
func computeNewFragments(desired []string, already map[string]bool) []string {
	if len(desired) == 0 {
		return nil
	}
	out := make([]string, 0, len(desired))
	for _, p := range desired {
		if !already[p] {
			out = append(out, p)
		}
	}
	return out
}

// buildSoulPatchPayload reads each fragment file, strips its frontmatter,
// and wraps it as a patch block. Returns the concatenated on-wire patch
// (one block per fragment, separated by single newlines), the slice of
// fragment paths that were successfully read, and an error only if every
// read failed (partial failures are best-effort: we ship what we got and
// log the rest to stderr).
func buildSoulPatchPayload(paths []string) (payload string, loaded []string, err error) {
	if len(paths) == 0 {
		return "", nil, nil
	}
	var blocks []string
	for _, p := range paths {
		data, rErr := os.ReadFile(p)
		if rErr != nil {
			fmt.Fprintf(os.Stderr, "[%s] soul-patch: read %s: %v\n", appName, p, rErr)
			continue
		}
		body := stripFragmentFrontmatter(string(data))
		block := wrapSoulPatchBlock(body)
		if block == "" {
			continue
		}
		blocks = append(blocks, block)
		loaded = append(loaded, p)
	}
	if len(blocks) == 0 {
		return "", nil, fmt.Errorf("no fragments could be read from %d candidates", len(paths))
	}
	return strings.Join(blocks, "\n"), loaded, nil
}

// soulPatchInjection is the result returned by serverSession.prepareSoulPatch.
// Callers consume:
//   - Outbound: the message string to actually feed the backend (patches +
//     real user text concatenated per protocol).
//   - DisplayMessage: the version safe to broadcast to UI clients (real
//     user text only, no patch blocks).
//   - NewFragments: paths newly absorbed in this turn — caller must commit
//     them onto LoadedFragments after the backend.send succeeds.
//   - SoulMode: the routing decision made for this turn.
//   - Sanitized: true if the user's input contained protocol markers that
//     had to be disarmed.
type soulPatchInjection struct {
	Outbound            string
	DisplayMessage      string
	NewFragments        []string
	FlushedPendingCount int
	SoulMode            SoulMode
	Sanitized           bool
}

// prepareSoulPatch is the per-message lazy injector. Given an inbound user
// message, it:
//
//  1. Sanitizes literal patch markers so untrusted clients cannot smuggle
//     fake patches into the model.
//  2. Detects the soul-mode this message routes to (intimate triggers,
//     cwd prefixes, source defaults — see prompt_routing.go).
//  3. Loads the fragment list for that mode.
//  4. Diffs against what's already in the session's persona.
//  5. Flushes any patches that were queued from earlier server-side mode
//     transitions (typed but undelivered because the user hadn't spoken).
//  6. Builds an outbound string of `<<soul-patch>>...<</soul-patch>>`
//     blocks followed by the sanitized user message.
//
// State mutation: prepareSoulPatch *only* reads session state and *clears*
// PendingPatches under the session lock. New fragments are returned but
// not yet appended to LoadedFragments — callers commit only after the
// backend successfully receives the message, so a sendMessage failure
// doesn't poison the loaded set.
// prepareSoulPatchResumeOnly is the resume-path variant that skips topic
// fragment detection. Resume/rehydrate messages are infrastructure (server
// restart notices, context continuations) and must not trigger personality
// fragment loading — otherwise words in the canned message (e.g. "your work")
// match fragment tags and cause repeated injection on every restart.
//
// It still sanitizes input and flushes any queued PendingPatches.
func (s *serverSession) prepareSoulPatchResumeOnly(message string) soulPatchInjection {
	clean, sanitized := sanitizeSoulPatchInput(message)

	s.mu.Lock()
	pending := append([]string(nil), s.PendingPatches...)
	s.mu.Unlock()

	// Resolve mode for the injection record (audit/debug) but don't
	// load fragments.
	signals := gatherRoutingSignals()
	signals.FirstMessage = clean
	if s.Project != "" {
		signals.LaunchDir = s.Project
	}
	mode := detectSoulMode(signals)

	return soulPatchInjection{
		Outbound:            joinPatchAndMessage(pending, clean),
		DisplayMessage:      clean,
		FlushedPendingCount: len(pending),
		SoulMode:            mode,
		Sanitized:           sanitized,
	}
}

func (s *serverSession) prepareSoulPatch(message string) soulPatchInjection {
	clean, sanitized := sanitizeSoulPatchInput(message)

	// Build routing signals using *this* message as the FirstMessage, not
	// the session-creation-time first message. That's the whole point of
	// per-message lazy: every turn reroutes.
	signals := gatherRoutingSignals()
	signals.FirstMessage = clean

	// Apply per-session overrides if set: the session was created with a
	// LaunchDir and Source that should dominate over the daemon-wide
	// launchDir / detectSourceFromEnv defaults.
	if s.Project != "" {
		signals.LaunchDir = s.Project
	}

	mode := detectSoulMode(signals)

	// Branch by mode:
	//
	//  - Lazy (interactive) modes: emotional / intimate / technical. The
	//    prompt prefix is core-only + fragment index. We only patch fragments
	//    the agent failed to Read on its own — i.e. tags hit the message but
	//    LoadedFragments doesn't include the file. This is the server-side
	//    fallback layer described in the protocol.
	//
	//  - Eager modes: cron / heartbeat / evolve. The prompt prefix already
	//    has the full mode set (no Read flow available), so prepareSoulPatch
	//    here is a no-op patch-wise — mode-set fragments are loaded at boot
	//    and don't need per-message diffs.
	soulDir := getSoulFragmentsDir()
	var desiredPaths []string
	var fragErr error

	// Single snapshot of already-loaded set + pending patches under one lock
	// acquisition. Both are needed regardless of mode (lazy uses `already`
	// for the topic detector and the diff; eager skips the detector but
	// computeNewFragments below still wants `already` to dedupe). Holding
	// the lock once avoids a TOCTOU window where commitSoulPatchInjection
	// from a concurrent turn mutates LoadedFragments between snapshots.
	s.mu.Lock()
	already := make(map[string]bool, len(s.LoadedFragments))
	for _, p := range s.LoadedFragments {
		already[p] = true
	}
	pending := append([]string(nil), s.PendingPatches...)
	s.mu.Unlock()

	if isLazyAssemblyMode(mode) {
		allMetas, mErr := listAllFragmentMetas(soulDir)
		if mErr != nil {
			fragErr = mErr
		} else {
			desiredPaths = detectTopicFragments(clean, allMetas, already)
		}
	} else {
		desiredPaths, fragErr = loadFragmentsByMode(soulDir, mode)
	}

	if fragErr != nil || len(desiredPaths) == 0 {
		return soulPatchInjection{
			Outbound:            joinPatchAndMessage(pending, clean),
			DisplayMessage:      clean,
			FlushedPendingCount: len(pending),
			SoulMode:            mode,
			Sanitized:           sanitized,
		}
	}

	newFrags := computeNewFragments(desiredPaths, already)

	var blocks []string
	blocks = append(blocks, pending...)

	if len(newFrags) > 0 {
		payload, loaded, perr := buildSoulPatchPayload(newFrags)
		if perr == nil && payload != "" {
			blocks = append(blocks, payload)
			newFrags = loaded // narrow to fragments actually wrapped
		} else {
			newFrags = nil
		}
	}

	return soulPatchInjection{
		Outbound:            joinPatchAndMessage(blocks, clean),
		DisplayMessage:      clean,
		NewFragments:        newFrags,
		FlushedPendingCount: len(pending),
		SoulMode:            mode,
		Sanitized:           sanitized,
	}
}

// joinPatchAndMessage builds the on-wire shape: each patch block followed
// by a newline, then the real user message. If there are no blocks the
// message is returned unchanged so cache prefixes don't shift for
// no-op turns.
func joinPatchAndMessage(blocks []string, message string) string {
	if len(blocks) == 0 {
		return message
	}
	var b strings.Builder
	for _, blk := range blocks {
		if blk == "" {
			continue
		}
		b.WriteString(blk)
		b.WriteString("\n")
	}
	b.WriteString(message)
	return b.String()
}

// commitLoadedFragments appends the just-shipped fragment paths to the
// session's persistent LoadedFragments set. Called after the backend
// accepts the message — if sendMessage fails, the patch never reaches the
// model and we want to retry it on the next turn.
//
// Updates CurrentSoulMode in the same critical section so audit reads
// always see a consistent (mode, fragments) pair, then persists the
// snapshot to the server_sessions DB so a restart-then-rehydrate cycle
// keeps the cumulative loaded set intact.
func (s *serverSession) commitLoadedFragments(newFrags []string, mode SoulMode) {
	s.commitSoulPatchInjection(soulPatchInjection{NewFragments: newFrags, SoulMode: mode})
}

// commitSoulPatchInjection commits the state represented by a successfully
// delivered soul-patch injection. Pending patches are removed by count from
// the head of the queue so entries enqueued after prepareSoulPatch's snapshot
// remain pending for a later turn.
func (s *serverSession) commitSoulPatchInjection(inj soulPatchInjection) {
	if len(inj.NewFragments) == 0 && inj.SoulMode == "" && inj.FlushedPendingCount == 0 {
		return
	}
	s.mu.Lock()
	if inj.FlushedPendingCount > 0 {
		if inj.FlushedPendingCount >= len(s.PendingPatches) {
			s.PendingPatches = nil
		} else {
			s.PendingPatches = append([]string(nil), s.PendingPatches[inj.FlushedPendingCount:]...)
		}
	}
	if len(inj.NewFragments) > 0 {
		seen := make(map[string]bool, len(s.LoadedFragments))
		for _, p := range s.LoadedFragments {
			seen[p] = true
		}
		for _, p := range inj.NewFragments {
			if !seen[p] {
				s.LoadedFragments = append(s.LoadedFragments, p)
				seen[p] = true
			}
		}
	}
	if inj.SoulMode != "" {
		s.CurrentSoulMode = string(inj.SoulMode)
	}
	// Snapshot under lock for the DB call outside it.
	id := s.ID
	loaded := append([]string(nil), s.LoadedFragments...)
	pending := append([]string(nil), s.PendingPatches...)
	curMode := s.CurrentSoulMode
	s.mu.Unlock()

	setSoulPatchState(id, loaded, pending, curMode)
}

// queueSoulPatch enqueues a patch block on the session's pending list.
// Reserved for future server-initiated mode transitions that happen *not*
// in response to a user message (e.g. background heartbeat detecting an
// emotional context shift). For now nothing calls it, but the queue
// flushing path in prepareSoulPatch is wired so adding a producer is a
// one-line drop-in.
func (s *serverSession) queueSoulPatch(block string) {
	if block == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.PendingPatches = append(s.PendingPatches, block)
}
