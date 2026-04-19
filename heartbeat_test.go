package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseHeartbeatItems(t *testing.T) {
	t.Parallel()

	content := `# Heading
- Check email
-
- [paused] Review old PRs
  - Indented item
prose line
- Review calendar
`
	// Write to WorkingDir and give the worker a distinct SessionDir to
	// guard against a regression where HEARTBEAT.md was looked up in
	// SessionDir (pi's jsonl storage) instead of the agent's cwd.
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "HEARTBEAT.md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	w := &Worker{piCfg: PiConfig{WorkingDir: workDir, SessionDir: t.TempDir()}}

	got := parseHeartbeatItems(w.readHeartbeatFile())
	want := []string{"Check email", "Indented item", "Review calendar"}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}

	if items := parseHeartbeatItems("# only headers\n\n"); items != nil {
		t.Errorf("empty file: got %q, want nil", items)
	}
}

func TestShouldSuppressReply(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		reply  string
		source string
		want   bool
	}{
		// HEARTBEAT_OK only works for heartbeats.
		{"heartbeat ok", "HEARTBEAT_OK", sourceHeartbeat, true},
		{"heartbeat ok in text", "all clear HEARTBEAT_OK done", sourceHeartbeat, true},
		{"heartbeat ok for trigger", "HEARTBEAT_OK", sourceTrigger, false},
		{"heartbeat ok for user", "HEARTBEAT_OK", sourceUser, false},

		// NO_REPLY only works for triggers and user messages.
		{"no reply for trigger", "NO_REPLY", sourceTrigger, true},
		{"no reply for user", "NO_REPLY", sourceUser, true},
		{"no reply in text for trigger", "nothing here NO_REPLY", sourceTrigger, true},
		{"no reply for heartbeat", "NO_REPLY", sourceHeartbeat, false},

		// Normal replies are never suppressed.
		{"normal trigger reply", "You have 3 new emails", sourceTrigger, false},
		{"normal user reply", "Here's the weather", sourceUser, false},
		{"normal heartbeat reply", "Found urgent email", sourceHeartbeat, false},

		// Empty replies are always suppressed.
		{"empty heartbeat", "", sourceHeartbeat, true},
		{"empty trigger", "", sourceTrigger, true},
		{"empty user", "", sourceUser, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := shouldSuppressReply(tt.reply, tt.source)
			if got != tt.want {
				t.Errorf("shouldSuppressReply(%q, %q) = %v, want %v", tt.reply, tt.source, got, tt.want)
			}
		})
	}
}

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
	item, err := inbox.Dequeue(ctx)
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
