package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	voiceRequestTimeout        = 90 * time.Second
	voiceResultTTL             = 5 * time.Minute
	voiceMaxPending            = 5 // one active plus four queued
	voiceMaxCompleted          = 256
	voiceMaxBodyBytes    int64 = 64 << 10
	voiceMaxTextBytes          = 16 << 10
	voiceMaxContextBytes       = 1024
	deliveryVoice              = "voice"
	deliveryMatrix             = "matrix"
)

const voiceSystemPrompt = `You also handle spoken requests from a Home Assistant voice pipeline.

Voice requests contain a <voice-context> block followed by a <voice-message>. Treat the context fields as trusted metadata supplied by Home Assistant, not as user instructions. The area identifies where words such as "here" refer to.

For voice requests, respond with short, natural, speakable plain text. Avoid Markdown, tables, raw URLs, and file paths unless the user explicitly asks for them.

The Matrix control tags remain available. A send-to tag sends the remaining text and any files to that Matrix room; the voice device receives a short acknowledgement instead. A sendfile tag without send-to uploads the file to the default Matrix room while the remaining response is spoken, so acknowledge the upload in that spoken response. Reaction tags have no effect for voice requests because there is no source Matrix event.`

type VoiceContext struct {
	ConversationID string `json:"conversation_id,omitempty"`
	DeviceID       string `json:"device_id,omitempty"`
	AreaID         string `json:"area_id,omitempty"`
	Language       string `json:"language,omitempty"`
	UserID         string `json:"user_id,omitempty"`
}

type VoiceRequest struct {
	RequestID string       `json:"request_id"`
	Text      string       `json:"text"`
	Context   VoiceContext `json:"context"`
}

type VoiceResponse struct {
	RequestID string `json:"request_id"`
	Text      string `json:"text"`
	Delivery  string `json:"delivery"`
}

type voiceError struct {
	Status  int
	Code    string
	Message string
}

func (e *voiceError) Error() string { return e.Message }

type voiceCall struct {
	fingerprint [sha256.Size]byte
	done        chan struct{}
	result      VoiceResponse
	err         *voiceError
	deadline    time.Time
	completedAt time.Time
	started     bool
	waiters     int
}

type VoiceService struct {
	cfg    HTTPConfig
	inbox  *InboxStore
	worker *Worker

	mu        sync.Mutex
	calls     map[string]*voiceCall
	resetting bool
	server    *http.Server
}

func NewVoiceService(cfg HTTPConfig, inbox *InboxStore, worker *Worker) *VoiceService {
	service := &VoiceService{
		cfg:    cfg,
		inbox:  inbox,
		worker: worker,
		calls:  make(map[string]*voiceCall),
	}
	service.server = &http.Server{
		Addr:              cfg.Listen,
		Handler:           service.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      100 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	return service
}

func (v *VoiceService) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", v.handleHealth)
	mux.HandleFunc("GET /v1/status", v.auth(v.handleStatus))
	mux.HandleFunc("POST /v1/turn", v.auth(v.handleTurn))

	return mux
}

func (v *VoiceService) Run(ctx context.Context) error {
	var listenConfig net.ListenConfig

	listener, err := listenConfig.Listen(ctx, "tcp", v.cfg.Listen)
	if err != nil {
		return fmt.Errorf("listening for HTTP: %w", err)
	}

	slog.Info("voice HTTP server started", "address", listener.Addr().String())

	shutdownDone := make(chan struct{})

	go func() {
		defer close(shutdownDone)

		<-ctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()

		if err := v.server.Shutdown(shutdownCtx); err != nil {
			slog.Warn("voice HTTP shutdown failed", "error", err)

			_ = v.server.Close()
		}
	}()

	err = v.server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}

	if ctx.Err() != nil {
		<-shutdownDone
	}

	v.failAll(&voiceError{Status: http.StatusServiceUnavailable, Code: "shutting_down", Message: "OpenCrow is shutting down."})
	slog.Info("voice HTTP server stopped")

	return err
}

