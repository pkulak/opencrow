package matrix

import (
	"context"
	"encoding/json"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

type videoSendCapture struct {
	t           *testing.T
	uploadTypes []string
	uploadNames []string
	sentContent map[string]any
}

func (capture *videoSendCapture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch {
	case strings.HasSuffix(r.URL.Path, "/upload"):
		capture.uploadTypes = append(capture.uploadTypes, r.Header.Get("Content-Type"))
		capture.uploadNames = append(capture.uploadNames, r.URL.Query().Get("filename"))

		uri := "mxc://example.org/video"
		if len(capture.uploadTypes) == 2 {
			uri = "mxc://example.org/thumbnail"
		}

		writeJSON(capture.t, w, map[string]string{"content_uri": uri})
	case strings.Contains(r.URL.Path, "/send/m.room.message/"):
		if err := json.NewDecoder(r.Body).Decode(&capture.sentContent); err != nil {
			capture.t.Errorf("decoding sent event: %v", err)
		}

		_, _ = w.Write([]byte(`{"event_id":"$video"}`))
	default:
		capture.t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}
}

func TestSendFileIncludesVideoThumbnailAndMetadata(t *testing.T) { //nolint:paralleltest // modifies PATH to install fake media tools
	tempDir := t.TempDir()
	videoPath := filepath.Join(tempDir, "clip.mp4")

	if err := os.WriteFile(videoPath, []byte("video"), 0o600); err != nil {
		t.Fatal(err)
	}

	thumbnailPath := filepath.Join(tempDir, "thumbnail.jpg")
	writeTestThumbnail(t, thumbnailPath)
	installFakeVideoTools(t, tempDir, thumbnailPath)

	capture := &videoSendCapture{t: t}
	server := httptest.NewServer(capture)

	defer server.Close()

	client, err := mautrix.NewClient(server.URL, id.UserID("@bot:example.org"), "token")
	if err != nil {
		t.Fatal(err)
	}

	backend := &Backend{client: client}
	if err := backend.SendFile(context.Background(), "!room:example.org", videoPath); err != nil {
		t.Fatal(err)
	}

	assertVideoUploads(t, capture)
	assertVideoEvent(t, capture.sentContent)
}

func writeTestThumbnail(t *testing.T, path string) {
	t.Helper()

	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := jpeg.Encode(file, image.NewRGBA(image.Rect(0, 0, 320, 180)), nil); err != nil {
		file.Close()
		t.Fatal(err)
	}

	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func installFakeVideoTools(t *testing.T, tempDir, thumbnailPath string) {
	t.Helper()

	binDir := filepath.Join(tempDir, "bin")
	if err := os.Mkdir(binDir, 0o700); err != nil {
		t.Fatal(err)
	}

	writeExecutable(t, filepath.Join(binDir, "ffprobe"), `#!/bin/sh
printf '%s\n' '{"streams":[{"width":636,"height":360}],"format":{"duration":"12.5"}}'
`)
	writeExecutable(t, filepath.Join(binDir, "ffmpeg"), `#!/bin/sh
cat "$FAKE_VIDEO_THUMBNAIL"
`)

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_VIDEO_THUMBNAIL", thumbnailPath)
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil { //nolint:gosec // test helper creates executables
		t.Fatal(err)
	}
}

func assertVideoUploads(t *testing.T, capture *videoSendCapture) {
	t.Helper()

	if len(capture.uploadTypes) != 2 {
		t.Fatalf("upload content types = %v, want video and thumbnail", capture.uploadTypes)
	}

	if capture.uploadTypes[0] != "video/mp4" || capture.uploadTypes[1] != "image/jpeg" {
		t.Errorf("upload content types = %v", capture.uploadTypes)
	}

	if capture.uploadNames[0] != "clip.mp4" || capture.uploadNames[1] != "clip-thumbnail.jpg" {
		t.Errorf("upload filenames = %v", capture.uploadNames)
	}
}

func assertVideoEvent(t *testing.T, sentContent map[string]any) {
	t.Helper()

	if sentContent["msgtype"] != "m.video" {
		t.Errorf("msgtype = %v, want m.video", sentContent["msgtype"])
	}

	info, ok := sentContent["info"].(map[string]any)
	if !ok {
		t.Fatalf("info = %#v", sentContent["info"])
	}

	if info["w"] != float64(636) || info["h"] != float64(360) || info["duration"] != float64(12500) {
		t.Errorf("video metadata = %#v", info)
	}

	if info["thumbnail_url"] != "mxc://example.org/thumbnail" {
		t.Errorf("thumbnail_url = %v", info["thumbnail_url"])
	}

	assertThumbnailInfo(t, info["thumbnail_info"])
}

func assertThumbnailInfo(t *testing.T, value any) {
	t.Helper()

	thumbnailInfo, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("thumbnail_info = %#v", value)
	}

	if thumbnailInfo["mimetype"] != "image/jpeg" || thumbnailInfo["w"] != float64(320) || thumbnailInfo["h"] != float64(180) {
		t.Errorf("thumbnail metadata = %#v", thumbnailInfo)
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()

	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encoding response: %v", err)
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
