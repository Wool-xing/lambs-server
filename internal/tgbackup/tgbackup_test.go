package tgbackup

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func resetSecrets() {
	// Fresh zero value of the package's anonymous secrets struct resets the
	// sync.Once too, so each test re-reads TG_SECRETS_PATH.
	secrets = struct {
		sync.Once
		token  string
		backup string
		gpg    string
	}{}
}

func TestLoadSecretsParsing(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "tg-secrets")
	os.WriteFile(p, []byte(`
# comment line
TG_BOT_TOKEN=123:abc

TG_CHANNEL_BACKUP=-100123
GPG_PASS=secret pass

TG_IGNORED=zzz
`), 0600)
	os.Setenv("TG_SECRETS_PATH", p)
	defer os.Unsetenv("TG_SECRETS_PATH")

	resetSecrets()
	loadSecrets()
	if secrets.token != "123:abc" || secrets.backup != "-100123" || secrets.gpg != "secret pass" {
		t.Errorf("secrets = %q %q %q", secrets.token, secrets.backup, secrets.gpg)
	}
}

func TestLoadSecretsUnsetPath(t *testing.T) {
	os.Unsetenv("TG_SECRETS_PATH")
	resetSecrets()
	loadSecrets()
	if secrets.token != "" || secrets.backup != "" || secrets.gpg != "" {
		t.Errorf("secrets should be empty without TG_SECRETS_PATH: %q %q %q", secrets.token, secrets.backup, secrets.gpg)
	}
}

// TestEncryptGPGNoPassphrase — no passphrase configured: passthrough.
func TestEncryptGPGNoPassphrase(t *testing.T) {
	resetSecrets()
	secrets.gpg = ""
	f := filepath.Join(t.TempDir(), "backup.db")
	os.WriteFile(f, []byte("data"), 0600)
	got, err := EncryptGPG(f)
	if err != nil || got != f {
		t.Errorf("EncryptGPG = %q, %v; want passthrough", got, err)
	}
}

// TestEncryptGPGWithPassphrase — env-adaptive: with gpg on PATH the file
// round-trips through AES256 symmetric encryption; without gpg, the call
// must fail with an explicit error (never a silent passthrough).
func TestEncryptGPGWithPassphrase(t *testing.T) {
	resetSecrets()
	secrets.gpg = "test-passphrase"
	dir := t.TempDir()
	f := filepath.Join(dir, "backup.db")
	os.WriteFile(f, []byte("secret-data"), 0600)
	out, err := EncryptGPG(f)
	if err != nil {
		if !strings.Contains(err.Error(), "gpg encrypt") {
			t.Errorf("EncryptGPG err = %v, want gpg encrypt error", err)
		}
		return // no gpg binary — error path verified
	}
	if out != f+".gpg" {
		t.Fatalf("out = %q, want %q", out, f+".gpg")
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("encrypted file missing: %v", err)
	}
	// Decrypt back and compare — proves the passphrase path really works.
	cmd := exec.Command("gpg", "--batch", "--yes", "--passphrase", "test-passphrase",
		"--output", filepath.Join(dir, "dec.db"), "--decrypt", out)
	if o, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("decrypt: %s — %v", o, err)
	}
	dec, _ := os.ReadFile(filepath.Join(dir, "dec.db"))
	if string(dec) != "secret-data" {
		t.Errorf("round-trip mismatch: %q", dec)
	}
}

func TestUploadNotConfigured(t *testing.T) {
	resetSecrets()
	secrets.token = ""
	f := filepath.Join(t.TempDir(), "x.db")
	os.WriteFile(f, []byte("x"), 0600)
	_, err := Upload(f, "cap")
	if err == nil || !strings.Contains(err.Error(), "TG not configured") {
		t.Errorf("Upload err = %v, want TG not configured", err)
	}
}

func TestUploadFileNotFound(t *testing.T) {
	resetSecrets()
	secrets.token = "tok"
	secrets.backup = "-100"
	_, err := Upload(filepath.Join(t.TempDir(), "missing.db"), "cap")
	if err == nil || !strings.Contains(err.Error(), "file not found") {
		t.Errorf("Upload err = %v, want file not found", err)
	}
}
