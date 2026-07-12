package main

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/pinpox/opencrow/matrix"
)

const (
	testRoom           = "!room1"
	reactionSourceRoom = "!source:matrix.org"
	reactionEventID    = "$event"
)

// mockMatrix records Matrix calls for testing.
type mockMatrix struct {
	mu                    sync.Mutex
	sentMessages          []sentMessage
	sentFiles             []sentFile
	typingCalls           []typingCall
	resetCalls            []string
	reactions             []reactionCall
	systemPromptExtraText string
}

type sentMessage struct {
	conversationID string
	text           string
}

type sentFile struct {
	conversationID string
	filePath       string
}

type typingCall struct {
	conversationID string
	typing         bool
}

type reactionCall struct {
	conversationID string
	messageID      string
	emoji          string
}

func (m *mockMatrix) SendReaction(_ context.Context, conversationID, messageID, emoji string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.reactions = append(m.reactions, reactionCall{conversationID, messageID, emoji})

	return nil
}

func (m *mockMatrix) SendMessage(_ context.Context, conversationID string, text string, _ string) string {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.sentMessages = append(m.sentMessages, sentMessage{conversationID, text})

	return ""
}

func (m *mockMatrix) SendFile(_ context.Context, conversationID string, filePath string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.sentFiles = append(m.sentFiles, sentFile{conversationID, filePath})

	return nil
}

func (m *mockMatrix) SetTyping(_ context.Context, conversationID string, typing bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.typingCalls = append(m.typingCalls, typingCall{conversationID, typing})
}

func (m *mockMatrix) ResetConversation(_ context.Context, conversationID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.resetCalls = append(m.resetCalls, conversationID)
}

func (m *mockMatrix) SystemPromptExtra() string {
	return m.systemPromptExtraText
}

type testMatrix interface {
	appMatrix
	workerMatrix
}

// newTestApp creates a mock Matrix client and App wired together for testing.
func newTestApp(t *testing.T) (*App, *mockMatrix) {
	t.Helper()

	return newTestAppWithMatrix(t, &mockMatrix{})
}

func newTestAppWithMatrix(t *testing.T, matrixClient *mockMatrix) (*App, *mockMatrix) {
	t.Helper()

	return newTestAppForMatrix(t, matrixClient), matrixClient
}

func newTestAppForMatrix(t *testing.T, matrixClient testMatrix) *App {
	t.Helper()

	ctx := context.Background()
	db := newTestDB(ctx, t)
	inbox := newTestInboxWithDB(ctx, t, db)

	worker := NewWorker(inbox, PiConfig{SessionDir: t.TempDir()}, "", "")
	worker.SetMatrix(matrixClient)

	app := NewApp(matrixClient, worker, inbox, db)
	worker.SetApp(app)

	return app
}

// sendCommand sends a command message from a default user to testRoom.
func sendCommand(app *App, command string) {
	app.HandleMessage(context.Background(), matrix.Message{
		ConversationID: testRoom,
		SenderID:       "@user:example.com",
		Text:           command,
	})
}

