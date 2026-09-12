package main

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Source names for inbox items.
const (
	sourceUser    = "user"
	sourceTrigger = "trigger"
	sourceCompact = "compact"
)

// Worker owns one Pi process and drains either chat or background inbox items.
type Worker struct {
	inbox   *InboxStore
	piCfg   PiConfig
	startPi func(PiConfig, string, bool) (*PiProcess, error)
	app     *App
	matrix  workerMatrix

	roomID atomic.Value // string, resolved lazily from .room_id file

	background    bool
	triggerPrompt string

	// mu protects pi, lastUse, compactResult, currentCancel, freshStart.
	mu            sync.Mutex
	pi            *PiProcess
	freshStart    bool // next ensurePi spawns without --continue
	lastUse       time.Time
	currentCancel context.CancelFunc
	compactResult chan compactOutcome

	// wake is signalled (non-blocking) after new work is enqueued.
	wake chan struct{}
}

// compactOutcome carries the result of a compact operation back to the caller.
type compactOutcome struct {
	result *CompactResult
	err    error
}

type workerMatrix interface {
	SetTyping(ctx context.Context, conversationID string, typing bool)
	SendMessage(ctx context.Context, conversationID, text, replyToID string) string
}

// NewWorker creates a chat worker. The Pi process starts lazily on first dequeue.
func NewWorker(inbox *InboxStore, piCfg PiConfig) *Worker {
	return newWorker(inbox, piCfg, "", false)
}

// NewBackgroundWorker creates a worker for trigger items.
func NewBackgroundWorker(inbox *InboxStore, piCfg PiConfig, triggerPrompt string) *Worker {
	return newWorker(inbox, piCfg, triggerPrompt, true)
}

func newWorker(inbox *InboxStore, piCfg PiConfig, triggerPrompt string, background bool) *Worker {
	return &Worker{
		inbox:         inbox,
		piCfg:         piCfg,
		startPi:       StartPi,
		triggerPrompt: triggerPrompt,
		background:    background,
		lastUse:       time.Now(),
		wake:          make(chan struct{}, 1),
	}
}

// SetApp wires the app reference (phase 2 of init).
func (w *Worker) SetApp(app *App) { w.app = app }

// SetMatrix wires the Matrix reference (phase 2 of init).
func (w *Worker) SetMatrix(matrixClient workerMatrix) { w.matrix = matrixClient }

// Notify wakes the worker loop after an item is enqueued.
func (w *Worker) Notify() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Run is the main worker loop. It blocks until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	slog.Info("worker: started")

	w.drainOnce(ctx)

	for {
		select {
		case <-ctx.Done():
			w.stopPi()
			slog.Info("worker: stopped")

			return
		case <-w.wake:
			w.drainOnce(ctx)
		}
	}
}

// Abort cancels the currently running operation, if any.
func (w *Worker) Abort() bool {
	w.mu.Lock()
	cancel := w.currentCancel
	w.mu.Unlock()

	if cancel != nil {
		cancel()

		return true
	}

	return false
}

// IsActive returns true if a pi process is alive.
func (w *Worker) IsActive() bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.pi != nil && w.pi.IsAlive()
}

// Restart kills the current pi process and marks the next spawn to
// skip --continue. Without the flag, the next ensurePi would resume
// the same on-disk session (pi persists sessions as jsonl files that
// --continue picks up), so a user stuck at a 429 context-limit wall
// would restart straight back into it.
func (w *Worker) Restart() {
	w.mu.Lock()
	w.freshStart = true
	cancel := w.currentCancel
	pi := w.pi
	w.pi = nil
	w.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	if pi != nil {
		slog.Info("worker: stopping Pi process for restart")
		pi.Kill()
	}
}

// Compact enqueues a compact operation and waits for the result.
func (w *Worker) Compact(ctx context.Context) (*CompactResult, error) {
	if !w.IsActive() {
		return nil, errors.New("no active session")
	}

	w.mu.Lock()
	if w.compactResult != nil {
		w.mu.Unlock()

		return nil, errors.New("compact already in progress")
	}

	ch := make(chan compactOutcome, 1)
	w.compactResult = ch
	w.mu.Unlock()

	if err := w.inbox.Enqueue(ctx, PriorityUser, sourceCompact, "", "", ""); err != nil {
		w.mu.Lock()
		w.compactResult = nil
		w.mu.Unlock()

		return nil, fmt.Errorf("enqueuing compact: %w", err)
	}

	w.Notify()

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("compact cancelled: %w", ctx.Err())
	case outcome := <-ch:
		return outcome.result, outcome.err
	}
}

