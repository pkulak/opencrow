package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/robfig/cron/v3"
)

// reminderTick is how often we poll the reminders table for due items.
// Independent of the heartbeat interval so one-shot reminders fire with
// reasonable precision even when heartbeat is set to 30m or disabled.
const (
	reminderTick        = 1 * time.Minute
	recurringInboxLimit = 5
)

// startHeartbeat runs two background loops:
//   - a reminder dispatcher (every reminderTick) that fires due one-shot and
//     recurring reminders as trigger items
//   - a heartbeat ticker (every cfg.Interval, if > 0) that enqueues a
//     heartbeat marker so the worker sends the configured heartbeat prompt
func startHeartbeat(ctx context.Context, w *Worker, cfg HeartbeatConfig) {
	go reminderLoop(ctx, w)

	if cfg.Interval <= 0 {
		slog.Info("heartbeat disabled (interval not set)")

		return
	}

	slog.Info("heartbeat scheduler started", "interval", cfg.Interval)

	go func() {
		ticker := time.NewTicker(cfg.Interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				inserted, err := w.inbox.EnqueueHeartbeat(ctx)
				if err != nil {
					slog.Error("heartbeat: failed to enqueue", "error", err)

					continue
				}

				if !inserted {
					slog.Debug("heartbeat: skipping, one already queued")

					continue
				}

				w.Notify(PriorityHeartbeat)
			}
		}
	}()
}

// reminderLoop polls the reminder tables and enqueues any due reminders as
// trigger items. One-shot reminders are deleted when claimed. Recurring
// reminders only fire when their cron expression matches the current minute;
// missed minutes are intentionally not replayed.
func reminderLoop(ctx context.Context, w *Worker) {
	slog.Info("reminder dispatcher started", "tick", reminderTick)

	ticker := time.NewTicker(reminderTick)
	defer ticker.Stop()

	// Fire once immediately so already-due reminders don't wait a full tick
	// after process start.
	dispatchDueReminders(ctx, w)
	dispatchRecurringReminders(ctx, w, time.Now())

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			dispatchDueReminders(ctx, w)
			dispatchRecurringReminders(ctx, w, time.Now())
		}
	}
}

func dispatchRecurringReminders(ctx context.Context, w *Worker, now time.Time) {
	reminders, err := w.inbox.queries.ListRecurringReminders(ctx)
	if err != nil {
		slog.Error("recurring reminder: failed to list reminders", "error", err)

		return
	}

	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

	for _, reminder := range reminders {
		scheduledAt, matches, removeReason := matchRecurringReminder(parser, reminder, now)
		if removeReason != "" {
			removeRecurringReminder(ctx, w, reminder, removeReason)

			continue
		}

		if !matches {
			continue
		}

		inboxSize, err := w.inbox.Count(ctx)
		if err != nil {
			slog.Error("recurring reminder: failed to count inbox, skipping", "id", reminder.ID, "error", err)

			continue
		}

		if inboxSize >= recurringInboxLimit {
			slog.Info("recurring reminder: inbox full, skipping", "id", reminder.ID, "inbox_size", inboxSize)

			continue
		}

		content := fmt.Sprintf(
			"Recurring reminder:\nSeries ID: %d\nCron: %s\nTimezone: %s\nScheduled for: %s\nDispatched at: %s\nTo cancel future occurrences, use remind_cron_cancel with id %d.\n\nReminder:\n%s",
			reminder.ID,
			reminder.Cron,
			reminder.Timezone,
			scheduledAt.Format(time.RFC3339),
			now.UTC().Format(time.RFC3339),
			reminder.ID,
			reminder.Prompt,
		)

		if err := w.inbox.Enqueue(ctx, PriorityTrigger, sourceTrigger, content, "", ""); err != nil {
			slog.Error("recurring reminder: failed to enqueue, skipping", "id", reminder.ID, "error", err)

			continue
		}

		slog.Info("recurring reminder: firing", "id", reminder.ID, "scheduled_at", scheduledAt)
		w.Notify(PriorityTrigger)
	}
}

