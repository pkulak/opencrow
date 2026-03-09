package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Source names for inbox items.
const (
	sourceUser      = "user"
	sourceTrigger   = "trigger"
	sourceHeartbeat = "heartbeat"
	sourceCompact   = "compact"
)

// Worker owns the single pi process and drains the inbox in priority order.
// There is exactly one worker per opencrow instance.
type Worker struct {
	inbox   *InboxStore
	piCfg   PiConfig
	pi      *PiProcess
	roomID  string // resolved lazily from .room_id file
	lastUse time.Time

	// callbacks
	sendReply  func(ctx context.Context, conversationID string, text string, replyToID string)
	setTyping  func(ctx context.Context, conversationID string, typing bool)
	onToolCall func(ToolCallEvent)

	// config
	hbPrompt      string
	triggerPrompt string

	// compactResult is set before enqueueing a compact item, read by the worker.
	compactResult chan compactOutcome
}

// compactOutcome carries the result of a compact operation back to the caller.
type compactOutcome struct {
	result *CompactResult
	err    error
}

// WorkerConfig holds the dependencies for creating a Worker.
type WorkerConfig struct {
	Inbox         *InboxStore
	PiCfg         PiConfig
	HbCfg         HeartbeatConfig
	TriggerPrompt string
	SendReply     func(ctx context.Context, conversationID string, text string, replyToID string)
	SetTyping     func(ctx context.Context, conversationID string, typing bool)
	OnToolCall    func(ToolCallEvent)
}

// NewWorker creates a new worker. The pi process is started lazily on first dequeue.
func NewWorker(cfg WorkerConfig) *Worker {
	return &Worker{
		inbox:         cfg.Inbox,
		piCfg:         cfg.PiCfg,
		hbPrompt:      cfg.HbCfg.Prompt,
		triggerPrompt: cfg.TriggerPrompt,
		sendReply:     cfg.SendReply,
		setTyping:     cfg.SetTyping,
		onToolCall:    cfg.OnToolCall,
		lastUse:       time.Now(),
	}
}

// Run is the main worker loop. It blocks until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	slog.Info("worker: started")

	// Drain any items left from a previous crash.
	w.drainOnce(ctx)

	for {
		select {
		case <-ctx.Done():
			w.stopPi()
			slog.Info("worker: stopped")

			return
		case <-w.inbox.Wake():
			w.drainOnce(ctx)
		}
	}
}

// Abort cancels the currently running operation, if any.
// Used by the !stop command.
func (w *Worker) Abort() bool {
	w.inbox.mu.Lock()
	cancel := w.inbox.currentCancel
	w.inbox.mu.Unlock()

	if cancel != nil {
		cancel()

		return true
	}

	return false
}

// IsActive returns true if a pi process is alive.
func (w *Worker) IsActive() bool {
	return w.pi != nil && w.pi.IsAlive()
}

// Restart kills the current pi process. The next inbox item will start a fresh one.
func (w *Worker) Restart() {
	w.stopPi()
}

// Compact enqueues a compact operation and waits for the result.
// It goes through the inbox so it's properly serialized with prompts.
func (w *Worker) Compact(ctx context.Context) (*CompactResult, error) {
	if w.pi == nil || !w.pi.IsAlive() {
		return nil, errors.New("no active session")
	}

	ch := make(chan compactOutcome, 1)
	w.compactResult = ch

	if err := w.inbox.Enqueue(ctx, PriorityUser, sourceCompact, "", ""); err != nil {
		w.compactResult = nil

		return nil, fmt.Errorf("enqueuing compact: %w", err)
	}

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("compact cancelled: %w", ctx.Err())
	case outcome := <-ch:
		return outcome.result, outcome.err
	}
}

// SetRoomID sets the room ID for this worker. Called when the first user
// message arrives and we know the conversation ID.
func (w *Worker) SetRoomID(roomID string) {
	w.roomID = roomID
}

// SkillsSummary returns a formatted list of loaded skill paths.
func (w *Worker) SkillsSummary() string {
	skills := w.piCfg.Skills
	if len(skills) == 0 {
		return "No skills loaded."
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%d skill(s) loaded:\n", len(skills)))

	for _, s := range skills {
		sb.WriteString(fmt.Sprintf("- %s\n", filepath.Base(s)))
	}

	return sb.String()
}

// StartIdleReaper runs a goroutine that kills the pi process
// if no user messages have been processed for the configured timeout.
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
				if w.pi != nil && w.pi.IsAlive() && time.Since(w.lastUse) > w.piCfg.IdleTimeout {
					slog.Info("worker: reaping idle pi process")
					w.stopPi()
				}
			}
		}
	}()
}

// drainOnce processes items from the inbox until it's empty.
func (w *Worker) drainOnce(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}

		item, err := w.inbox.Dequeue(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			return // inbox empty
		}

		if err != nil {
			if ctx.Err() == nil {
				slog.Error("worker: dequeue failed", "error", err)
			}

			return
		}

		w.processItem(ctx, item)
	}
}

func (w *Worker) processItem(ctx context.Context, item Inbox) {
	slog.Info("worker: processing", "source", item.Source, "priority", item.Priority, "id", item.ID)

	itemCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	w.inbox.SetRunning(item.Priority, cancel)
	defer w.inbox.ClearRunning()

	if item.Source == sourceCompact {
		w.processCompact(itemCtx)

		return
	}

	w.processPrompt(itemCtx, item)
}

