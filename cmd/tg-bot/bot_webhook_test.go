package main

import (
	"net/http/httptest"
	"testing"
	"time"
)

// TestWebhookHandler — the secret-token gate is the only thing between the
// public nginx port and the bot waking up: bad secret → 403, no wake;
// good secret → 200 and exactly one wake signal.
func TestWebhookHandler(t *testing.T) {
	oldSecret := webhookSecret
	webhookSecret = "s3cret"
	defer func() { webhookSecret = oldSecret }()

	// Drain any stale wake signals.
	for {
		select {
		case <-wakeCh:
		default:
			goto drained
		}
	}
drained:

	h := webhookHandler()

	// Bad secret: 403, body ok:false, no wake signal.
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Telegram-Bot-Api-Secret-Token", "wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Errorf("bad secret code = %d, want 403", rec.Code)
	}
	if rec.Body.String() != `{"ok":false}` {
		t.Errorf("bad secret body = %q", rec.Body.String())
	}
	select {
	case <-wakeCh:
		t.Error("bad secret woke the bot")
	case <-time.After(50 * time.Millisecond):
	}

	// Good secret: 200, ok:true, wake signal delivered.
	req = httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Telegram-Bot-Api-Secret-Token", "s3cret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("good secret code = %d, want 200", rec.Code)
	}
	select {
	case <-wakeCh:
	case <-time.After(time.Second):
		t.Error("good secret did not wake the bot")
	}
}

// TestProcessUpdates — update_id advances for every update (message or not);
// hasMsg only for updates carrying a text message.
func TestProcessUpdates(t *testing.T) {
	// Empty adminChats → handleCommand refuses everything without side
	// effects; dispatch itself is covered by bot_commands_test.go.
	adminChats = map[int64]bool{}

	updates := []interface{}{
		map[string]interface{}{"update_id": float64(101), "message": map[string]interface{}{
			"chat": map[string]interface{}{"id": float64(42)}, "text": "/status"}},
		// Non-message update: id still advances, no hasMsg.
		map[string]interface{}{"update_id": float64(102), "edited_message": map[string]interface{}{"text": "x"}},
		// Garbage entries skipped without panicking.
		"junk",
		map[string]interface{}{"update_id": float64(103)},
	}

	hasMsg, latest := processUpdates(updates, 100)
	if !hasMsg {
		t.Error("hasMsg = false, want true (one text message present)")
	}
	if latest != 103 {
		t.Errorf("latest = %d, want 103", latest)
	}

	// Empty batch: nothing advances.
	hasMsg, latest = processUpdates([]interface{}{}, 200)
	if hasMsg || latest != 200 {
		t.Errorf("empty batch = (%v, %d), want (false, 200)", hasMsg, latest)
	}
}
