//go:build integration

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

var listeningPortRE = regexp.MustCompile(`Starting server on .*:([0-9]+)$`)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) WriteString(s string) {
	_, _ = b.Write([]byte(s))
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func TestServerProcess(t *testing.T) {
	tempDir := t.TempDir()
	binPath := filepath.Join(tempDir, "artisan-webserver")
	build := exec.Command("go", "build", "-o", binPath, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build server: %v\n%s", err, output)
	}

	flagStatic := filepath.Join(tempDir, "flag-static")
	envStatic := filepath.Join(tempDir, "env-static")
	for _, dir := range []string{flagStatic, envStatic} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(flagStatic, "index.html"), []byte("served-from-flag"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(envStatic, "index.html"), []byte("served-from-env"), 0o644); err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(tempDir, ".env")
	envBody := fmt.Sprintf("WEBSITE_FILES=%s\nCAPTCHA_API_ENDPOINT=https://cap.example/site/\nCAPTCHA_SECRET_KEY=process-secret\n", envStatic)
	if err := os.WriteFile(envPath, []byte(envBody), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(binPath,
		"-port", "0",
		"-env-path", envPath,
		"-website-files", flagStatic,
		"-preview-cache-dir", filepath.Join(tempDir, "previews"),
		"-sim-error", "503",
	)
	cmd.Env = withoutEnvironmentKeys(os.Environ(), "WEBSITE_FILES", "CAPTCHA_API_ENDPOINT", "CAPTCHA_SECRET_KEY")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	var logs lockedBuffer
	cmd.Stdout = &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	t.Cleanup(func() {
		if !stopped && cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})

	portCh := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			logs.WriteString(line + "\n")
			if match := listeningPortRE.FindStringSubmatch(line); len(match) == 2 {
				select {
				case portCh <- match[1]:
				default:
				}
			}
		}
	}()

	var port string
	select {
	case port = <-portCh:
	case <-time.After(10 * time.Second):
		t.Fatalf("server did not report readiness\n%s", logs.String())
	}
	baseURL := "http://127.0.0.1:" + port
	client := &http.Client{Timeout: 2 * time.Second}

	resp, err := client.Get(baseURL + "/")
	if err != nil {
		t.Fatalf("GET /: %v\n%s", err, logs.String())
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "served-from-flag") {
		t.Fatalf("static response=%d %q", resp.StatusCode, body)
	}

	resp, err = client.Get(baseURL + "/api/captcha-config")
	if err != nil {
		t.Fatal(err)
	}
	var captcha map[string]any
	err = json.NewDecoder(resp.Body).Decode(&captcha)
	resp.Body.Close()
	if err != nil || captcha["enabled"] != true || captcha["endpoint"] != "https://cap.example/site/" {
		t.Fatalf("captcha config=%v err=%v", captcha, err)
	}
	if strings.Contains(fmt.Sprint(captcha), "process-secret") {
		t.Fatal("captcha config leaked secret")
	}

	resp, err = client.Post(baseURL+"/api/contact", "application/json", strings.NewReader(`{"name":"Jane","email":"jane@example.com"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("simulated contact status=%d", resp.StatusCode)
	}

	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		stopped = true
		if err != nil {
			t.Fatalf("server shutdown: %v\n%s", err, logs.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("server did not shut down cleanly\n%s", logs.String())
	}
}

func withoutEnvironmentKeys(environment []string, keys ...string) []string {
	blocked := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		blocked[key] = struct{}{}
	}
	filtered := make([]string, 0, len(environment))
	for _, entry := range environment {
		key, _, _ := strings.Cut(entry, "=")
		if _, skip := blocked[key]; !skip {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}
