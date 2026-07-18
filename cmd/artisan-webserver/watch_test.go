package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type streamRecorder struct {
	mu      sync.Mutex
	header  http.Header
	body    bytes.Buffer
	flushed chan struct{}
}

type noFlushRecorder struct {
	header http.Header
	status int
}

func (r *noFlushRecorder) Header() http.Header {
	if r.header == nil {
		r.header = make(http.Header)
	}
	return r.header
}
func (r *noFlushRecorder) WriteHeader(status int) { r.status = status }
func (r *noFlushRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return len(p), nil
}

func newStreamRecorder() *streamRecorder {
	return &streamRecorder{header: make(http.Header), flushed: make(chan struct{}, 1)}
}

func (r *streamRecorder) Header() http.Header { return r.header }
func (r *streamRecorder) WriteHeader(int)     {}
func (r *streamRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.Write(p)
}
func (r *streamRecorder) Flush() {
	select {
	case r.flushed <- struct{}{}:
	default:
	}
}
func (r *streamRecorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.String()
}

func TestReloadHubAndHandler(t *testing.T) {
	hub := newReloadHub()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/reload", nil).WithContext(ctx)
	recorder := newStreamRecorder()
	done := make(chan struct{})
	go func() {
		reloadHandlerWithHub(hub)(recorder, req)
		close(done)
	}()
	deadline := time.Now().Add(time.Second)
	for hub.count() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if hub.count() != 1 {
		t.Fatal("SSE client did not subscribe")
	}
	hub.broadcast()
	select {
	case <-recorder.flushed:
	case <-time.After(time.Second):
		t.Fatal("reload event was not flushed")
	}
	if recorder.String() != "data: reload\n\n" {
		t.Fatalf("event=%q", recorder.String())
	}
	if recorder.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatal("missing SSE content type")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler did not stop after cancellation")
	}
	if hub.count() != 0 {
		t.Fatal("SSE client was not removed")
	}

	plain := &noFlushRecorder{}
	reloadHandlerWithHub(hub)(plain, httptest.NewRequest(http.MethodGet, "/reload", nil))
	if plain.status != http.StatusInternalServerError {
		t.Fatalf("non-streaming status=%d", plain.status)
	}
}

func TestWatchFilesContextObservesChangeAndStops(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	hub := newReloadHub()
	changed := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		watchFilesContext(ctx, dir, func() {
			select {
			case changed <- struct{}{}:
			default:
			}
		}, hub)
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	observed := false
	for attempt := 0; time.Now().Before(deadline); attempt++ {
		if err := os.WriteFile(filepath.Join(dir, "change.txt"), []byte{byte(attempt)}, 0o644); err != nil {
			t.Fatal(err)
		}
		select {
		case <-changed:
			observed = true
		case <-time.After(20 * time.Millisecond):
		}
		if observed {
			break
		}
	}
	if !observed {
		t.Fatal("watcher did not observe change")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watcher did not stop")
	}
}
