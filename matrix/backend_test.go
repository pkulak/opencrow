package matrix

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func TestSendReaction(t *testing.T) {
	t.Parallel()

	type capturedRequest struct {
		method string
		path   string
		body   map[string]any
		err    error
	}

	requests := make(chan capturedRequest, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any

		err := json.NewDecoder(r.Body).Decode(&body)
		requests <- capturedRequest{method: r.Method, path: r.URL.Path, body: body, err: err}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"event_id":"$reaction"}`))
	}))
	defer server.Close()

	client, err := mautrix.NewClient(server.URL, id.UserID("@bot:example.org"), "token")
	if err != nil {
		t.Fatal(err)
	}

	b := &Backend{client: client}
	if err := b.SendReaction(context.Background(), "!room:example.org", "$event", "👍"); err != nil {
		t.Fatal(err)
	}

	got := <-requests
	if got.err != nil {
		t.Fatalf("decoding request body: %v", got.err)
	}

	if got.method != http.MethodPut {
		t.Errorf("method = %q, want PUT", got.method)
	}

	if !strings.Contains(got.path, "/rooms/!room:example.org/send/m.reaction/") {
		t.Errorf("path = %q", got.path)
	}

	relatesTo, ok := got.body["m.relates_to"].(map[string]any)
	if !ok {
		t.Fatalf("m.relates_to = %#v", got.body["m.relates_to"])
	}

	if relatesTo["event_id"] != "$event" || relatesTo["rel_type"] != "m.annotation" || relatesTo["key"] != "👍" {
		t.Errorf("m.relates_to = %#v", relatesTo)
	}
}

func TestFilterMessageDropsPreJoinHistory(t *testing.T) {
	roomID := id.RoomID("!room:example.org")
	cutoff := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC).UnixMilli()

	tests := []struct {
		name     string
		joinedAt map[id.RoomID]int64
		eventTS  int64
		wantDrop bool
	}{
		{
			name:     "drops messages from before recorded join",
			joinedAt: map[id.RoomID]int64{roomID: cutoff},
			eventTS:  cutoff - 1,
			wantDrop: true,
		},
		{
			name:     "allows message exactly at recorded join",
			joinedAt: map[id.RoomID]int64{roomID: cutoff},
			eventTS:  cutoff,
		},
		{
			name:     "allows messages after recorded join",
			joinedAt: map[id.RoomID]int64{roomID: cutoff},
			eventTS:  cutoff + 1,
		},
		{
			name:     "allows messages in rooms without recorded join",
			joinedAt: map[id.RoomID]int64{},
			eventTS:  cutoff - 1,
		},
		{
			name:     "allows messages without server timestamp",
			joinedAt: map[id.RoomID]int64{roomID: cutoff},
			eventTS:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &Backend{
				userID:   id.UserID("@bot:example.org"),
				joinedAt: tt.joinedAt,
			}
			b.initialSynced.Store(true)

			got := b.filterMessage(&event.Event{
				Sender:    id.UserID("@alice:example.org"),
				RoomID:    roomID,
				Timestamp: tt.eventTS,
				Content: event.Content{Parsed: &event.MessageEventContent{
					MsgType: event.MsgText,
					Body:    "hello",
				}},
			})

			if (got == nil) != tt.wantDrop {
				t.Fatalf("filterMessage() nil = %v, want drop %v", got == nil, tt.wantDrop)
			}
		})
	}
}

func TestRecordRoomJoinKeepsEarliestCutoff(t *testing.T) {
	roomID := id.RoomID("!room:example.org")
	b := &Backend{}

	b.recordRoomJoin(roomID, time.UnixMilli(2000))
	b.recordRoomJoin(roomID, time.UnixMilli(3000))
	b.recordRoomJoin(roomID, time.UnixMilli(1000))

	if got := b.joinedAt[roomID]; got != 1000 {
		t.Fatalf("joinedAt cutoff = %d, want 1000", got)
	}
}

func TestCleanupRoomClearsJoinCutoff(t *testing.T) {
	roomID := id.RoomID("!room:example.org")
	b := &Backend{
		joinedAt: map[id.RoomID]int64{roomID: 1234},
	}

	b.cleanupRoom(string(roomID))

	if _, ok := b.joinedAt[roomID]; ok {
		t.Fatal("cleanupRoom left join cutoff behind")
	}
}