func matchRecurringReminder(parser cron.Parser, reminder RecurringReminders, now time.Time) (time.Time, bool, string) {
	if len(strings.Fields(reminder.Cron)) != 5 {
		return time.Time{}, false, "invalid cron expression"
	}

	if reminder.Timezone == "" || reminder.Timezone == "Local" {
		return time.Time{}, false, "invalid timezone"
	}

	location, err := time.LoadLocation(reminder.Timezone)
	if err != nil {
		return time.Time{}, false, "invalid timezone"
	}

	scheduledAt := now.In(location).Truncate(time.Minute)

	if reminder.EndAt.Valid {
		endAt, err := time.Parse(time.RFC3339, reminder.EndAt.String)
		if err != nil {
			return time.Time{}, false, "invalid end time"
		}

		if scheduledAt.After(endAt) {
			return time.Time{}, false, "end time passed"
		}
	}

	schedule, err := parser.Parse(reminder.Cron)
	if err != nil {
		return time.Time{}, false, "invalid cron expression"
	}

	return scheduledAt, schedule.Next(scheduledAt.Add(-time.Minute)).Equal(scheduledAt), ""
}

func removeRecurringReminder(ctx context.Context, w *Worker, reminder RecurringReminders, reason string) {
	if err := w.inbox.queries.DeleteRecurringReminder(ctx, reminder.ID); err != nil {
		slog.Error("recurring reminder: failed to delete", "id", reminder.ID, "reason", reason, "error", err)

		return
	}

	slog.Info("recurring reminder: deleted", "id", reminder.ID, "reason", reason)
}

func dispatchDueReminders(ctx context.Context, w *Worker) {
	now := time.Now().UTC().Format(time.RFC3339)

	due, err := w.inbox.queries.DueReminders(ctx, now)
	if err != nil {
		slog.Error("reminder: failed to query due reminders", "error", err)

		return
	}

	for _, r := range due {
		slog.Info("reminder: firing", "id", r.ID, "fire_at", r.FireAt)

		content := fmt.Sprintf("Reminder (set for %s): %s", r.FireAt, r.Prompt)

		if err := w.inbox.Enqueue(ctx, PriorityTrigger, sourceTrigger, content, "", ""); err != nil {
			// DueReminders is DELETE…RETURNING, so the row is already gone.
			// Re-insert it so the next tick retries instead of silently
			// dropping the reminder.
			slog.Error("reminder: failed to enqueue, re-inserting", "id", r.ID, "error", err)

			if rerr := w.inbox.queries.InsertReminder(ctx, InsertReminderParams{
				FireAt: r.FireAt,
				Prompt: r.Prompt,
			}); rerr != nil {
				slog.Error("reminder: re-insert failed, reminder lost", "id", r.ID, "error", rerr)
			}

			continue
		}

		w.Notify(PriorityTrigger)
	}
}

// parseHeartbeatItems extracts active checklist items from HEARTBEAT.md.
// Only `- text` lines count; `- [paused] text` is skipped. Everything else
// (headers, blank lines, prose) is ignored. No completed/priority metadata —
// obsolete checks are deleted, not marked.
func parseHeartbeatItems(content string) []string {
	var items []string

	for line := range strings.SplitSeq(content, "\n") {
		text, ok := strings.CutPrefix(strings.TrimSpace(line), "- ")
		if !ok {
			continue
		}

		text = strings.TrimSpace(text)
		if text == "" || strings.HasPrefix(text, "[paused]") {
			continue
		}

		items = append(items, text)
	}

	return items
}

func buildHeartbeatPrompt(basePrompt string, items []string) string {
	var sb strings.Builder

	sb.WriteString(basePrompt)
	sb.WriteString("\n\nStanding checks:\n")

	for _, it := range items {
		sb.WriteString("- ")
		sb.WriteString(it)
		sb.WriteByte('\n')
	}

	return sb.String()
}

// shouldSuppressReply returns true if the reply should not be forwarded
// to the user. Each source type has its own sentinel to prevent the model
// from cross-contaminating — a heartbeat can only be silenced by
// HEARTBEAT_OK, while triggers and user messages can only be silenced by
// NO_REPLY.
func shouldSuppressReply(reply, source string) bool {
	if source == sourceHeartbeat && strings.Contains(reply, "HEARTBEAT_OK") {
		slog.Info(source + ": HEARTBEAT_OK, suppressing")

		return true
	}

	if source != sourceHeartbeat {
		firstLine, _, _ := strings.Cut(strings.TrimSpace(reply), "\n")
		if strings.TrimSpace(firstLine) == "NO_REPLY" {
			slog.Info(source + ": NO_REPLY, suppressing")

			return true
		}
	}

	if reply == "" {
		slog.Info(source + ": empty response, suppressing")

		return true
	}

	return false
}