func TestExtractReaction(t *testing.T) {
	t.Parallel()

	longEmoji := strings.Repeat("x", maxReactionBytes+1)

	cases := []struct {
		name          string
		input         string
		wantClean     string
		wantMessageID string
		wantEmoji     string
	}{
		{"no tag", "hello", "hello", "", ""},
		{"reaction only", `<react id="$event">👍</react>`, "", "$event", "👍"},
		{"reaction with text", "Hello\n<react id=\"$event\"> ❤️ </react>", "Hello", "$event", "❤️"},
		{"HTML escaped id", `<react id="$a&amp;b">👍</react>`, "", "$a&b", "👍"},
		{"multiple uses first valid", "<react id=\"$first\">👍</react>\nText\n<react id=\"$second\">❤️</react>", "Text", "$first", "👍"},
		{"empty emoji stripped", `<react id="$event"> </react>`, "", "", ""},
		{"oversized emoji stripped", `<react id="$event">` + longEmoji + `</react>`, "", "", ""},
		{"inline tag ignored", `Show <react id="$event">👍</react> literally`, `Show <react id="$event">👍</react> literally`, "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			gotClean, gotReaction := extractReaction(tc.input)
			if gotClean != tc.wantClean {
				t.Errorf("clean = %q, want %q", gotClean, tc.wantClean)
			}

			if tc.wantMessageID == "" {
				if gotReaction != nil {
					t.Fatalf("reaction = %+v, want nil", gotReaction)
				}

				return
			}

			if gotReaction == nil {
				t.Fatal("reaction is nil")
			}

			if gotReaction.messageID != tc.wantMessageID {
				t.Errorf("messageID = %q, want %q", gotReaction.messageID, tc.wantMessageID)
			}

			if gotReaction.emoji != tc.wantEmoji {
				t.Errorf("emoji = %q, want %q", gotReaction.emoji, tc.wantEmoji)
			}
		})
	}
}

func TestExtractSendTo(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		input     string
		wantClean string
		wantRoom  string
	}{
		{"no tag", "hello world", "hello world", ""},
		{"simple tag", "<send-to>!other:matrix.org</send-to>Hello", "Hello", "!other:matrix.org"},
		{"whitespace around room", "<send-to>  !devchat:matrix.org  </send-to>\nHi", "Hi", "!devchat:matrix.org"},
		{"multiple tags", "<send-to>!first:matrix.org</send-to>\n<send-to>!second:matrix.org</send-to>Text", "Text", "!first:matrix.org"},
		{"empty tag", "<send-to>  </send-to>Text", "Text", ""},
		{"tag in middle", "Start\n<send-to>!room:matrix.org</send-to>\nEnd", "Start\n\nEnd", "!room:matrix.org"},
		{"combined with sendfile", "<send-to>!room:matrix.org</send-to>\n<sendfile>/x</sendfile>Text", "<sendfile>/x</sendfile>Text", "!room:matrix.org"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			gotClean, gotRoom := extractSendTo(tc.input)
			if gotClean != tc.wantClean {
				t.Errorf("clean = %q, want %q", gotClean, tc.wantClean)
			}

			if gotRoom != tc.wantRoom {
				t.Errorf("room = %q, want %q", gotRoom, tc.wantRoom)
			}
		})
	}
}

// TestApp_Commands covers the !-commands that reply with a single message.
// Each case only differs in the input command and what substrings the
// reply must contain, so a table avoids repeating the setup/assert
// boilerplate five times.
func TestApp_Commands(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		command      string
		wantContains []string
		wantReset    bool
	}{
		{"stop no session", "!stop", []string{"No active session"}, false},
		{"compact no session", "!compact", []string{"No active session"}, false},
		{"compact trailing whitespace", "!compact ", []string{"No active session"}, false},
		{"help trailing newline", "!help\n", []string{"!help", "!restart"}, false},
		{"help", "!help", []string{"!help", "!restart", "!stop", "!compact", "!skills"}, false},
		{"restart", "!restart", []string{"Session restarted"}, true},
		{"skills", "!skills", []string{"No skills loaded"}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			app, mb := newTestApp(t)
			sendCommand(app, tc.command)

			mb.mu.Lock()
			defer mb.mu.Unlock()

			if len(mb.sentMessages) != 1 {
				t.Fatalf("sent %d messages, want 1", len(mb.sentMessages))
			}

			msg := mb.sentMessages[0]
			if msg.conversationID != testRoom {
				t.Errorf("sent to %q, want %q", msg.conversationID, testRoom)
			}

			for _, want := range tc.wantContains {
				if !strings.Contains(msg.text, want) {
					t.Errorf("reply %q missing %q", msg.text, want)
				}
			}

			if tc.wantReset {
				if len(mb.resetCalls) != 1 || mb.resetCalls[0] != testRoom {
					t.Errorf("ResetConversation calls = %v, want [%s]", mb.resetCalls, testRoom)
				}
			} else if len(mb.resetCalls) != 0 {
				t.Errorf("ResetConversation calls = %v, want none", mb.resetCalls)
			}
		})
	}
}

