package tgbackup

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeTGSrv records the sendDocument request for contract assertions.
type fakeTGSrv struct {
	ts       *httptest.Server
	chatID   string
	caption  string
	filename string
	content  string
	replyOK  bool
	replyErr string
}

func newFakeTG(t *testing.T) *fakeTGSrv {
	s := &fakeTGSrv{replyOK: true}
	s.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/bot") || !strings.HasSuffix(r.URL.Path, "/sendDocument") {
			http.Error(w, "bad path", 404)
			return
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		s.chatID = r.FormValue("chat_id")
		s.caption = r.FormValue("caption")
		files := r.MultipartForm.File["document"]
		if len(files) == 0 {
			http.Error(w, "no document", 400)
			return
		}
		f, err := files[0].Open()
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		defer f.Close()
		raw, _ := io.ReadAll(f)
		s.filename = files[0].Filename
		s.content = string(raw)
		if !s.replyOK {
			json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "description": s.replyErr})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ok": true,
			"result": map[string]interface{}{
				"document": map[string]interface{}{"file_id": "F1", "file_name": s.filename, "file_size": len(raw)},
			},
		})
	}))
	t.Cleanup(s.ts.Close)
	return s
}

func writeTGSecrets(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "tg-secrets")
	os.WriteFile(p, []byte("TG_BOT_TOKEN=123:abc\nTG_CHANNEL_BACKUP=-100123\n"), 0600)
	t.Setenv("TG_SECRETS_PATH", p)
}

// TestUploadContractReplay — calibration candidate 9: the TG upload protocol
// stream (multipart shape, chat_id/caption fields, response mapping) runs
// against a mock server instead of zero coverage.
func TestUploadContractReplay(t *testing.T) {
	s := newFakeTG(t)
	t.Setenv("TG_API_BASE", s.ts.URL)
	writeTGSecrets(t)
	resetSecrets()

	dir := t.TempDir()
	f := filepath.Join(dir, "proj_backup.db")
	os.WriteFile(f, []byte("backup-data"), 0600)

	got, err := Upload(f, "Backup: proj @ 2026-08-21 10:00")
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if got["file_id"] != "F1" || got["file_name"] != "proj_backup.db" || got["channel_id"] != "-100123" || got["encrypted"] != false {
		t.Errorf("result = %v", got)
	}
	if s.chatID != "-100123" || s.caption != "Backup: proj @ 2026-08-21 10:00" || s.content != "backup-data" {
		t.Errorf("server saw chat_id=%q caption=%q content=%q", s.chatID, s.caption, s.content)
	}
}

// TestUploadContractErrorBranch — ok:false maps to an error, never a silent
// success.
func TestUploadContractErrorBranch(t *testing.T) {
	s := newFakeTG(t)
	s.replyOK = false
	s.replyErr = "chat not found"
	t.Setenv("TG_API_BASE", s.ts.URL)
	writeTGSecrets(t)
	resetSecrets()

	dir := t.TempDir()
	f := filepath.Join(dir, "x.db")
	os.WriteFile(f, []byte("d"), 0600)

	_, err := Upload(f, "c")
	if err == nil || !strings.Contains(err.Error(), "chat not found") {
		t.Errorf("err = %v, want TG API error", err)
	}
}

// TestUploadContractHTTPFailure — unreachable endpoint: error propagates, no
// panic.
func TestUploadContractHTTPFailure(t *testing.T) {
	writeTGSecrets(t)
	resetSecrets()
	t.Setenv("TG_API_BASE", "http://127.0.0.1:1") // nothing listens

	dir := t.TempDir()
	f := filepath.Join(dir, "x.db")
	os.WriteFile(f, []byte("d"), 0600)

	_, err := Upload(f, "c")
	if err == nil || !strings.Contains(err.Error(), "TG API") {
		t.Errorf("err = %v, want TG API error", err)
	}
}