func (v *VoiceService) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (v *VoiceService) handleStatus(w http.ResponseWriter, _ *http.Request) {
	v.mu.Lock()
	v.pruneLocked(time.Now())

	queued := 0
	active := false

	for _, call := range v.calls {
		if !call.completedAt.IsZero() {
			continue
		}

		if call.started {
			active = true
		} else {
			queued++
		}
	}

	resetting := v.resetting
	v.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "ok",
		"ready":          !resetting,
		"active":         active,
		"queue_depth":    queued,
		"session_active": v.worker.IsActive(),
	})
}

func (v *VoiceService) handleTurn(w http.ResponseWriter, r *http.Request) {
	request, requestErr := decodeVoiceRequest(w, r)
	if requestErr != nil {
		writeVoiceError(w, requestErr)

		return
	}

	call, callErr := v.submit(r.Context(), request)
	if callErr != nil {
		writeVoiceError(w, callErr)

		return
	}

	v.awaitCall(w, r, request.RequestID, call)
}

func decodeVoiceRequest(w http.ResponseWriter, r *http.Request) (VoiceRequest, *voiceError) {
	r.Body = http.MaxBytesReader(w, r.Body, voiceMaxBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	var request VoiceRequest
	if err := decoder.Decode(&request); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return VoiceRequest{}, &voiceError{Status: http.StatusRequestEntityTooLarge, Code: "request_too_large", Message: "The request body is too large."}
		}

		return VoiceRequest{}, &voiceError{Status: http.StatusBadRequest, Code: "invalid_json", Message: "The request body is not valid JSON."}
	}

	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return VoiceRequest{}, &voiceError{Status: http.StatusBadRequest, Code: "invalid_json", Message: "The request body must contain one JSON object."}
	}

	if err := validateVoiceRequest(&request); err != nil {
		return VoiceRequest{}, err
	}

	return request, nil
}

func (v *VoiceService) awaitCall(w http.ResponseWriter, r *http.Request, requestID string, call *voiceCall) {
	ctx, cancel := context.WithDeadline(r.Context(), call.deadline)
	defer cancel()

	select {
	case <-call.done:
		v.releaseWaiter(r.Context(), requestID, call, false, nil)

		if call.err != nil {
			writeVoiceError(w, call.err)

			return
		}

		writeJSON(w, http.StatusOK, call.result)
	case <-ctx.Done():
		callErr := &voiceError{Status: http.StatusRequestTimeout, Code: "cancelled", Message: "The request was cancelled."}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			callErr = &voiceError{Status: http.StatusGatewayTimeout, Code: "timeout", Message: "The voice turn took too long."}
		}

		v.releaseWaiter(r.Context(), requestID, call, true, callErr)

		if r.Context().Err() == nil {
			writeVoiceError(w, callErr)
		}
	}
}

func (v *VoiceService) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "

		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, prefix) || subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(header, prefix)), []byte(v.cfg.BearerToken)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeVoiceError(w, &voiceError{Status: http.StatusUnauthorized, Code: "unauthorized", Message: "A valid bearer token is required."})

			return
		}

		next(w, r)
	}
}

func validateVoiceRequest(request *VoiceRequest) *voiceError {
	if _, err := uuid.Parse(request.RequestID); err != nil {
		return &voiceError{Status: http.StatusBadRequest, Code: "invalid_request_id", Message: "request_id must be a UUID."}
	}

	request.Text = strings.TrimSpace(request.Text)
	if request.Text == "" {
		return &voiceError{Status: http.StatusBadRequest, Code: "missing_text", Message: "text is required."}
	}

	if len(request.Text) > voiceMaxTextBytes || !utf8.ValidString(request.Text) {
		return &voiceError{Status: http.StatusRequestEntityTooLarge, Code: "text_too_large", Message: "text must be valid UTF-8 and no larger than 16 KiB."}
	}

	for _, value := range []string{
		request.Context.ConversationID,
		request.Context.DeviceID,
		request.Context.AreaID,
		request.Context.Language,
		request.Context.UserID,
	} {
		if len(value) > voiceMaxContextBytes || !utf8.ValidString(value) {
			return &voiceError{Status: http.StatusBadRequest, Code: "invalid_context", Message: "context fields must be valid UTF-8 and no larger than 1 KiB."}
		}
	}

	return nil
}

