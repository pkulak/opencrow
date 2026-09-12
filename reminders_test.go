package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestDispatchDueReminders(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := newTestDB(ctx, t)

	inbox, err := NewInboxStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}

	w := &Worker{inbox: inbox, wake: make(chan struct{}, 1)}

	past := time.Now().UTC().Add(-1 * time.Minute).Format(time.RFC3339)
	future := time.Now().UTC().Add(1 * time.Hour).Format(time.RFC3339)

	if _, err := db.ExecContext(ctx,
		`INSERT INTO reminders (fire_at, prompt) VALUES (?, ?), (?, ?)`,
		past, "due reminder",
		future, "future reminder",
	); err != nil {
		t.Fatal(err)
	}

	dispatchDueReminders(ctx, w)

	// Due reminder should now be a trigger item in the inbox.
	item, err := inbox.DequeueBackground(ctx)
	if err != nil {
		t.Fatalf("expected one inbox item, got error: %v", err)
	}

	if item.Source != sourceTrigger {
		t.Errorf("source = %q, want %q", item.Source, sourceTrigger)
	}

	if want := "due reminder"; !strings.Contains(item.Content, want) {
		t.Errorf("content %q does not contain %q", item.Content, want)
	}

	// Inbox should now be empty (future reminder not dispatched).
	if n, _ := inbox.Count(ctx); n != 0 {
		t.Errorf("inbox count = %d, want 0", n)
	}

	// Future reminder must still be in the table.
	var remaining int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM reminders`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}

	if remaining != 1 {
		t.Errorf("reminders remaining = %d, want 1", remaining)
	}
}

func TestDispatchRecurringReminders(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := newTestDB(ctx, t)
	inbox := newTestInboxWithDB(ctx, t, db)
	w := &Worker{inbox: inbox, wake: make(chan struct{}, 1)}
	now := time.Date(2026, time.June, 1, 19, 0, 30, 0, time.UTC)

	if _, err := db.ExecContext(ctx, `
		INSERT INTO recurring_reminders (cron, timezone, end_at, prompt) VALUES
			('0 12 * * *', 'America/Los_Angeles', '2026-06-01T12:00:00-07:00', 'matching reminder'),
			('1 12 * * *', 'America/Los_Angeles', NULL, 'future reminder'),
			('0 12 * * *', 'America/Los_Angeles', '2026-06-01T11:59:00-07:00', 'expired reminder'),
			('0 12 * * *', 'America/Los_Angeles', 'not-a-time', 'invalid end time'),
			('@daily', 'America/Los_Angeles', NULL, 'invalid cron'),
			('TZ=UTC', 'America/Los_Angeles', NULL, 'malformed timezone prefix'),
			('TZ=UTC 0 12 * * *', 'America/Los_Angeles', NULL, 'timezone prefix'),
			('0 12 * * *', 'Not/A_Timezone', NULL, 'invalid timezone'),
			('0 12 * * *', '', NULL, 'empty timezone'),
			('0 12 * * *', 'Local', NULL, 'local timezone')
	`); err != nil {
		t.Fatal(err)
	}

	dispatchRecurringReminders(ctx, w, now)

	item, err := inbox.DequeueBackground(ctx)
	if err != nil {
		t.Fatalf("expected one inbox item, got error: %v", err)
	}

	for _, want := range []string{
		"Series ID: 1",
		"Cron: 0 12 * * *",
		"Timezone: America/Los_Angeles",
		"Scheduled for: 2026-06-01T12:00:00-07:00",
		"matching reminder",
	} {
		if !strings.Contains(item.Content, want) {
			t.Errorf("content %q does not contain %q", item.Content, want)
		}
	}

	if n, _ := inbox.Count(ctx); n != 0 {
		t.Errorf("inbox count = %d, want 0", n)
	}

	var remaining int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM recurring_reminders`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}

	if remaining != 2 {
		t.Errorf("recurring reminders remaining = %d, want 2", remaining)
	}
}

func TestDispatchRecurringRemindersCapsInbox(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := newTestDB(ctx, t)
	inbox := newTestInboxWithDB(ctx, t, db)
	w := &Worker{inbox: inbox, wake: make(chan struct{}, 1)}
	now := time.Date(2026, time.January, 5, 12, 0, 30, 0, time.UTC)

	for i := 1; i <= 6; i++ {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO recurring_reminders (cron, timezone, prompt) VALUES ('0 12 * * *', 'UTC', ?)`,
			fmt.Sprintf("reminder %d", i),
		); err != nil {
			t.Fatal(err)
		}
	}

	dispatchRecurringReminders(ctx, w, now)

	if n, _ := inbox.Count(ctx); n != recurringInboxLimit {
		t.Errorf("inbox count = %d, want %d", n, recurringInboxLimit)
	}

	for i := int64(1); i <= recurringInboxLimit; i++ {
		item, err := inbox.DequeueBackground(ctx)
		if err != nil {
			t.Fatal(err)
		}

		if want := fmt.Sprintf("Series ID: %d", i); !strings.Contains(item.Content, want) {
			t.Errorf("content %q does not contain %q", item.Content, want)
		}
	}
}

func TestDispatchRecurringRemindersIgnoresChatBacklog(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := newTestDB(ctx, t)
	inbox := newTestInboxWithDB(ctx, t, db)
	w := &Worker{inbox: inbox, wake: make(chan struct{}, 1)}
	now := time.Date(2026, time.January, 5, 12, 0, 30, 0, time.UTC)

	for range recurringInboxLimit {
		if err := inbox.EnqueueUser(ctx, "chat", "", "room", "", false); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := db.ExecContext(ctx,
		`INSERT INTO recurring_reminders (cron, timezone, prompt) VALUES ('0 12 * * *', 'UTC', 'background reminder')`,
	); err != nil {
		t.Fatal(err)
	}

	dispatchRecurringReminders(ctx, w, now)

	item, err := inbox.DequeueBackground(ctx)
	if err != nil {
		t.Fatalf("expected recurring reminder despite chat backlog: %v", err)
	}

	if !strings.Contains(item.Content, "background reminder") {
		t.Errorf("content %q missing reminder prompt", item.Content)
	}
}

// TestDueRemindersTimestampFormats guards against lexicographic-comparison
// bugs when the agent inserts timestamps in ISO 8601 variants that differ
// from the RFC3339 Z-suffix form the dispatcher uses.
func TestDueRemindersTimestampFormats(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := newTestDB(ctx, t)
	q := New(db)

	// All of these are one hour in the past; only the formatting differs.
	past := time.Now().UTC().Add(-1 * time.Hour)
	variants := []string{
		past.Format(time.RFC3339), // 2025-06-15T13:00:00Z
		past.Format("2006-01-02T15:04:05+00:00"),
		past.Format("2006-01-02 15:04:05"), // SQLite's own default
	}

	for _, v := range variants {
		if err := q.InsertReminder(ctx, InsertReminderParams{FireAt: v, Prompt: v}); err != nil {
			t.Fatal(err)
		}
	}

	now := time.Now().UTC().Format(time.RFC3339)

	due, err := q.DueReminders(ctx, now)
	if err != nil {
		t.Fatal(err)
	}

	if len(due) != len(variants) {
		t.Errorf("got %d due, want %d; variants not normalized: %v", len(due), len(variants), variants)
	}
}
