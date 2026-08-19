package tgbackup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var secrets struct {
	sync.Once
	token  string
	backup string
	gpg    string
}

var httpClient = &http.Client{Timeout: 60 * time.Second}

func loadSecrets() {
	// 凭据路径经 TG_SECRETS_PATH 配置；未配置 = TG 备份通道停用（开源默认 R24）
	p := os.Getenv("TG_SECRETS_PATH")
	if p == "" {
		return
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		kv := strings.SplitN(line, "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "TG_BOT_TOKEN":
			secrets.token = kv[1]
		case "TG_CHANNEL_BACKUP":
			secrets.backup = kv[1]
		case "GPG_PASS":
			secrets.gpg = kv[1]
		}
	}
}

// EncryptGPG encrypts a file with symmetric GPG using the configured passphrase.
func EncryptGPG(filePath string) (string, error) {
	secrets.Do(loadSecrets)
	if secrets.gpg == "" {
		return filePath, nil // no encryption configured
	}
	outPath := filePath + ".gpg"
	// Passphrase via fd 0 — argv would expose it in /proc/<pid>/cmdline
	// (R12 security).
	cmd := exec.Command("gpg", "--batch", "--yes", "--passphrase-fd", "0",
		"--symmetric", "--cipher-algo", "AES256", "--output", outPath, filePath)
	cmd.Stdin = strings.NewReader(secrets.gpg + "\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("gpg encrypt: %s — %w", string(out), err)
	}
	return outPath, nil
}

// Upload sends a file to the Telegram backup channel.
// Encrypts with GPG first if a passphrase is configured.
func Upload(filePath, caption string) (map[string]interface{}, error) {
	secrets.Do(loadSecrets)
	if secrets.token == "" || secrets.backup == "" {
		return nil, fmt.Errorf("TG not configured")
	}

	uploadPath := filePath
	var cleanup string

	// GPG encrypt if available
	if secrets.gpg != "" {
		encPath, err := EncryptGPG(filePath)
		if err != nil {
			return nil, err
		}
		uploadPath = encPath
		cleanup = encPath
	}

	if _, err := os.Stat(uploadPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("file not found: %s", uploadPath)
	}

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	f, err := os.Open(uploadPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	part, err := w.CreateFormFile("document", filepath.Base(uploadPath))
	if err != nil {
		return nil, fmt.Errorf("multipart create: %w", err)
	}
	if _, err := io.Copy(part, f); err != nil {
		return nil, fmt.Errorf("multipart copy: %w", err)
	}
	w.WriteField("chat_id", secrets.backup)
	if caption != "" {
		w.WriteField("caption", caption)
	}
	w.Close()

	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendDocument", secrets.token)
	resp, err := httpClient.Post(url, w.FormDataContentType(), &buf)
	if err != nil {
		if cleanup != "" {
			os.Remove(cleanup)
		}
		return nil, fmt.Errorf("TG API: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		OK     bool `json:"ok"`
		Result struct {
			Document struct {
				FileID   string `json:"file_id"`
				FileName string `json:"file_name"`
				FileSize int64  `json:"file_size"`
			} `json:"document"`
		} `json:"result"`
		Description string `json:"description"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	if cleanup != "" {
		os.Remove(cleanup)
	}
	if !result.OK {
		return nil, fmt.Errorf("TG API error: %s", result.Description)
	}
	return map[string]interface{}{
		"file_id":    result.Result.Document.FileID,
		"file_name":  result.Result.Document.FileName,
		"file_size":  result.Result.Document.FileSize,
		"channel_id": secrets.backup,
		"encrypted":  secrets.gpg != "",
	}, nil
}
