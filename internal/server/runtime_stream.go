package server

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/net/websocket"
)

const (
	maxRuntimeStreamFrame = 256 << 10
	runtimeStreamLifetime = 30 * time.Minute
)

var runtimeStreamKinds = map[string]bool{"proxies": true, "configs": true, "rules": true, "connections": true, "traffic": true, "memory": true, "logs": true, "docker_preview": true, "docker_desktop_preview": true}

type runtimeStreamSession struct {
	instanceID int64
	kind       string
	frames     chan string
	done       chan struct{}
	closeOnce  sync.Once
	expires    time.Time
}

func (s *runtimeStreamSession) close() { s.closeOnce.Do(func() { close(s.done) }) }

type runtimeStreamHub struct {
	mu       sync.Mutex
	sessions map[string]*runtimeStreamSession
}

func newRuntimeStreamHub() *runtimeStreamHub {
	return &runtimeStreamHub{sessions: make(map[string]*runtimeStreamSession)}
}

func (h *runtimeStreamHub) create(instanceID int64, kind string, now time.Time) (string, *runtimeStreamSession) {
	id := randomHex(24)
	session := &runtimeStreamSession{instanceID: instanceID, kind: kind, frames: make(chan string, 32), done: make(chan struct{}), expires: now.Add(runtimeStreamLifetime)}
	h.mu.Lock()
	h.pruneLocked(now)
	h.sessions[id] = session
	h.mu.Unlock()
	return id, session
}

func (h *runtimeStreamHub) get(id string, instanceID int64, now time.Time) (*runtimeStreamSession, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pruneLocked(now)
	session := h.sessions[id]
	if session == nil || session.instanceID != instanceID {
		return nil, errors.New("runtime stream session is unavailable")
	}
	return session, nil
}

func (h *runtimeStreamHub) remove(id string, session *runtimeStreamSession) {
	h.mu.Lock()
	if h.sessions[id] == session {
		delete(h.sessions, id)
	}
	h.mu.Unlock()
	session.close()
}

func (h *runtimeStreamHub) pruneLocked(now time.Time) {
	for id, session := range h.sessions {
		if !now.Before(session.expires) {
			delete(h.sessions, id)
			session.close()
		}
	}
}

func (s *Server) handleBrowserRuntimeStream(w http.ResponseWriter, r *http.Request) {
	instanceID, err := idParam(r)
	kind := chi.URLParam(r, "kind")
	if err != nil || !runtimeStreamKinds[kind] {
		http.Error(w, "invalid runtime stream target", http.StatusBadRequest)
		return
	}
	instance, err := s.store.GetRuntimeInstance(instanceID)
	if err != nil || instance.RevokedAt != "" {
		http.Error(w, "runtime instance does not exist or is revoked", http.StatusNotFound)
		return
	}
	required := "mihomo.runtime.observe"
	if kind == "docker_preview" {
		required = "integration.docker_daemon"
	} else if kind == "docker_desktop_preview" {
		required = "integration.docker_desktop"
	}
	capable := false
	for _, capability := range instance.Capabilities {
		if capability == required {
			capable = true
			break
		}
	}
	if !capable {
		http.Error(w, "runtime instance does not support this stream", http.StatusConflict)
		return
	}
	websocket.Server{
		Handshake: sameOriginWebSocket,
		Handler: websocket.Handler(func(connection *websocket.Conn) {
			defer connection.Close()
			sessionID, session := s.streams.create(instanceID, kind, time.Now().UTC())
			defer s.streams.remove(sessionID, session)
			s.updates.notify(instanceID, "runtime_stream|"+sessionID+"|"+kind)
			disconnected := make(chan struct{})
			go func() {
				var ignored string
				_ = websocket.Message.Receive(connection, &ignored)
				close(disconnected)
			}()
			timer := time.NewTimer(runtimeStreamLifetime)
			defer timer.Stop()
			for {
				select {
				case frame := <-session.frames:
					if err := websocket.Message.Send(connection, frame); err != nil {
						return
					}
				case <-session.done:
					for {
						select {
						case frame := <-session.frames:
							if err := websocket.Message.Send(connection, frame); err != nil {
								return
							}
						default:
							return
						}
					}
				case <-timer.C:
					return
				case <-r.Context().Done():
					return
				case <-disconnected:
					return
				}
			}
		}),
	}.ServeHTTP(w, r)
}

func (s *Server) handleAgentRuntimeStream(w http.ResponseWriter, r *http.Request) {
	instance := deviceInstance(r)
	sessionID := chi.URLParam(r, "session")
	session, err := s.streams.get(sessionID, instance.ID, time.Now().UTC())
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	websocket.Server{
		Handshake: sameOriginWebSocket,
		Handler: websocket.Handler(func(connection *websocket.Conn) {
			defer connection.Close()
			defer session.close()
			go func() {
				select {
				case <-session.done:
					_ = connection.Close()
				case <-r.Context().Done():
					_ = connection.Close()
				}
			}()
			windowStart, windowBytes := time.Now(), 0
			for {
				var frame string
				if err := websocket.Message.Receive(connection, &frame); err != nil {
					return
				}
				if len(frame) == 0 || len(frame) > maxRuntimeStreamFrame {
					return
				}
				if time.Since(windowStart) >= time.Second {
					windowStart, windowBytes = time.Now(), 0
				}
				windowBytes += len(frame)
				if windowBytes > 1<<20 {
					return
				}
				select {
				case session.frames <- frame:
				case <-session.done:
					return
				case <-r.Context().Done():
					return
				}
			}
		}),
	}.ServeHTTP(w, r)
}

func sameOriginWebSocket(_ *websocket.Config, r *http.Request) error {
	origin := r.Header.Get("Origin")
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || !strings.EqualFold(parsed.Host, r.Host) {
		return errors.New("WebSocket origin does not match the control plane")
	}
	return nil
}
