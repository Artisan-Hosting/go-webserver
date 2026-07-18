package main

import (
	"fmt"
	"log"
	"net/http"

	"github.com/fsnotify/fsnotify"
)

// clients holds one channel per connected /reload Server-Sent-Events
// client. Sending on a channel notifies that client's handler to push a
// "reload" event to the browser.
var clients = make(map[chan bool]bool)

// reloadHandler implements the /reload Server-Sent-Events endpoint used for
// live-reload during local development. Each connected browser registers a
// channel in clients and receives a "reload" event whenever watchFiles
// detects a static file change, until the request context is canceled.
func reloadHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan bool)
	clients[ch] = true
	defer delete(clients, ch)

	for {
		select {
		case <-ch:
			fmt.Fprintf(w, "data: reload\n\n")
			w.(http.Flusher).Flush()
		case <-r.Context().Done():
			return
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
		case event := <-watcher.Events:
			log.Println("watch: file change detected:", event.Name)
			if onChange != nil {
				onChange()
				log.Println("watch: site assets refreshed")
			}
			log.Printf("watch: notifying %d connected reload client(s)", len(clients))
			for ch := range clients {
				ch <- true
			}
		case err := <-watcher.Errors:
			log.Println("Watcher error:", err)
		}
	}
}
