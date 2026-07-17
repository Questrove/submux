//go:build windows

package agentlocal

import "testing"

func TestWindowsAgentPipePathIsFixed(t *testing.T) {
	if _, err := Listen(`\\.\pipe\attacker-selected`); err == nil {
		t.Fatal("arbitrary Windows named pipe path was accepted")
	}
	if _, err := NewClient(`\\.\pipe\attacker-selected`); err == nil {
		t.Fatal("arbitrary Windows named pipe client path was accepted")
	}
}