func TestApp_PromptEnqueuesInbox(t *testing.T) {
	t.Parallel()

	app, _ := newTestApp(t)

	app.HandleMessage(context.Background(), matrix.Message{
		ConversationID: testRoom,
		SenderID:       "@user:example.com",
		Text:           "hello world",
		MessageID:      "msg-1",
	})

	ctx := context.Background()

	count, err := app.inbox.Count(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if count != 1 {
		t.Fatalf("inbox count = %d, want 1", count)
	}

	item, err := app.inbox.Dequeue(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if item.Source != "user" {
		t.Errorf("Source = %q, want %q", item.Source, "user")
	}

	if item.Priority != PriorityUser {
		t.Errorf("Priority = %d, want %d", item.Priority, PriorityUser)
	}

	if !strings.Contains(item.Content, "hello world") {
		t.Errorf("Content = %q, want to contain %q", item.Content, "hello world")
	}

	if !strings.Contains(item.Content, "<from-id>@user:example.com</from-id>") {
		t.Errorf("Content = %q, want to contain <from-id> tag", item.Content)
	}

	if item.ConversationID != testRoom {
		t.Errorf("ConversationID = %q, want %q", item.ConversationID, testRoom)
	}
}

func TestApp_BuildPromptText_ReplyToUserMessage(t *testing.T) {
	t.Parallel()

	app, _ := newTestApp(t)

	ctx := context.Background()

	// Record a user message as HandleMessage would.
	app.outbox.Put(ctx, "conv1", "user-msg-123", "original question")

	// Now simulate the user replying to their own message.
	replyMsg := matrix.Message{
		ConversationID: "conv1",
		SenderID:       "user1",
		Text:           "follow-up",
		MessageID:      "user-msg-456",
		ReplyToID:      "user-msg-123",
	}

	got := app.buildPromptText(ctx, replyMsg)

	// Should contain the reply-quote context and original text.
	if !strings.Contains(got, `[user replied to message: "original question"]`) {
		t.Errorf("buildPromptText missing reply quote, got: %q", got)
	}

	if !strings.Contains(got, "follow-up") {
		t.Errorf("buildPromptText missing follow-up text, got: %q", got)
	}

	// Should also contain context tags from the enriched Message.
	if !strings.Contains(got, "<from-id>user1</from-id>") {
		t.Errorf("buildPromptText missing from-id tag, got: %q", got)
	}
}

func TestBuildContextTags(t *testing.T) {
	t.Parallel()

	t.Run("no enrichment fields", func(t *testing.T) {
		t.Parallel()

		msg := matrix.Message{Text: "hello"}
		got := buildContextTags(msg)
		// IsDM is always emitted, even when false.
		want := "<is-dm>false</is-dm>"
		if got != want {
			t.Errorf("buildContextTags = %q, want %q", got, want)
		}
	})

	t.Run("all fields set", func(t *testing.T) {
		t.Parallel()

		msg := matrix.Message{
			ConversationID: "!room:matrix.org",
			SenderID:       "@alice:matrix.org",
			SenderName:     "Alice",
			RoomName:       "Dev Chat",
			RoomSize:       5,
			IsDM:           false,
		}
		got := buildContextTags(msg)

		checks := []string{
			"<from-id>@alice:matrix.org</from-id>",
			"<room-id>!room:matrix.org</room-id>",
			"<is-dm>false</is-dm>",
			"<from-name>Alice</from-name>",
			"<room-name>Dev Chat</room-name>",
			"<room-size>5</room-size>",
		}

		for _, want := range checks {
			if !strings.Contains(got, want) {
				t.Errorf("buildContextTags missing %q\ngot: %q", want, got)
			}
		}
	})

	t.Run("partial fields — only universal fields set", func(t *testing.T) {
		t.Parallel()

		msg := matrix.Message{
			ConversationID: "abcdef1234",
			SenderID:       "abcdef1234",
			IsDM:           true,
		}
		got := buildContextTags(msg)

		checks := []string{
			"<from-id>abcdef1234</from-id>",
			"<room-id>abcdef1234</room-id>",
			"<is-dm>true</is-dm>",
		}

		for _, want := range checks {
			if !strings.Contains(got, want) {
				t.Errorf("buildContextTags missing %q\ngot: %q", want, got)
			}
		}

		// Matrix-only fields should be absent.
		for _, absent := range []string{"from-name", "room-name", "room-size"} {
			if strings.Contains(got, absent) {
				t.Errorf("buildContextTags should not contain %q\ngot: %q", absent, got)
			}
		}
	})

	t.Run("is-dm always present", func(t *testing.T) {
		t.Parallel()

		msg := matrix.Message{Text: "hello"}
		got := buildContextTags(msg)

		if !strings.Contains(got, "<is-dm>") {
			t.Errorf("buildContextTags missing <is-dm>, got: %q", got)
		}
	})

	t.Run("room-name and room-size omitted when is-dm is true", func(t *testing.T) {
		t.Parallel()

		msg := matrix.Message{
			ConversationID: "!dm:matrix.org",
			SenderID:       "@bob:matrix.org",
			RoomName:       "My DM",
			RoomSize:       2,
			IsDM:           true,
		}
		got := buildContextTags(msg)

		for _, absent := range []string{"room-size", "room-name"} {
			if strings.Contains(got, absent) {
				t.Errorf("%s should be omitted for DMs, got: %q", absent, got)
			}
		}

		if !strings.Contains(got, "<is-dm>true</is-dm>") {
			t.Errorf("missing <is-dm>true</is-dm>, got: %q", got)
		}
	})

	t.Run("tag contents are escaped", func(t *testing.T) {
		t.Parallel()

		msg := matrix.Message{
			ConversationID: "!room<&>:matrix.org",
			SenderID:       "@alice<&>:matrix.org",
			SenderName:     "Alice <admin> & \"owner\"",
			RoomName:       "Dev <Chat> & Friends",
			RoomSize:       3,
		}
		got := buildContextTags(msg)

		checks := []string{
			"<from-id>@alice&lt;&amp;&gt;:matrix.org</from-id>",
			"<room-id>!room&lt;&amp;&gt;:matrix.org</room-id>",
			"<from-name>Alice &lt;admin&gt; &amp; &#34;owner&#34;</from-name>",
			"<room-name>Dev &lt;Chat&gt; &amp; Friends</room-name>",
		}

		for _, want := range checks {
			if !strings.Contains(got, want) {
				t.Errorf("buildContextTags missing escaped %q\ngot: %q", want, got)
			}
		}
	})
}

func TestBuildPromptText_ContextTags(t *testing.T) {
	t.Parallel()

	app, _ := newTestApp(t)
	ctx := context.Background()

	msg := matrix.Message{
		ConversationID: "!room:matrix.org",
		SenderID:       "@alice:matrix.org",
		Text:           "hello there",
		IsDM:           false,
	}

	got := app.buildPromptText(ctx, msg)

	// Should contain context tags followed by a blank line then the text.
	if !strings.Contains(got, "<from-id>@alice:matrix.org</from-id>") {
		t.Errorf("buildPromptText missing <from-id>, got: %q", got)
	}

	if !strings.Contains(got, "hello there") {
		t.Errorf("buildPromptText missing original text, got: %q", got)
	}

	// Verify structure: tags block, blank line, then content.
	if !strings.Contains(got, "\n\nhello there") {
		t.Errorf("buildPromptText should have blank line before content, got: %q", got)
	}
}

func TestBuildPromptText_IncludesMessageID(t *testing.T) {
	t.Parallel()

	msg := matrix.Message{
		ConversationID: "!room:matrix.org",
		SenderID:       "@alice:matrix.org",
		MessageID:      "$event<&>",
		Text:           "hello",
	}

	app, _ := newTestApp(t)

	got := app.buildPromptText(context.Background(), msg)
	if !strings.Contains(got, "<message-id>$event&lt;&amp;&gt;</message-id>") {
		t.Errorf("prompt missing escaped message-id, got: %q", got)
	}
}

func TestApp_SendReactionValidatesConversationAndMessage(t *testing.T) {
	t.Parallel()

	matrixClient := &mockMatrix{}
	app := newTestAppForMatrix(t, matrixClient)
	ctx := context.Background()

	app.outbox.Put(ctx, reactionSourceRoom, reactionEventID, "hello")
	app.sendReaction(ctx, reactionSourceRoom, reactionRequest{messageID: reactionEventID, emoji: "👍"})
	app.sendReaction(ctx, "!other:matrix.org", reactionRequest{messageID: reactionEventID, emoji: "❤️"})
	app.sendReaction(ctx, reactionSourceRoom, reactionRequest{messageID: "$unknown", emoji: "❤️"})

	matrixClient.mu.Lock()
	defer matrixClient.mu.Unlock()

	if len(matrixClient.reactions) != 1 {
		t.Fatalf("reactions = %+v, want one", matrixClient.reactions)
	}

	got := matrixClient.reactions[0]
	if got.conversationID != reactionSourceRoom || got.messageID != reactionEventID || got.emoji != "👍" {
		t.Errorf("reaction = %+v", got)
	}
}

func TestApp_EnqueuesMessageMetadata(t *testing.T) {
	t.Parallel()

	app, _ := newTestApp(t)
	app.HandleMessage(t.Context(), matrix.Message{
		ConversationID: "!room:matrix.org",
		MessageID:      "$metadata-event",
		Text:           "hello",
		IsDM:           false,
	})

	item, err := app.inbox.Dequeue(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if item.MessageID != "$metadata-event" || !item.IsGroup {
		t.Errorf("message metadata = %+v", item)
	}
}

func TestInbox_ConversationID(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := newTestDB(ctx, t)
	inbox := newTestInboxWithDB(ctx, t, db)

	if err := inbox.Enqueue(ctx, PriorityUser, sourceUser, "hello", "reply-1", "!room:matrix.org"); err != nil {
		t.Fatal(err)
	}

	item, err := inbox.Dequeue(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if item.ConversationID != "!room:matrix.org" {
		t.Errorf("ConversationID = %q, want %q", item.ConversationID, "!room:matrix.org")
	}

	// Items enqueued without a conversation ID should default to empty.
	if err := inbox.Enqueue(ctx, PriorityTrigger, sourceTrigger, "event", "", ""); err != nil {
		t.Fatal(err)
	}

	item2, err := inbox.Dequeue(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if item2.ConversationID != "" {
		t.Errorf("ConversationID = %q, want empty", item2.ConversationID)
	}
}

func TestApp_SystemPrompt(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		extra string
		want  string
	}{
		{"with extra", "Matrix room context.", "Base prompt\n\nMatrix room context."},
		{"no extra", "", "Base prompt"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			app, _ := newTestAppWithMatrix(t, &mockMatrix{systemPromptExtraText: tc.extra})

			if got := app.systemPrompt("Base prompt"); got != tc.want {
				t.Errorf("systemPrompt = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFormatToolCall(t *testing.T) {
	t.Parallel()

	cases := []struct {
		event ToolCallEvent
		want  string
	}{
		{ToolCallEvent{ToolName: "bash", Args: map[string]any{"command": "ls -la"}}, "🔧\n```sh\nls -la\n```"},
		{ToolCallEvent{ToolName: "read", Args: map[string]any{"path": "/etc/hosts"}}, "📄 reading `/etc/hosts`"},
	}

	for _, tc := range cases {
		if got := formatToolCall(tc.event); got != tc.want {
			t.Errorf("formatToolCall(%s) = %q, want %q", tc.event.ToolName, got, tc.want)
		}
	}
}
