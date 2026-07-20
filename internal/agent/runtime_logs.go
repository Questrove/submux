package agent

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"submux/internal/mihomo"
)

type runtimeLogEntry struct {
	StreamID string `json:"stream_id"`
	Sequence uint64 `json:"sequence"`
	Time     string `json:"time"`
	Level    string `json:"level"`
	Message  string `json:"message"`
}

// RuntimeLogBuffer keeps a small local-only tail and relays it only while an
// authenticated browser explicitly opens the Agent log view.
type RuntimeLogBuffer struct {
	mu           sync.Mutex
	streamID     string
	nextSequence uint64
	entries      []runtimeLogEntry
	subscribers  map[chan runtimeLogEntry]struct{}
}

func NewRuntimeLogBuffer() *RuntimeLogBuffer {
	return &RuntimeLogBuffer{
		streamID:    newRuntimeLogStreamID(),
		subscribers: make(map[chan runtimeLogEntry]struct{}),
	}
}

func (b *RuntimeLogBuffer) Printf(format string, args ...any) {
	message := strings.TrimSpace(mihomo.SanitizeLogText(fmt.Sprintf(format, args...)))
	if message == "" {
		return
	}
	b.mu.Lock()
	b.nextSequence++
	entry := runtimeLogEntry{
		StreamID: b.streamID,
		Sequence: b.nextSequence,
		Time:     time.Now().UTC().Format(time.RFC3339),
		Level:    "info",
		Message:  message,
	}
	b.entries = append(b.entries, entry)
	if len(b.entries) > 300 {
		b.entries = append([]runtimeLogEntry(nil), b.entries[len(b.entries)-300:]...)
	}
	for subscriber := range b.subscribers {
		select {
		case subscriber <- entry:
		default:
		}
	}
	b.mu.Unlock()
}

func newRuntimeLogStreamID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err == nil {
		return fmt.Sprintf("%x", value[:])
	}
	return fmt.Sprintf("fallback-%d", time.Now().UTC().UnixNano())
}

func (b *RuntimeLogBuffer) Stream(ctx context.Context, send func(json.RawMessage) error) error {
	updates := make(chan runtimeLogEntry, 32)
	b.mu.Lock()
	snapshot := append([]runtimeLogEntry(nil), b.entries...)
	b.subscribers[updates] = struct{}{}
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.subscribers, updates)
		b.mu.Unlock()
	}()
	for _, entry := range snapshot {
		if err := send(runtimeLogFrame(entry)); err != nil {
			return err
		}
	}
	for {
		select {
		case entry := <-updates:
			if err := send(runtimeLogFrame(entry)); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func runtimeLogFrame(entry runtimeLogEntry) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"kind": "agent_logs", "data": entry})
	return raw
}
