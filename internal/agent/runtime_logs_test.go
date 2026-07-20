package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestRuntimeLogBufferRelaysSanitizedLocalTail(t *testing.T) {
	buffer := NewRuntimeLogBuffer()
	buffer.Printf("download https://example.com/file?token=secret Bearer credential")
	stop := errors.New("stop")
	var frame string
	err := buffer.Stream(context.Background(), func(value json.RawMessage) error {
		frame = string(value)
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatalf("stream error = %v", err)
	}
	if strings.Contains(frame, "example.com") || strings.Contains(frame, "credential") || !strings.Contains(frame, "agent_logs") || !strings.Contains(frame, "[redacted") {
		t.Fatalf("unsafe Agent log frame: %s", frame)
	}
}

func TestRuntimeLogBufferUsesStableStreamAndMonotonicSequence(t *testing.T) {
	buffer := NewRuntimeLogBuffer()
	buffer.Printf("first")
	buffer.Printf("second")

	first := collectRuntimeLogSnapshot(t, buffer, 2)
	second := collectRuntimeLogSnapshot(t, buffer, 2)
	if first[0].StreamID == "" || first[0].StreamID != first[1].StreamID {
		t.Fatalf("stream IDs = %q, %q", first[0].StreamID, first[1].StreamID)
	}
	if first[0].Sequence != 1 || first[1].Sequence != 2 {
		t.Fatalf("sequences = %d, %d", first[0].Sequence, first[1].Sequence)
	}
	for i := range first {
		if second[i].StreamID != first[i].StreamID || second[i].Sequence != first[i].Sequence {
			t.Fatalf("replayed entry %d changed: first=%+v second=%+v", i, first[i], second[i])
		}
	}
}

func collectRuntimeLogSnapshot(t *testing.T, buffer *RuntimeLogBuffer, count int) []runtimeLogEntry {
	t.Helper()
	stop := errors.New("stop")
	entries := make([]runtimeLogEntry, 0, count)
	err := buffer.Stream(context.Background(), func(value json.RawMessage) error {
		var frame struct {
			Kind string          `json:"kind"`
			Data runtimeLogEntry `json:"data"`
		}
		if err := json.Unmarshal(value, &frame); err != nil {
			t.Fatalf("decode runtime log frame: %v", err)
		}
		if frame.Kind != "agent_logs" {
			t.Fatalf("frame kind = %q", frame.Kind)
		}
		entries = append(entries, frame.Data)
		if len(entries) == count {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) {
		t.Fatalf("stream error = %v", err)
	}
	return entries
}