// SetRoomID sets the room ID for this worker.
func (w *Worker) SetRoomID(roomID string) {
	w.roomID.Store(roomID)
}

// SkillsSummary returns a formatted list of loaded skill paths.
func (w *Worker) SkillsSummary() string {
	skills := w.piCfg.Skills
	if len(skills) == 0 {
		return "No skills loaded."
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%d skill(s) loaded:\n", len(skills))

	for _, s := range skills {
		fmt.Fprintf(&sb, "- %s\n", filepath.Base(s))
	}

	return sb.String()
}

// StartIdleReaper kills the pi process after the configured idle timeout.
func (w *Worker) StartIdleReaper(ctx context.Context) {
	if w.piCfg.IdleTimeout <= 0 {
		return
	}

	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				w.mu.Lock()
				processing := w.currentCancel != nil
				idle := !processing && w.pi != nil && w.pi.IsAlive() && time.Since(w.lastUse) > w.piCfg.IdleTimeout
				w.mu.Unlock()

				if idle {
					slog.Info("worker: reaping idle pi process")
					w.stopPi()
				}
			}
		}
	}()
}

func (w *Worker) dequeue(ctx context.Context) (Inbox, error) {
	if w.background {
		return w.inbox.DequeueBackground(ctx)
	}

	return w.inbox.DequeueChat(ctx)
}

func (w *Worker) drainOnce(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}

		item, err := w.dequeue(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			return
		}

		if err != nil {
			if ctx.Err() == nil {
				slog.Error("worker: dequeue failed", "error", err)
			}

			return
		}

		if w.processItem(ctx, item) {
			return
		}
	}
}

// processItem handles one inbox item.
func (w *Worker) processItem(ctx context.Context, item Inbox) bool {
	slog.Info("worker: processing", "source", item.Source, "priority", item.Priority, "id", item.ID)

	itemCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	w.mu.Lock()
	w.currentCancel = cancel
	w.mu.Unlock()

	defer func() {
		w.mu.Lock()
		w.currentCancel = nil
		w.mu.Unlock()
	}()

	var stopDraining bool

	if item.Source == sourceCompact {
		w.processCompact(itemCtx)
	} else {
		stopDraining = w.processPrompt(itemCtx, item)
	}

	if itemCtx.Err() != nil {
		w.stopPi()
	}

	return stopDraining
}

// processPrompt handles a user or trigger item.
func (w *Worker) processPrompt(ctx context.Context, item Inbox) bool {
	prompt, ok := w.buildPrompt(item)
	if !ok {
		return false
	}

	convID := resolveConversationID(item.ConversationID, w.piCfg.DefaultRoomID, w.resolveRoomID())

	if convID == "" {
		return w.handleNoRoomID(item) //nolint:contextcheck // requeue uses context.Background intentionally
	}

	stopTyping := func(context.Context) {}
	if item.Source == sourceUser && !item.IsGroup {
		stopTyping = w.startTyping(ctx, convID)
	}
	defer stopTyping(context.Background()) //nolint:contextcheck // must clear typing after cancellation

	onToolCall := w.toolCallHandler(ctx, item, convID)
	taskStart := time.Now()

	pi, reply, err := w.sendWithRetry(ctx, prompt, onToolCall)
	if err != nil {
		killPi := pi != nil

		w.handlePiError(ctx, item, convID, "pi prompt failed", err, killPi)

		return false
	}

	w.mu.Lock()
	w.lastUse = time.Now()
	w.mu.Unlock()

	reply = w.prepareReply(ctx, pi, item, convID, reply, onToolCall)

	if shouldSuppressReply(reply, item.Source) {
		return false
	}

	if w.piCfg.DebugTiming {
		reply += fmt.Sprintf("\n\n⏱ %s", time.Since(taskStart).Round(time.Millisecond))
	}

	reply, targetRoom := extractSendTo(reply)
	convID, replyToID := replyDestination(convID, item.ConversationID, item.ReplyTo, targetRoom)

	if ctx.Err() != nil {
		slog.Info("worker: cancelled item dropped before reply", "source", item.Source)

		return false
	}

	w.app.sendReplyWithFiles(ctx, convID, reply, replyToID, !w.background)

	return false
}

// prepareReply extracts and applies an optional Matrix reaction, and retries a
// genuinely empty user response. A reaction-only response is intentional
// output and must not trigger the empty-response summary prompt.
func (w *Worker) prepareReply(
	ctx context.Context,
	pi *PiProcess,
	item Inbox,
	convID, reply string,
	onToolCall func(ToolCallEvent),
) string {
	reply, reaction := extractReaction(reply)
	if item.Source == sourceUser && reply == "" && reaction == nil {
		reply = w.retryEmptyResponse(ctx, pi, onToolCall)
		reply, reaction = extractReaction(reply)
	}

	// Reactions always apply to the source conversation. A later <send-to>
	// tag may reroute the text reply, but never the reaction target.
	if item.Source == sourceUser && reaction != nil {
		w.app.sendReaction(ctx, convID, *reaction)
	}

	return reply
}

