package main

import (
	"context"
	"fmt"
	"testing"
)

func TestSentMessageStore_PutAndGet(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s, err := newSentMessageStore(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	s.Put(ctx, "room1", "msg1", "hello")
	s.Put(ctx, "room1", "msg2", "world")
	s.Put(ctx, "room2", "msg3", "other room")

	if got := s.Get(ctx, "room1", "msg1"); got != "hello" {
		t.Errorf("Get(room1, msg1) = %q, want %q", got, "hello")
	}

	if got := s.Get(ctx, "room1", "msg2"); got != "world" {
		t.Errorf("Get(room1, msg2) = %q, want %q", got, "world")
	}

	if got := s.Get(ctx, "room2", "msg3"); got != "other room" {
		t.Errorf("Get(room2, msg3) = %q, want %q", got, "other room")
	}

	if got := s.Get(ctx, "room1", "nonexistent"); got != "" {
		t.Errorf("Get(room1, nonexistent) = %q, want empty", got)
	}

	if got := s.Get(ctx, "nonexistent", "msg1"); got != "" {
		t.Errorf("Get(nonexistent, msg1) = %q, want empty", got)
	}
}

func TestSentMessageStore_EmptyIDIgnored(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s, err := newSentMessageStore(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	s.Put(ctx, "room1", "", "should be ignored")

	if got := s.Get(ctx, "room1", ""); got != "" {
		t.Errorf("Get(room1, empty) = %q, want empty", got)
	}
}

func TestSentMessageStore_Eviction(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s, err := newSentMessageStore(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Fill beyond the limit.
	for i := range maxSentMessagesPerConversation + 10 {
		s.Put(ctx, "room1", fmt.Sprintf("msg%d", i), fmt.Sprintf("text%d", i))
	}

	// The first 10 should have been evicted.
	for i := range 10 {
		if got := s.Get(ctx, "room1", fmt.Sprintf("msg%d", i)); got != "" {
			t.Errorf("msg%d should have been evicted, got %q", i, got)
		}
	}

	// The rest should still be there.
	for i := 10; i < maxSentMessagesPerConversation+10; i++ {
		want := fmt.Sprintf("text%d", i)
		if got := s.Get(ctx, "room1", fmt.Sprintf("msg%d", i)); got != want {
			t.Errorf("msg%d = %q, want %q", i, got, want)
		}
	}
}

func TestSentMessageStore_DuplicatePutNoCorruption(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s, err := newSentMessageStore(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Insert max entries, then overwrite the first one. This must not
	// create a duplicate in Order, which would break eviction.
	for i := range maxSentMessagesPerConversation {
		s.Put(ctx, "room1", fmt.Sprintf("msg%d", i), fmt.Sprintf("text%d", i))
	}

	// Overwrite msg0 with new text — should update value, not grow Order.
	s.Put(ctx, "room1", "msg0", "updated")

	if got := s.Get(ctx, "room1", "msg0"); got != "updated" {
		t.Errorf("Get(room1, msg0) = %q, want %q", got, "updated")
	}

	// Add one more entry. If Order had a duplicate msg0, two entries
	// would be evicted and msg1 would disappear.
	s.Put(ctx, "room1", "new", "new-text")

	if got := s.Get(ctx, "room1", "msg1"); got != "text1" {
		t.Errorf("msg1 should survive, got %q", got)
	}
}

func TestSentMessageStore_Persistence(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dir := t.TempDir()

	// First store: write some messages.
	s1, err := newSentMessageStore(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s1.Close()

	s1.Put(ctx, "room1", "msg1", "hello")
	s1.Put(ctx, "room1", "msg2", "world")

	// Second store: loads from the same directory.
	s2, err := newSentMessageStore(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	if got := s2.Get(ctx, "room1", "msg1"); got != "hello" {
		t.Errorf("after reload, Get(room1, msg1) = %q, want %q", got, "hello")
	}

	if got := s2.Get(ctx, "room1", "msg2"); got != "world" {
		t.Errorf("after reload, Get(room1, msg2) = %q, want %q", got, "world")
	}
}
