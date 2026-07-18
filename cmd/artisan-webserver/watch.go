package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"

	"github.com/fsnotify/fsnotify"
)

// clients holds one channel per connected /reload Server-Sent-Events
// client. Sending on a channel notifies that client's handler to push a
// "reload" event to the browser.
type reloadHub struct {
	mu      sync.RWMutex
	clients map[chan struct{}]struct{}
}

func newReloadHub() *reloadHub {
	return &reloadHub{clients: make(map[chan struct{}]struct{})}
}

func (h *reloadHub) subscribe() chan struct{} {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *reloadHub) unsubscribe(ch chan struct{}) {
	h.mu.Lock()
	delete(h.clients, ch)
	h.mu.Unlock()
}

func (h *reloadHub) broadcast() {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for ch := range h.clients {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (h *reloadHub) count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

var defaultReloadHub = newReloadHub()

// reloadHandler implements the /reload Server-Sent-Events endpoint used for
// live-reload during local development. Each connected browser registers a
// channel in clients and receives a "reload" event whenever watchFiles
// detects a static file change, until the request context is canceled.
func reloadHandler(w http.ResponseWriter, r *http.Request) {
	reloadHandlerWithHub(defaultReloadHub)(w, r)
}

func reloadHandlerWithHub(hub *reloadHub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		ch := hub.subscribe()
		defer hub.unsubscribe(ch)

		for {
			select {
			case <-ch:
				fmt.Fprintf(w, "data: reload\n\n")
				flusher.Flush()
			case <-r.Context().Done():
				return
			}
		}
	}
}

// watchFiles watches path (recursively is not supported by fsnotify at the
// top level, so only direct changes to path are observed) for filesystem
// events. On each event it invokes onChange (if non-nil) to let callers
// refresh derived state such as the site build hash, then notifies every
// connected /reload client so browsers can refresh. If the watcher cannot
// be created, live-reload is silently disabled.
func watchFiles(path string, onChange func()) {
	watchFilesContext(context.Background(), path, onChange, defaultReloadHub)
}

func watchFilesContext(ctx context.Context, path string, onChange func(), hub *reloadHub) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("live-reload disabled: failed to create file watcher: %v", err)
		return
	}
	defer watcher.Close()

	if err := watcher.Add(path); err != nil {
		log.Printf("live-reload disabled: failed to watch %s: %v", path, err)
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			log.Println("watch: file change detected:", event.Name)
			if onChange != nil {
				onChange()
				log.Println("watch: site assets refreshed")
			}
			log.Printf("watch: notifying %d connected reload client(s)", hub.count())
			hub.broadcast()
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Println("Watcher error:", err)
		}
	}
}
