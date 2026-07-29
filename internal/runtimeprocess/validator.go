package runtimeprocess

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"submux/internal/runtimecore"
)

type ConfigValidator struct {
	BinaryPath   string
	DataDir      string
	ExactVersion string
	OutputLimit  int
}

func (v ConfigValidator) ValidateConfig(ctx context.Context, configPath string) error {
	if err := validateManagedExecutable(v.BinaryPath); err != nil {
		return err
	}
	if err := validateManagedConfig(configPath); err != nil {
		return err
	}
	dataDir, err := prepareDataDir(v.DataDir)
	if err != nil {
		return err
	}
	if v.ExactVersion == "" {
		return errors.New("exact Mihomo version is required for static configuration validation")
	}
	if err := (runtimecore.CommandVerifier{}).VerifyBinary(ctx, v.BinaryPath, v.ExactVersion); err != nil {
		return err
	}
	command := exec.CommandContext(ctx, v.BinaryPath, "-t", "-d", dataDir, "-f", configPath)
	command.Dir = dataDir
	command.Env = sanitizedEnvironment()
	limit := v.OutputLimit
	if limit <= 0 {
		limit = 64 << 10
	}
	output := &limitedBuffer{remaining: limit}
	command.Stdout = output
	command.Stderr = output
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(output.String())
		if message != "" {
			return fmt.Errorf("Mihomo static configuration validation failed: %s: %w", message, err)
		}
		return fmt.Errorf("Mihomo static configuration validation failed: %w", err)
	}
	return nil
}

type limitedBuffer struct {
	buffer    bytes.Buffer
	remaining int
}

func (b *limitedBuffer) Write(value []byte) (int, error) {
	original := len(value)
	if b.remaining <= 0 {
		return original, nil
	}
	if len(value) > b.remaining {
		value = value[:b.remaining]
	}
	written, err := b.buffer.Write(value)
	b.remaining -= written
	if err != nil && !errors.Is(err, io.ErrShortWrite) {
		return written, err
	}
	return original, nil
}

func (b *limitedBuffer) String() string {
	return b.buffer.String()
}