func (w *Worker) processPrompt(ctx context.Context, item Inbox) {
	prompt, ok := w.buildPrompt(item)
	if !ok {
		return
	}

	convID := w.resolveRoomID()
	if convID == "" {
		if item.Source == sourceTrigger {
			slog.Error("worker: no room ID for trigger, requeueing")
			w.inbox.Requeue(context.Background(), item) //nolint:contextcheck // need a live ctx to persist
		} else {
			slog.Error("worker: no room ID available, dropping item", "source", item.Source)
		}

		return
	}

	w.setTyping(ctx, convID, true)
	defer w.setTyping(ctx, convID, false)

	pi, err := w.ensurePi(ctx)
	if err != nil {
		slog.Error("worker: failed to start pi", "source", item.Source, "error", err)

		if item.Source == sourceUser {
			w.sendReply(ctx, convID, fmt.Sprintf("Error starting AI backend: %v", err), "")
		}

		return
	}

	reply, err := pi.sendAndWait(ctx, prompt)
	if err != nil {
		w.handlePromptError(ctx, item, convID, err)

		return
	}

	if item.Source == sourceUser {
		w.lastUse = time.Now()

		if reply == "" {
			reply = w.retryEmptyResponse(ctx, pi)
		}
	}

	if shouldSuppressReply(reply, item.Source) {
		return
	}

	w.sendReply(ctx, convID, reply, item.ReplyTo)
}

// buildPrompt constructs the pi prompt for an inbox item.
// Returns false if the item should be skipped (e.g. empty heartbeat).
func (w *Worker) buildPrompt(item Inbox) (string, bool) {
	switch item.Source {
	case sourceUser:
		return item.Content, true
	case sourceTrigger:
		return buildTriggerPrompt(w.triggerPrompt, item.Content), true
	case sourceHeartbeat:
		content := w.readHeartbeatContent()
		if isEffectivelyEmpty(content) {
			return "", false
		}

		return buildHeartbeatPrompt(w.hbPrompt, content), true
	default:
		return "", false
	}
}

func (w *Worker) handlePromptError(ctx context.Context, item Inbox, convID string, err error) {
	if wasPreempted(ctx, err) {
		slog.Info("worker: preempted", "source", item.Source)

		if item.Source != sourceHeartbeat {
			w.inbox.Requeue(context.Background(), item) //nolint:contextcheck // item ctx is cancelled; need a live ctx to persist
		}

		return
	}

	slog.Error("worker: pi prompt failed", "source", item.Source, "error", err)
	w.stopPi()

	if item.Source == sourceUser {
		w.sendReply(ctx, convID, fmt.Sprintf("Error: %v", err), "")
	}
}

func (w *Worker) processCompact(ctx context.Context) {
	ch := w.compactResult
	w.compactResult = nil

	if ch == nil {
		slog.Warn("worker: compact item but no result channel")

		return
	}

	if w.pi == nil || !w.pi.IsAlive() {
		ch <- compactOutcome{err: errors.New("no active session")}

		return
	}

	result, err := w.pi.Compact(ctx)
	ch <- compactOutcome{result: result, err: err}
}

// wasPreempted returns true if the error is due to context cancellation.
func wasPreempted(ctx context.Context, err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		ctx.Err() != nil
}

func (w *Worker) ensurePi(ctx context.Context) (*PiProcess, error) {
	if w.pi != nil && w.pi.IsAlive() {
		return w.pi, nil
	}

	roomID := w.resolveRoomID()

	pi, err := StartPi(ctx, w.piCfg, roomID)
	if err != nil {
		return nil, err
	}

	pi.onToolCall = w.onToolCall
	w.pi = pi

	return pi, nil
}

func (w *Worker) stopPi() {
	if w.pi != nil {
		slog.Info("worker: stopping pi process")
		w.pi.Kill()
		w.pi = nil
	}
}

func (w *Worker) resolveRoomID() string {
	if w.roomID != "" {
		return w.roomID
	}

	path := filepath.Join(w.piCfg.SessionDir, ".room_id")

	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}

	w.roomID = strings.TrimSpace(string(data))

	return w.roomID
}

func (w *Worker) readHeartbeatContent() string {
	heartbeatPath := filepath.Join(w.piCfg.SessionDir, "HEARTBEAT.md")

	heartbeatContent, err := os.ReadFile(heartbeatPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("worker: failed to read HEARTBEAT.md", "path", heartbeatPath, "error", err)
	}

	return strings.TrimSpace(string(heartbeatContent))
}

func (w *Worker) retryEmptyResponse(ctx context.Context, pi *PiProcess) string {
	slog.Warn("worker: empty response, re-prompting for summary")

	reply, err := pi.sendAndWait(ctx, "You just completed a task but your response contained no text for the user. Please briefly summarize what you did or respond to the user's message.")
	if err != nil {
		slog.Error("worker: re-prompt failed", "error", err)

		return "(I completed some actions but failed to generate a summary.)"
	}

	if reply == "" {
		return "(empty response)"
	}

	return reply
}
