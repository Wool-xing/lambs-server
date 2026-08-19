package handlers

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestDeleteBackupHonest — deleting a file returns deleted and removes it;
// a failed remove (non-empty directory) must NOT report success (QA round 3
// calibration: os.Remove error was ignored — false success).
func TestDeleteBackupHonest(t *testing.T) {
	baseDir := "/home/ubuntu/lambs-backups"
	os.MkdirAll(baseDir, 0755)
	defer os.RemoveAll(baseDir)

	saReq := func() *http.Request {
		r := httptest.NewRequest("DELETE", "/api/backups/proj-a/download/x", nil)
		r.Header.Set("X-User-ID", "admin")
		r.Header.Set("X-Role", "super_admin")
		r.SetPathValue("id", "proj-a")
		return r
	}

	// Happy path: real file removed, honest deleted.
	f := filepath.Join(baseDir, "proj-a_test.db")
	os.WriteFile(f, []byte("data"), 0600)
	w := httptest.NewRecorder()
	DeleteBackup(w, saReq(), "proj-a", "proj-a_test.db")
	if w.Code != 200 {
		t.Fatalf("delete = %d (body %s)", w.Code, w.Body.String())
	}
	if _, err := os.Stat(f); !os.IsNotExist(err) {
		t.Error("file still exists after delete")
	}

	// Failure path: a non-empty DIRECTORY passes safeBackupPath (name has the
	// project prefix) but os.Remove fails — the API must not say deleted.
	dir := filepath.Join(baseDir, "proj-a_dir.db")
	os.MkdirAll(filepath.Join(dir, "inner"), 0755)
	defer os.RemoveAll(dir)
	w2 := httptest.NewRecorder()
	DeleteBackup(w2, saReq(), "proj-a", "proj-a_dir.db")
	if w2.Code == 200 {
		t.Fatalf("delete of non-empty dir = 200 (body %s), want error", w2.Body.String())
	}

	// Traversal rejected before any filesystem access.
	w3 := httptest.NewRecorder()
	DeleteBackup(w3, saReq(), "proj-a", "../etc/passwd")
	if w3.Code != 404 {
		t.Errorf("traversal delete = %d, want 404", w3.Code)
	}
}
