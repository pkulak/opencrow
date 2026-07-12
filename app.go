package main

import (
	"context"
	"database/sql"
	"fmt"
	"html"
	"log/slog"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/pinpox/opencrow/matrix"
)

var (
	sendFileRe = regexp.MustCompile(`<sendfile>\s*(.*?)\s*</sendfile>`)
	sendToRe   = regexp.MustCompile(`<send-to>\s*(.*?)\s*</send-to>`)
	reactRe    = regexp.MustCompile(`(?m)(?:^[\t ]*|[\t ]+)<react[\t ]+id="([^"\r\n]+)">([^\r\n]*)</react>[\t ]*$`)
)

const maxReactionBytes = 64

type reactionRequest struct {
	messageID string
	emoji     string
}

type appMatrix interface {
	SendMessage(ctx context.Context, conversationID, text, replyToID string) string
	SendFile(ctx context.Context, conversationID, filePath string) error
	SendReaction(ctx context.Context, conversationID, messageID, emoji string) error
	ResetConversation(ctx context.Context, conversationID string)
	SystemPromptExtra() string
}

// extractReaction finds standalone or line-trailing <react> control tags,
// strips all of them, and returns the first valid request. Tags followed by
// prose are ignored to avoid activating syntax that is only being discussed.
func extractReaction(text string) (string, *reactionRequest) {
	matches := reactRe.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return text, nil
	}

	if len(matches) > 1 {
		slog.Warn("multiple reaction tags in agent response; using first valid tag", "count", len(matches))
	}

	var reaction *reactionRequest

	for _, match := range matches {
		messageID := strings.TrimSpace(html.UnescapeString(match[1]))
		emoji := strings.TrimSpace(html.UnescapeString(match[2]))

		if reaction == nil && messageID != "" && emoji != "" && len(emoji) <= maxReactionBytes {
			reaction = &reactionRequest{messageID: messageID, emoji: emoji}
		}
	}

	cleaned := reactRe.ReplaceAllString(text, "")

	return strings.TrimSpace(cleaned), reaction
}

// extractSendFiles finds all <sendfile>/path</sendfile> tags in text,
// returns the cleaned text with tags stripped and the list of file paths.
func extractSendFiles(text string) (string, []string) {
	matches := sendFileRe.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return text, nil
	}

	var paths []string

	for _, m := range matches {
		p := strings.TrimSpace(m[1])
		if p != "" {
			paths = append(paths, p)
		}
	}

	cleaned := sendFileRe.ReplaceAllString(text, "")
	cleaned = strings.TrimSpace(cleaned)

	return cleaned, paths
}

// extractSendTo finds a <send-to>room-id</send-to> tag in text,
// returns the cleaned text with the tag stripped and the target room ID.
// If multiple tags are present, the first one wins.
func extractSendTo(text string) (string, string) {
	match := sendToRe.FindStringSubmatch(text)
	if match == nil {
		return text, ""
	}

	roomID := strings.TrimSpace(match[1])
	cleaned := sendToRe.ReplaceAllString(text, "")
	cleaned = strings.TrimSpace(cleaned)

	return cleaned, roomID
}

// App orchestrates command handling, inbox enqueueing, and file extraction.
type App struct {
	matrix appMatrix
	worker *Worker
	inbox  *InboxStore
	outbox *outboxStore
}

// NewApp creates a new App. The db connection is shared with the inbox
// and owned by the caller.
func NewApp(matrixClient appMatrix, worker *Worker, inbox *InboxStore, db *sql.DB) *App {
	return &App{
		matrix: matrixClient,
		worker: worker,
		inbox:  inbox,
		outbox: newOutboxStore(db),
	}
}

// HandleMessage dispatches commands and enqueues normal Matrix messages.
func (a *App) HandleMessage(ctx context.Context, msg matrix.Message) {
	// Record the incoming message so future reply-to references can quote it.
	a.outbox.Put(ctx, msg.ConversationID, msg.MessageID, msg.Text)

	switch strings.TrimSpace(msg.Text) {
	case "!help":
		a.handleHelp(ctx, msg)
	case "!restart":
		a.handleRestart(ctx, msg)
	case "!stop":
		a.handleStop(ctx, msg)
	case "!compact":
		a.handleCompact(ctx, msg)
	case "!skills":
		a.handleSkills(ctx, msg)
	default:
		a.handlePrompt(ctx, msg)
	}
}