// toolCallHandler reports visible tool calls when configured and acknowledges
// the first tool used for a Matrix group message with an eyes reaction.
func (w *Worker) toolCallHandler(ctx context.Context, item Inbox, convID string) func(ToolCallEvent) {
	acknowledge := !w.background && item.Source == sourceUser && item.IsGroup && item.MessageID != ""
	if w.background || (!acknowledge && !w.piCfg.ShowToolCalls) {
		return nil
	}

	notificationCtx := context.WithoutCancel(ctx)
	acknowledged := false

	return func(evt ToolCallEvent) {
		if acknowledge && !acknowledged {
			acknowledged = true

			go func() {
				reactionCtx, cancel := context.WithTimeout(notificationCtx, 10*time.Second)
				defer cancel()

				w.app.sendReaction(reactionCtx, convID, reactionRequest{messageID: item.MessageID, emoji: "👀"})
			}()
		}

		if w.piCfg.ShowToolCalls {
			w.matrix.SendMessage(notificationCtx, convID, formatToolCall(evt), "")
		}
	}
}

// startTyping starts the Matrix typing indicator without an artificial delay.
// The returned function waits for the start attempt and clears it if necessary.
func (w *Worker) startTyping(ctx context.Context, convID string) func(context.Context) {
	var (
		started bool
		once    sync.Once
	)

	stop := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)

		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		default:
		}

		w.matrix.SetTyping(ctx, convID, true)

		started = true
	}()

	return func(clearCtx context.Context) {
		once.Do(func() { close(stop) })
		<-done

		if started {
			w.matrix.SetTyping(clearCtx, convID, false)
		}
	}
}

// buildPrompt assembles the prompt for the given inbox item, injecting the current time.
func (w *Worker) buildPrompt(item Inbox) (string, bool) {
	var prompt string

	switch item.Source {
	case sourceUser:
		prompt = item.Content
	case sourceTrigger:
		prompt = buildTriggerPrompt(w.triggerPrompt, item.Content)
	default:
		return "", false
	}

	return injectTimestamp(prompt), true
}

// shouldSuppressReply returns true if the reply should not be forwarded.
func shouldSuppressReply(reply, source string) bool {
	firstLine, _, _ := strings.Cut(strings.TrimSpace(reply), "\n")
	if strings.TrimSpace(firstLine) == "NO_REPLY" {
		slog.Info(source + ": NO_REPLY, suppressing")

		return true
	}

	if reply == "" {
		slog.Info(source + ": empty response, suppressing")

		return true
	}

	return false
}

// injectTimestamp prepends the current date/time to every prompt so the agent knows
// the current time. Uses RFC 3339 (the internet profile of ISO 8601):
// unambiguous, machine-readable, and includes UTC offset.
func injectTimestamp(prompt string) string {
	return "<time>" + time.Now().Format(time.RFC3339) + "</time>\n" + prompt
}

// handleNoRoomID requeues triggers (room may appear later) and drops
// everything else. Returns true if the item was requeued, signalling
// drainOnce to stop looping and wait for the next Notify.
func (w *Worker) handleNoRoomID(item Inbox) bool {
	if item.Source == sourceTrigger {
		if err := w.inbox.Requeue(context.Background(), item); err != nil {
			slog.Error("worker: failed to requeue trigger (item lost)", "error", err)
		} else {
			slog.Info("worker: no room ID for trigger, requeued")
		}

		return true
	}

	slog.Error("worker: no room ID available, dropping item", "source", item.Source)

	return false
}

// handlePiError handles errors from ensurePi or sendAndWait. Cancelled items
// are dropped; terminal failures kill the process and notify chat users.
func (w *Worker) handlePiError(ctx context.Context, item Inbox, convID, label string, err error, killPi bool) {
	if isContextCancellation(ctx, err) {
		slog.Info("worker: cancelled item dropped", "source", item.Source)

		return
	}

	var providerErr *providerError
	if errors.As(err, &providerErr) {
		slog.Warn("worker: provider request failed", "source", item.Source, "error", err)

		if item.Source == sourceUser {
			w.matrix.SendMessage(ctx, convID, providerErr.userMessage(), item.ReplyTo)
		}

		return
	}

	slog.Error("worker: "+label, "source", item.Source, "error", err)

	if killPi {
		w.stopPi()
	}

	if item.Source == sourceUser {
		w.matrix.SendMessage(ctx, convID, fmt.Sprintf("Error: %v", err), "")
	}
}

