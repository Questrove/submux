package server

import (
	"net/http"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

type runtimeUpdateHub struct {
	mu          sync.Mutex
	subscribers map[int64]map[chan string]struct{}
}

func newRuntimeUpdateHub() *runtimeUpdateHub {
	return &runtimeUpdateHub{subscribers: make(map[int64]map[chan string]struct{})}
}

func (h *runtimeUpdateHub) subscribe(instanceID int64) (<-chan string, func()) {
	updates := make(chan string, 8)
	h.mu.Lock()
	if h.subscribers[instanceID] == nil {
		h.subscribers[instanceID] = make(map[chan string]struct{})
	}
	h.subscribers[instanceID][updates] = struct{}{}
	h.mu.Unlock()
	return updates, func() {
		h.mu.Lock()
		delete(h.subscribers[instanceID], updates)
		if len(h.subscribers[instanceID]) == 0 {
			delete(h.subscribers, instanceID)
		}
		h.mu.Unlock()
	}
}

func (h *runtimeUpdateHub) notify(instanceID int64, reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for subscriber := range h.subscribers[instanceID] {
		select {
		case subscriber <- reason:
		default:
		}
	}
}

func (s *Server) handleAgentUpdates(w http.ResponseWriter, r *http.Request) {
	instance := deviceInstance(r)
	server := websocket.Server{
		Handshake: sameOriginWebSocket,
		Handler: websocket.Handler(func(connection *websocket.Conn) {
			updates, unsubscribe := s.updates.subscribe(instance.ID)
			defer unsubscribe()
			defer connection.Close()
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				var message map[string]any
				select {
				case reason := <-updates:
					message = map[string]any{"type": "check_state", "reason": reason}
				case <-ticker.C:
					message = map[string]any{"type": "keepalive"}
				case <-r.Context().Done():
					return
				}
				if err := websocket.JSON.Send(connection, message); err != nil {
					return
				}
			}
		}),
	}
	server.ServeHTTP(w, r)
}
