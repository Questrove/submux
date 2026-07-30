package runtimeinstall

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

const maximumReceiptBytes = 64 << 10

func ReadReceipt(name string) (*Receipt, error) {
	if name == "" || !filepath.IsAbs(name) || filepath.Clean(name) != name {
		return nil, errors.New("Runtime installation receipt path is invalid")
	}
	body, err := os.ReadFile(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(body) == 0 || len(body) > maximumReceiptBytes {
		return nil, errors.New("Runtime installation receipt size is invalid")
	}
	receipt, err := DecodeReceipt(body)
	if err != nil {
		return nil, err
	}
	return &receipt, nil
}

func WriteReceipt(name string, receipt Receipt) error {
	if name == "" || !filepath.IsAbs(name) || filepath.Clean(name) != name {
		return errors.New("Runtime installation receipt path is invalid")
	}
	if err := ValidateReceipt(receipt); err != nil {
		return err
	}
	parent := filepath.Dir(name)
	info, err := os.Lstat(parent)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("Runtime installation receipt directory is invalid")
	}
	body, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	temporary, err := os.CreateTemp(parent, ".install-receipt-*.tmp")
	if err != nil {
		return err
	}
	success := false
	defer func() {
		_ = temporary.Close()
		if !success {
			_ = os.Remove(temporary.Name())
		}
	}()
	if _, err := temporary.Write(body); err != nil {
		return err
	}
	if err := temporary.Chmod(0600); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := replaceReceiptFile(temporary.Name(), name); err != nil {
		return err
	}
	success = true
	return nil
}