func (a *App) handleHelp(ctx context.Context, msg matrix.Message) {
	help := "Available commands:\n" +
		"  !help    — Show this help message\n" +
		"  !restart — Kill the current session and start fresh\n" +
		"  !stop    — Abort the currently running agent turn\n" +
		"  !compact — Compact conversation context to reduce token usage\n" +
		"  !skills  — List loaded skills"
	a.matrix.SendMessage(ctx, msg.ConversationID, help, "")
}

func (a *App) handleRestart(ctx context.Context, msg matrix.Message) {
	a.matrix.ResetConversation(ctx, msg.ConversationID)
	a.worker.Restart()
	a.matrix.SendMessage(ctx, msg.ConversationID, "Session restarted. Next message starts a fresh session (previous context discarded).", "")
}

func (a *App) handleStop(ctx context.Context, msg matrix.Message) {
	if !a.worker.IsActive() {
		a.matrix.SendMessage(ctx, msg.ConversationID, "No active session.", "")

		return
	}

	if a.worker.Abort() {
		a.matrix.SendMessage(ctx, msg.ConversationID, "Aborted current operation.", "")
	} else {
		a.matrix.SendMessage(ctx, msg.ConversationID, "Nothing running to stop.", "")
	}
}

func (a *App) handleCompact(ctx context.Context, msg matrix.Message) {
	if !a.worker.IsActive() {
		a.matrix.SendMessage(ctx, msg.ConversationID, "No active session to compact.", "")

		return
	}

	result, err := a.worker.Compact(ctx)
	if err != nil {
		slog.Error("compact failed", "conversation", msg.ConversationID, "error", err)
		a.matrix.SendMessage(ctx, msg.ConversationID, fmt.Sprintf("Compaction failed: %v", err), "")

		return
	}

	reply := fmt.Sprintf("Compacted conversation (was %d tokens).\nSummary: %s", result.TokensBefore, result.Summary)
	a.matrix.SendMessage(ctx, msg.ConversationID, reply, "")
}

func (a *App) handleSkills(ctx context.Context, msg matrix.Message) {
	a.matrix.SendMessage(ctx, msg.ConversationID, a.worker.SkillsSummary(), "")
}

func (a *App) handlePrompt(ctx context.Context, msg matrix.Message) {
	a.worker.SetRoomID(msg.ConversationID)

	promptText := a.buildPromptText(ctx, msg)

	if err := a.inbox.EnqueueUser(ctx, promptText, msg.ReplyToID, msg.ConversationID, msg.MessageID, !msg.IsDM); err != nil {
		slog.Error("failed to enqueue user message", "error", err)
		a.matrix.SendMessage(ctx, msg.ConversationID, fmt.Sprintf("Error: %v", err), "")

		return
	}

	a.worker.Notify(PriorityUser)
}

// buildPromptText prepends context tags and reply-quote context to the message text.
func (a *App) buildPromptText(ctx context.Context, msg matrix.Message) string {
	promptText := msg.Text

	if msg.ReplyToID != "" {
		if quoted := a.outbox.Get(ctx, msg.ConversationID, msg.ReplyToID); quoted != "" {
			promptText = fmt.Sprintf("[user replied to message: %q]\n%s", quoted, promptText)
		} else {
			promptText = "[user replied to a message whose content is unavailable — ask for clarification if their message is unclear]\n" + promptText
		}
	}

	tags := buildContextTags(msg)
	if msg.MessageID != "" {
		messageIDTag := "<message-id>" + escape(msg.MessageID) + "</message-id>"
		if tags == "" {
			tags = messageIDTag
		} else {
			tags += "\n" + messageIDTag
		}
	}

	if tags != "" {
		promptText = tags + "\n\n" + promptText
	}

	return promptText
}

