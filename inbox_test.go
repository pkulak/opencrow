package main

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// newTestInbox creates an InboxStore backed by an in-memory SQLite DB.
func newTestInbox(ctx context.Context, t *testing.T) *InboxStore {
	t.Helper()

	db, err := sql.Open("sqlite", ":memory:?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { db.Close() })

	inbox, err := NewInboxStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}

	return inbox
}

func TestInbox_PriorityOrder(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	inbox := newTestInbox(ctx, t)

	// Enqueue in reverse priority order.
	must(t, inbox.Enqueue(ctx, PriorityHeartbeat, sourceHeartbeat, "", ""))
	must(t, inbox.Enqueue(ctx, PriorityTrigger, sourceTrigger, "event data", ""))
	must(t, inbox.Enqueue(ctx, PriorityUser, sourceUser, "urgent msg", ""))

	// Should dequeue in priority order: user, trigger, heartbeat.
	item1, _ := inbox.Dequeue(ctx)
	if item1.Source != sourceUser {
		t.Errorf("first dequeue: Source = %q, want %q", item1.Source, sourceUser)
	}

	item2, _ := inbox.Dequeue(ctx)
	if item2.Source != sourceTrigger {
		t.Errorf("second dequeue: Source = %q, want %q", item2.Source, sourceTrigger)
	}

	item3, _ := inbox.Dequeue(ctx)
	if item3.Source != sourceHeartbeat {
		t.Errorf("third dequeue: Source = %q, want %q", item3.Source, sourceHeartbeat)
	}
}

func TestInbox_PreemptsLowerPriority(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	inbox := newTestInbox(ctx, t)

	// Simulate a heartbeat running.
	cancelled := make(chan struct{})

	inbox.SetRunning(PriorityHeartbeat, func() {
		close(cancelled)
	})

	// Enqueue a user message — should preempt.
	must(t, inbox.Enqueue(ctx, PriorityUser, sourceUser, "urgent", ""))

	select {
	case <-cancelled:
		// good — the heartbeat was cancelled
	case <-time.After(1 * time.Second):
		t.Fatal("preemption did not cancel the running operation")
	}
}

func TestInbox_NoPreemptSamePriority(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	inbox := newTestInbox(ctx, t)

	preempted := false

	inbox.SetRunning(PriorityUser, func() {
		preempted = true
	})

	// Another user message should NOT preempt the currently running user message.
	must(t, inbox.Enqueue(ctx, PriorityUser, sourceUser, "second", ""))

	if preempted {
		t.Error("same-priority enqueue should not preempt")
	}
}

func TestInbox_ClearsStaleHeartbeatsOnInit(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	db, err := sql.Open("sqlite", ":memory:?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}

	defer db.Close()

	// Create table and insert a stale heartbeat.
	if _, err := db.ExecContext(ctx, inboxSchema); err != nil {
		t.Fatal(err)
	}

	if _, err := db.ExecContext(ctx, "INSERT INTO inbox (priority, source, content) VALUES (2, 'heartbeat', '')"); err != nil {
		t.Fatal(err)
	}

	// Also insert a trigger that should survive.
	if _, err := db.ExecContext(ctx, "INSERT INTO inbox (priority, source, content) VALUES (1, 'trigger', 'keep me')"); err != nil {
		t.Fatal(err)
	}

	inbox, err := NewInboxStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}

	count, _ := inbox.Count(ctx)
	if count != 1 {
		t.Fatalf("count = %d, want 1 (heartbeat should be cleared, trigger kept)", count)
	}

	item, _ := inbox.Dequeue(ctx)
	if item.Source != sourceTrigger {
		t.Errorf("surviving item source = %q, want %q", item.Source, sourceTrigger)
	}
}

func TestInbox_Persistence(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dir := t.TempDir()
	dbPath := dir + "/test.db?_journal_mode=WAL&_busy_timeout=5000"

	// First store: enqueue items.
	db1, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}

	inbox1, err := NewInboxStore(ctx, db1)
	if err != nil {
		t.Fatal(err)
	}

	must(t, inbox1.Enqueue(ctx, PriorityTrigger, sourceTrigger, "survived crash", ""))
	db1.Close()

	// Second store: items should still be there.
	db2, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}

	defer db2.Close()

	inbox2, err := NewInboxStore(ctx, db2)
	if err != nil {
		t.Fatal(err)
	}

	count, _ := inbox2.Count(ctx)
	if count != 1 {
		t.Fatalf("count after reopen = %d, want 1", count)
	}

	item, _ := inbox2.Dequeue(ctx)
	if item.Content != "survived crash" {
		t.Errorf("Content = %q, want %q", item.Content, "survived crash")
	}
}

func must(t *testing.T, err error) {
	t.Helper()

	if err != nil {
		t.Fatal(err)
	}
}