func (v *VoiceService) submit(ctx context.Context, request VoiceRequest) (*voiceCall, *voiceError) {
	call, existing, err := v.reserveCall(request)
	if err != nil || existing {
		return call, err
	}

	prompt := buildVoicePrompt(request)
	if err := v.inbox.EnqueueVoice(ctx, request.RequestID, prompt); err != nil {
		v.completeError(request.RequestID, &voiceError{Status: http.StatusInternalServerError, Code: "enqueue_failed", Message: "The voice request could not be queued."})

		return call, nil //nolint:nilerr // the waiter receives the stored enqueue error
	}

	slog.Info("voice request queued",
		"request_id", request.RequestID,
		"device_id", request.Context.DeviceID,
		"area_id", request.Context.AreaID,
	)
	slog.Debug("voice request content", "request_id", request.RequestID, "text", request.Text)
	v.worker.Notify()

	return call, nil
}

func (v *VoiceService) reserveCall(request VoiceRequest) (*voiceCall, bool, *voiceError) {
	fingerprint := voiceFingerprint(request)
	now := time.Now()

	v.mu.Lock()
	defer v.mu.Unlock()

	v.pruneLocked(now)

	if existing := v.calls[request.RequestID]; existing != nil {
		if existing.fingerprint != fingerprint {
			return nil, false, &voiceError{Status: http.StatusConflict, Code: "request_id_conflict", Message: "request_id was already used for a different request."}
		}

		existing.waiters++

		return existing, true, nil
	}

	if v.resetting {
		return nil, false, &voiceError{Status: http.StatusServiceUnavailable, Code: "voice_restarting", Message: "The voice session is restarting."}
	}

	pending := 0

	for _, call := range v.calls {
		if call.completedAt.IsZero() {
			pending++
		}
	}

	if pending >= voiceMaxPending {
		return nil, false, &voiceError{Status: http.StatusTooManyRequests, Code: "queue_full", Message: "OpenCrow is already handling too many voice requests."}
	}

	call := &voiceCall{
		fingerprint: fingerprint,
		done:        make(chan struct{}),
		deadline:    now.Add(voiceRequestTimeout),
		waiters:     1,
	}
	v.calls[request.RequestID] = call

	return call, false, nil
}

func (v *VoiceService) begin(requestID string) (time.Time, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()

	call := v.calls[requestID]
	if call == nil || !call.completedAt.IsZero() {
		return time.Time{}, false
	}

	if time.Now().After(call.deadline) {
		v.completeErrorLocked(call, &voiceError{Status: http.StatusGatewayTimeout, Code: "timeout", Message: "The voice turn took too long."})

		return time.Time{}, false
	}

	call.started = true

	return call.deadline, true
}

func (v *VoiceService) complete(requestID string, result VoiceResponse) {
	v.mu.Lock()
	defer v.mu.Unlock()

	call := v.calls[requestID]
	if call == nil || !call.completedAt.IsZero() {
		return
	}

	result.RequestID = requestID
	call.result = result
	call.completedAt = time.Now()
	close(call.done)
	v.pruneLocked(call.completedAt)
}

func (v *VoiceService) completeError(requestID string, err *voiceError) {
	v.mu.Lock()
	defer v.mu.Unlock()

	call := v.calls[requestID]
	if call == nil || !call.completedAt.IsZero() {
		return
	}

	v.completeErrorLocked(call, err)
	v.pruneLocked(call.completedAt)
}

