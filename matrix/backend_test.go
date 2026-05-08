package matrix

import (
	"testing"
	"time"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

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
