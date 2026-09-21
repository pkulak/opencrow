package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testVoiceToken = "voice-test-token"
	testRequestID  = "35cc6ab3-12fe-4bc5-8dda-31f599d5ce78"
)

func newTestVoiceService(t *testing.T) (*VoiceService, *InboxStore) {
	t.Helper()

	db := newTestDBAt(t.Context(), t, t.TempDir()+"/voice.db")
	inbox := newTestInboxWithDB(t.Context(), t, db)
	worker := NewVoiceWorker(inbox, PiConfig{SessionDir: t.TempDir()})
	service := NewVoiceService(HTTPConfig{BearerToken: testVoiceToken}, inbox, worker)
	worker.SetVoiceService(service)

	return service, inbox
}

func TestVoiceHTTPAuthenticationAndStatus(t *testing.T) {
	t.Parallel()

	service, _ := newTestVoiceService(t)

	health := httptest.NewRecorder()
	service.Handler().ServeHTTP(health, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil))

	if health.Code != http.StatusOK {
		t.Fatalf("health status = %d", health.Code)
	}

	unauthorized := httptest.NewRecorder()
	service.Handler().ServeHTTP(unauthorized, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/status", nil))

	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.Code)
	}

	statusRequest := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/status", nil)
	statusRequest.Header.Set("Authorization", "Bearer "+testVoiceToken)

	status := httptest.NewRecorder()
	service.Handler().ServeHTTP(status, statusRequest)

	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"ready":true`) {
		t.Fatalf("status response = %d %s", status.Code, status.Body.String())
	}
}

func TestVoiceHTTPValidation(t *testing.T) {
	t.Parallel()

	service, _ := newTestVoiceService(t)
	request := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"/v1/turn",
		strings.NewReader(`{"request_id":"not-a-uuid","text":"hello","context":{}}`),
	)
	request.Header.Set("Authorization", "Bearer "+testVoiceToken)

	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_request_id") {
		t.Fatalf("validation response = %d %s", response.Code, response.Body.String())
	}
}

func TestVoiceHTTPTurnReturnsCompletedResult(t *testing.T) { //nolint:cyclop,funlen // end-to-end HTTP assertions stay together
	t.Parallel()

	service, inbox := newTestVoiceService(t)
	server := httptest.NewServer(service.Handler())
	t.Cleanup(server.Close)

	processed := make(chan struct{})

	go func() {
		defer close(processed)

		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			item, err := inbox.DequeueVoice(context.Background())
			if err == nil {
				service.complete(item.MessageID, VoiceResponse{Text: "Done.", Delivery: "voice"})

				return
			}

			if !errors.Is(err, sql.ErrNoRows) {
				return
			}

			time.Sleep(time.Millisecond)
		}
	}()

	payload := VoiceRequest{
		RequestID: testRequestID,
		Text:      "Turn on the lights",
		Context: VoiceContext{
			DeviceID: "voice_pe_kitchen",
			AreaID:   "kitchen",
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}

	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/v1/turn", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	request.Header.Set("Authorization", "Bearer "+testVoiceToken)
	request.Header.Set("Content-Type", "application/json")

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}

	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.StatusCode, responseBody)
	}

	var result VoiceResponse
	if err := json.Unmarshal(responseBody, &result); err != nil {
		t.Fatal(err)
	}

	if result.RequestID != testRequestID || result.Text != "Done." || result.Delivery != "voice" {
		t.Fatalf("result = %+v", result)
	}

	<-processed
}

func TestVoiceIdempotency(t *testing.T) {
	t.Parallel()

	service, inbox := newTestVoiceService(t)
	request := VoiceRequest{RequestID: testRequestID, Text: "hello"}

	first, callErr := service.submit(t.Context(), request)
	if callErr != nil {
		t.Fatal(callErr)
	}

	second, callErr := service.submit(t.Context(), request)
	if callErr != nil {
		t.Fatal(callErr)
	}

	if first != second || first.waiters != 2 {
		t.Fatalf("duplicate calls = %p and %p, waiters = %d", first, second, first.waiters)
	}

	item, err := inbox.DequeueVoice(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if item.MessageID != testRequestID {
		t.Fatalf("queued request = %q", item.MessageID)
	}

	if _, err := inbox.DequeueVoice(t.Context()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second dequeue error = %v, want no rows", err)
	}

	conflict := request

	conflict.Text = "different"
	if _, callErr := service.submit(t.Context(), conflict); callErr == nil || callErr.Status != http.StatusConflict {
		t.Fatalf("conflict error = %+v", callErr)
	}
}

func TestVoiceExpiredRequestDoesNotRun(t *testing.T) {
	t.Parallel()

	service, _ := newTestVoiceService(t)

	call, callErr := service.submit(t.Context(), VoiceRequest{RequestID: testRequestID, Text: "hello"})
	if callErr != nil {
		t.Fatal(callErr)
	}

	call.deadline = time.Now().Add(-time.Second)

	if _, ok := service.begin(testRequestID); ok {
		t.Fatal("expired request began processing")
	}

	if call.err == nil || call.err.Code != "timeout" {
		t.Fatalf("call error = %+v", call.err)
	}
}

func TestVoiceQueueLimitAndCancellation(t *testing.T) {
	t.Parallel()

	service, inbox := newTestVoiceService(t)

	var first *voiceCall

	for i := range voiceMaxPending {
		request := VoiceRequest{
			RequestID: "35cc6ab3-12fe-4bc5-8dda-31f599d5ce7" + string(rune('0'+i)),
			Text:      "queued",
		}

		call, err := service.submit(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}

		if i == 0 {
			first = call
		}
	}

	overflow := VoiceRequest{RequestID: "35cc6ab3-12fe-4bc5-8dda-31f599d5ce99", Text: "too many"}
	if _, err := service.submit(t.Context(), overflow); err == nil || err.Status != http.StatusTooManyRequests {
		t.Fatalf("overflow error = %+v", err)
	}

	firstID := "35cc6ab3-12fe-4bc5-8dda-31f599d5ce70"
	service.releaseWaiter(t.Context(), firstID, first, true, &voiceError{Status: http.StatusRequestTimeout, Code: "cancelled", Message: "cancelled"})

	for {
		item, err := inbox.DequeueVoice(t.Context())
		if errors.Is(err, sql.ErrNoRows) {
			break
		}

		if err != nil {
			t.Fatal(err)
		}

		if item.MessageID == firstID {
			t.Fatal("cancelled request remained in the queue")
		}
	}
}

func TestVoiceWorkerCompletesHTTPCall(t *testing.T) { //nolint:cyclop // worker lifecycle assertions stay together
	t.Parallel()

	script, err := filepath.Abs("testdata/fake-pi")
	if err != nil {
		t.Fatal(err)
	}

	stateDir := t.TempDir()
	db := newTestDBAt(t.Context(), t, stateDir+"/voice.db")
	inbox := newTestInboxWithDB(t.Context(), t, db)
	worker := NewVoiceWorker(inbox, PiConfig{
		BinaryPath: "bash",
		BinaryArgs: []string{script},
		SessionDir: filepath.Join(stateDir, "voice"),
		StateDir:   stateDir,
		WorkingDir: stateDir,
	})
	matrixClient := &mockMatrix{}
	app := NewApp(matrixClient, NewWorker(inbox, PiConfig{}), inbox, db)
	service := NewVoiceService(HTTPConfig{BearerToken: testVoiceToken}, inbox, worker)
	worker.SetApp(app)
	worker.SetMatrix(matrixClient)
	worker.SetVoiceService(service)

	ctx, cancel := context.WithCancel(t.Context())
	done := spawnWorker(ctx, worker)

	t.Cleanup(func() {
		cancel()
		<-done
	})

	call, callErr := service.submit(t.Context(), VoiceRequest{RequestID: testRequestID, Text: "hello"})
	if callErr != nil {
		t.Fatal(callErr)
	}

	select {
	case <-call.done:
		if call.err != nil {
			t.Fatal(call.err)
		}

		if call.result.Text != "ok" || call.result.Delivery != deliveryVoice {
			t.Fatalf("result = %+v", call.result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("voice worker did not complete the request")
	}

	compact, err := worker.Compact(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if compact.TokensBefore != 1 || compact.Summary != "s" {
		t.Fatalf("compact result = %+v", compact)
	}
}

func TestVoiceRestartFailsAndRemovesQueuedTurns(t *testing.T) {
	t.Parallel()

	service, inbox := newTestVoiceService(t)

	call, callErr := service.submit(t.Context(), VoiceRequest{RequestID: testRequestID, Text: "hello"})
	if callErr != nil {
		t.Fatal(callErr)
	}

	if err := service.restart(t.Context()); err != nil {
		t.Fatal(err)
	}

	<-call.done

	if call.err == nil || call.err.Code != "voice_restarted" {
		t.Fatalf("call error = %+v", call.err)
	}

	if _, err := inbox.DequeueVoice(t.Context()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("dequeue after restart = %v, want no rows", err)
	}
}

func TestBuildVoicePromptEscapesContextAndText(t *testing.T) {
	t.Parallel()

	prompt := buildVoicePrompt(VoiceRequest{
		Text: "lights <now>",
		Context: VoiceContext{
			AreaID: "kitchen & dining",
		},
	})

	for _, want := range []string{
		"<area-id>kitchen &amp; dining</area-id>",
		"<voice-message>lights &lt;now&gt;</voice-message>",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt %q missing %q", prompt, want)
		}
	}
}
