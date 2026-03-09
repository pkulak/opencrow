package main

import (
	"context"
	"log/slog"
	"strings"
	"time"
)

// HeartbeatScheduler periodically enqueues heartbeat markers into the inbox.
// The worker reads HEARTBEAT.md at dequeue time and decides whether to
// prompt pi.
type HeartbeatScheduler struct {
	inbox *InboxStore
	cfg   HeartbeatConfig
}

// NewHeartbeatScheduler creates a new heartbeat scheduler.
func NewHeartbeatScheduler(inbox *InboxStore, cfg HeartbeatConfig) *HeartbeatScheduler {
	return &HeartbeatScheduler{
		inbox: inbox,
		cfg:   cfg,
	}
}

// Start begins the heartbeat loop. Stops when ctx is cancelled.
func (h *HeartbeatScheduler) Start(ctx context.Context) {
	if h.cfg.Interval <= 0 {
		slog.Info("heartbeat disabled (interval not set)")

		return
	}

	slog.Info("heartbeat scheduler started", "interval", h.cfg.Interval)

	go func() {
		ticker := time.NewTicker(h.cfg.Interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.enqueueHeartbeat(ctx)
			}
		}
	}()
}

// enqueueHeartbeat inserts a heartbeat marker into the inbox.
func (h *HeartbeatScheduler) enqueueHeartbeat(ctx context.Context) {
	if err := h.inbox.Enqueue(ctx, PriorityHeartbeat, "heartbeat", "", ""); err != nil {
		slog.Error("heartbeat: failed to enqueue", "error", err)
	}
}

func buildHeartbeatPrompt(basePrompt, content string) string {
	var prompt strings.Builder

	prompt.WriteString(basePrompt)

	if !isEffectivelyEmpty(content) {
		prompt.WriteString("\n\n--- HEARTBEAT.md contents ---\n")
		prompt.WriteString(content)
		prompt.WriteString("\n--- end HEARTBEAT.md ---")
	}

	return prompt.String()
}

// isEffectivelyEmpty returns true if the content contains only headers,
// blank lines, and empty list items.
func isEffectivelyEmpty(content string) bool {
	if content == "" {
		return true
	}

	for line := range strings.SplitSeq(content, "\n") {
		line = strings.TrimSpace(line)

		if line == "" || isMarkdownHeader(line) || isEmptyListItem(line) {
			continue
		}

		// Found a non-empty, non-structural line
		return false
	}

	return true
}

func isMarkdownHeader(line string) bool {
	return strings.HasPrefix(line, "#")
}

func isEmptyListItem(line string) bool {
	// Bare bullet markers
	if line == "-" || line == "*" || line == "+" {
		return true
	}

	// Bullet followed by only whitespace
	for _, prefix := range []string{"- ", "* ", "+ "} {
		if after, ok := strings.CutPrefix(line, prefix); ok {
			return strings.TrimSpace(after) == ""
		}
	}

	return false
}

// shouldSuppressReply returns true if the reply should not be forwarded
// to the user — either because it contains HEARTBEAT_OK or is empty.
// The label is used for log messages (e.g. "heartbeat", "trigger").
func shouldSuppressReply(reply, label string) bool {
	if strings.Contains(reply, "HEARTBEAT_OK") {
		slog.Info(label + ": HEARTBEAT_OK, suppressing")

		return true
	}

	if reply == "" {
		slog.Info(label + ": empty response, suppressing")

		return true
	}

	return false
}