// buildContextTags returns a block of XML-style context tags derived from the
// message's enrichment fields. Each non-zero field produces one line; zero
// values are omitted entirely. Returns an empty string if no fields are set.
func buildContextTags(msg matrix.Message) string {
	var lines []string

	if msg.SenderID != "" {
		lines = append(lines, "<from-id>"+escape(msg.SenderID)+"</from-id>")
	}

	if msg.ConversationID != "" {
		lines = append(lines, "<room-id>"+escape(msg.ConversationID)+"</room-id>")
	}

	lines = append(lines, "<is-dm>"+strconv.FormatBool(msg.IsDM)+"</is-dm>")

	if msg.SenderName != "" {
		lines = append(lines, "<from-name>"+escape(msg.SenderName)+"</from-name>")
	}

	if msg.RoomName != "" && !msg.IsDM {
		lines = append(lines, "<room-name>"+escape(msg.RoomName)+"</room-name>")
	}

	if msg.RoomSize > 0 && !msg.IsDM {
		lines = append(lines, "<room-size>"+strconv.Itoa(msg.RoomSize)+"</room-size>")
	}

	return strings.Join(lines, "\n")
}

func escape(s string) string {
	return html.EscapeString(s)
}

// sendReaction validates that the requested message is known in the current
// conversation before sending it to Matrix.
func (a *App) sendReaction(ctx context.Context, conversationID string, reaction reactionRequest) {
	if a.outbox.Get(ctx, conversationID, reaction.messageID) == "" {
		slog.Warn("ignoring reaction to unknown message",
			"conversation", conversationID,
			"message", reaction.messageID,
		)

		return
	}

	if err := a.matrix.SendReaction(ctx, conversationID, reaction.messageID, reaction.emoji); err != nil {
		slog.Warn("failed to send reaction",
			"conversation", conversationID,
			"message", reaction.messageID,
			"error", err,
		)
	}
}

// sendReplyWithFiles extracts <sendfile> tags, uploads each file, and
// sends the final text reply.
func (a *App) sendReplyWithFiles(ctx context.Context, conversationID, reply, replyToID string) {
	slog.Info("sending reply", "conversation", conversationID, "len", len(reply))
	slog.Debug("outgoing reply content", "conversation", conversationID, "content", reply)

	cleanReply, filePaths := extractSendFiles(reply)

	var fileSendErrors strings.Builder

	for _, fp := range filePaths {
		slog.Info("sending file", "conversation", conversationID, "path", fp)

		if err := a.matrix.SendFile(ctx, conversationID, fp); err != nil {
			slog.Error("failed to send file", "conversation", conversationID, "path", fp, "error", err)
			fmt.Fprintf(&fileSendErrors, "\n\n(failed to send file %s: %v)", filepath.Base(fp), err)
		}
	}

	cleanReply += fileSendErrors.String()

	if cleanReply != "" {
		sentID := a.matrix.SendMessage(ctx, conversationID, cleanReply, replyToID)
		a.outbox.Put(ctx, conversationID, sentID, cleanReply)
	}
}

// formatToolCall produces a Matrix-formatted summary of a tool invocation.
func formatToolCall(evt ToolCallEvent) string {
	switch evt.ToolName {
	case "bash":
		return formatBashCall(evt)
	case "read":
		return formatPathCall(evt, "📄 reading", "file")
	case "edit":
		return formatPathCall(evt, "✏️ editing", "file")
	case "write":
		return formatPathCall(evt, "📝 writing", "file")
	default:
		return "🔧 " + evt.ToolName
	}
}

func formatBashCall(evt ToolCallEvent) string {
	cmd, ok := evt.Args["command"].(string)
	if !ok {
		return "🔧 bash"
	}

	return fmt.Sprintf("🔧\n```sh\n%s\n```", cmd)
}

func formatPathCall(evt ToolCallEvent, prefix, fallback string) string {
	p, ok := evt.Args["path"].(string)
	if !ok {
		return prefix + " " + fallback
	}

	return prefix + " `" + p + "`"
}

// systemPrompt returns the full system prompt including Matrix-specific context.
func (a *App) systemPrompt(basePrompt string) string {
	extra := a.matrix.SystemPromptExtra()
	if extra == "" {
		return basePrompt
	}

	return strings.TrimRight(basePrompt, "\n") + "\n\n" + extra
}