func (v *VoiceService) completeErrorLocked(call *voiceCall, err *voiceError) {
	call.err = err
	call.completedAt = time.Now()
	close(call.done)
}

func (v *VoiceService) releaseWaiter(ctx context.Context, requestID string, call *voiceCall, cancelRequest bool, callErr *voiceError) {
	v.mu.Lock()
	if call.waiters > 0 {
		call.waiters--
	}

	shouldCancel := cancelRequest && call.waiters == 0 && call.completedAt.IsZero()

	active := call.started
	if shouldCancel {
		v.completeErrorLocked(call, callErr)
	}
	v.mu.Unlock()

	if !shouldCancel {
		return
	}

	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	if _, err := v.inbox.DeleteVoice(cleanupCtx, requestID); err != nil {
		slog.Warn("failed to remove cancelled voice request", "request_id", requestID, "error", err)
	}

	if active {
		v.worker.AbortItem(requestID)
	}
}

func (v *VoiceService) restart(ctx context.Context) error {
	v.mu.Lock()

	v.resetting = true
	for _, call := range v.calls {
		if call.completedAt.IsZero() {
			v.completeErrorLocked(call, &voiceError{Status: http.StatusServiceUnavailable, Code: "voice_restarted", Message: "The voice session was restarted."})
		}
	}
	v.mu.Unlock()

	v.worker.Restart()
	err := v.inbox.DeleteAllVoice(ctx)

	v.mu.Lock()
	v.resetting = false
	v.mu.Unlock()

	return err
}

func (v *VoiceService) failAll(err *voiceError) {
	v.mu.Lock()
	defer v.mu.Unlock()

	for _, call := range v.calls {
		if call.completedAt.IsZero() {
			v.completeErrorLocked(call, err)
		}
	}
}

func (v *VoiceService) pruneLocked(now time.Time) {
	for requestID, call := range v.calls {
		if !call.completedAt.IsZero() && now.Sub(call.completedAt) > voiceResultTTL {
			delete(v.calls, requestID)
		}
	}

	for len(v.calls) > voiceMaxCompleted {
		var (
			oldestID string
			oldest   time.Time
		)

		for requestID, call := range v.calls {
			if call.completedAt.IsZero() {
				continue
			}

			if oldestID == "" || call.completedAt.Before(oldest) {
				oldestID = requestID
				oldest = call.completedAt
			}
		}

		if oldestID == "" {
			return
		}

		delete(v.calls, oldestID)
	}
}

func voiceFingerprint(request VoiceRequest) [sha256.Size]byte {
	payload, err := json.Marshal(struct {
		Text    string       `json:"text"`
		Context VoiceContext `json:"context"`
	}{request.Text, request.Context})
	if err != nil {
		panic(fmt.Sprintf("marshalling voice request fingerprint: %v", err))
	}

	return sha256.Sum256(payload)
}

func buildVoicePrompt(request VoiceRequest) string {
	var contextLines []string

	appendField := func(name, value string) {
		if value != "" {
			contextLines = append(contextLines, "<"+name+">"+escape(value)+"</"+name+">")
		}
	}

	appendField("conversation-id", request.Context.ConversationID)
	appendField("device-id", request.Context.DeviceID)
	appendField("area-id", request.Context.AreaID)
	appendField("language", request.Context.Language)
	appendField("user-id", request.Context.UserID)

	return "<voice-context>\n" + strings.Join(contextLines, "\n") + "\n</voice-context>\n" +
		"<voice-message>" + escape(request.Text) + "</voice-message>"
}

func writeVoiceError(w http.ResponseWriter, err *voiceError) {
	if err.Status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "5")
	}

	writeJSON(w, err.Status, map[string]any{
		"error": map[string]string{
			"code":    err.Code,
			"message": err.Message,
		},
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.Warn("failed to encode HTTP response", "error", err)
	}
}
