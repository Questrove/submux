//go:build windows

package hostops

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestGuardUserChildKillsProcessWhenClosed(t *testing.T) {
	if os.Getenv("SUBMUX_JOB_OBJECT_HELPER") == "1" {
		time.Sleep(30 * time.Second)
		return
	}
	command := exec.Command(os.Args[0], "-test.run=TestGuardUserChildKillsProcessWhenClosed")
	command.Env = append(os.Environ(), "SUBMUX_JOB_OBJECT_HELPER=1")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer command.Process.Kill()
	guard, err := guardUserChild(command)
	if err != nil {
		t.Fatal(err)
	}
	if err := guard(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("closing the Job Object did not terminate the child process")
	}
}