func (w *Worker) processCompact(ctx context.Context) {
	w.mu.Lock()
	ch := w.compactResult
	w.compactResult = nil
	pi := w.pi
	w.mu.Unlock()

	if ch == nil {
		slog.Warn("worker: compact item but no result channel")

		return
	}

	if pi == nil || !pi.IsAlive() {
		ch <- compactOutcome{err: errors.New("no active session")}

		return
	}

	result, err := pi.Compact(ctx)
	ch <- compactOutcome{result: result, err: err}
}

func isContextCancellation(ctx context.Context, err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		ctx.Err() != nil
}

// sendWithRetry sends a prompt to pi, retrying once with a fresh
// process if the first attempt fails due to a stale/crashed process.
func (w *Worker) sendWithRetry(
	ctx context.Context,
	prompt string,
	onToolCall func(ToolCallEvent),
) (*PiProcess, string, error) {
	pi, err := w.ensurePi(ctx)
	if err != nil {
		return nil, "", err
	}

	reply, err := pi.sendAndWait(ctx, prompt, onToolCall)
	if err == nil {
		return pi, reply, nil
	}

	var providerErr *providerError
	if ctx.Err() != nil || errors.As(err, &providerErr) {
		return pi, "", err
	}

	slog.Info("worker: pi exited, starting fresh process")
	w.stopPi()

	pi, err = w.ensurePi(ctx)
	if err != nil {
		return nil, "", err
	}

	reply, err = pi.sendAndWait(ctx, prompt, onToolCall)

	return pi, reply, err
}

func (w *Worker) ensurePi(ctx context.Context) (*PiProcess, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.pi != nil && w.pi.IsAlive() {
		return w.pi, nil
	}

	if ctx.Err() != nil {
		return nil, fmt.Errorf("ensurePi cancelled: %w", ctx.Err())
	}

	fresh := w.freshStart
	roomID := w.resolveRoomID()

	pi, err := w.startPi(w.piCfg, roomID, fresh)
	if err != nil {
		return nil, err
	}

	if ctx.Err() != nil {
		pi.Kill()

		return nil, fmt.Errorf("ensurePi cancelled: %w", ctx.Err())
	}

	w.pi = pi
	w.freshStart = false

	return pi, nil
}

func (w *Worker) stopPi() {
	w.mu.Lock()
	pi := w.pi
	w.pi = nil
	w.mu.Unlock()

	if pi != nil {
		slog.Info("worker: stopping pi process")
		pi.Kill()
	}
}

func replyDestination(currentRoom, sourceRoom, replyToID, targetRoom string) (string, string) {
	if targetRoom == "" {
		return currentRoom, replyToID
	}

	if targetRoom != sourceRoom {
		replyToID = ""
	}

	return targetRoom, replyToID
}

// resolveConversationID determines the conversation ID for routing a reply,
// using the first non-empty value from the priority chain:
//
//  1. item.ConversationID — set by the user message that created the inbox row
//  2. DefaultRoomID — OPENCROW_MATRIX_ROOM_ID, a stable default for triggers
//  3. resolveRoomID() — last user conversation captured by SetRoomID
func resolveConversationID(itemConvID, defaultRoomID, activeRoomID string) string {
	if itemConvID != "" {
		return itemConvID
	}

	if defaultRoomID != "" {
		return defaultRoomID
	}

	return activeRoomID
}

func (w *Worker) resolveRoomID() string {
	if id, ok := w.roomID.Load().(string); ok && id != "" {
		return id
	}

	stateDir := w.piCfg.StateDir
	if stateDir == "" {
		stateDir = w.piCfg.SessionDir
	}

	path := filepath.Join(stateDir, ".room_id")

	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}

	id := strings.TrimSpace(string(data))
	w.roomID.Store(id)

	return id
}

func (w *Worker) retryEmptyResponse(ctx context.Context, pi *PiProcess, onToolCall func(ToolCallEvent)) string {
	slog.Warn("worker: empty response, re-prompting for summary")

	reply, err := pi.sendAndWait(ctx, "You just completed a task but your response contained no text for the user. Please briefly summarize what you did or respond to the user's message.", onToolCall)
	if err != nil {
		slog.Error("worker: re-prompt failed", "error", err)

		return "(I completed some actions but failed to generate a summary.)"
	}

	return cmp.Or(reply, "(empty response)")
}
