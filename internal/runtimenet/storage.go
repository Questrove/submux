package runtimenet

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"submux/internal/safepath"
)

const (
	integrityKeyFile = "ownership.key"
	ownershipFile    = "ownership.json"
	maxStateFileSize = 8 << 20
)

type durableState struct {
	Ownership *Ownership        `json:"ownership,omitempty"`
	Results   []CommittedResult `json:"results,omitempty"`
}

type signedState struct {
	State durableState `json:"state"`
	MAC   string       `json:"mac"`
}

func openIntegrityKey(root string) ([]byte, error) {
	root, err := prepareRoot(root)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(root, integrityKeyFile)
	body, err := readPrivateRegularFile(path, 32)
	if err == nil {
		if len(body) != 32 {
			return nil, errors.New("privileged Runtime network integrity key is invalid")
		}
		return body, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	body = make([]byte, 32)
	if _, err := rand.Read(body); err != nil {
		return nil, err
	}
	if err := writeExclusivePrivateFile(path, body); err != nil {
		if errors.Is(err, os.ErrExist) {
			return openIntegrityKey(root)
		}
		return nil, err
	}
	return body, nil
}

func writeState(root string, key []byte, state durableState) error {
	root, err := prepareRoot(root)
	if err != nil {
		return err
	}
	body, err := json.Marshal(state)
	if err != nil {
		return err
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(body)
	envelope, err := json.Marshal(signedState{
		State: state,
		MAC:   hex.EncodeToString(mac.Sum(nil)),
	})
	if err != nil {
		return err
	}
	return replacePrivateFile(filepath.Join(root, ownershipFile), envelope)
}

func readState(root string, key []byte) (durableState, error) {
	root, err := prepareRoot(root)
	if err != nil {
		return durableState{}, err
	}
	path := filepath.Join(root, ownershipFile)
	body, err := readPrivateRegularFile(path, maxStateFileSize)
	if errors.Is(err, os.ErrNotExist) {
		return durableState{}, nil
	}
	if err != nil {
		return durableState{}, err
	}
	var envelope signedState
	if err := json.Unmarshal(body, &envelope); err != nil {
		return durableState{}, errors.New("privileged Runtime network state is invalid")
	}
	stateBody, err := json.Marshal(envelope.State)
	if err != nil {
		return durableState{}, err
	}
	expected, err := hex.DecodeString(envelope.MAC)
	if err != nil {
		return durableState{}, errors.New("privileged Runtime network state integrity record is invalid")
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(stateBody)
	if !hmac.Equal(expected, mac.Sum(nil)) {
		return durableState{}, errors.New("privileged Runtime network state integrity check failed")
	}
	return envelope.State, nil
}

func prepareRoot(root string) (string, error) {
	if root == "" || !filepath.IsAbs(root) {
		return "", errors.New("privileged Runtime network state root must be absolute")
	}
	absolute, err := filepath.Abs(root)
	if err != nil || absolute == filepath.VolumeName(absolute)+string(filepath.Separator) {
		return "", errors.New("privileged Runtime network state root must be a fixed non-root path")
	}
	linked, err := safepath.ContainsLinkInExistingPath(absolute)
	if err != nil {
		return "", fmt.Errorf("inspect privileged Runtime network state root: %w", err)
	}
	if linked {
		return "", errors.New("privileged Runtime network state root must not contain links")
	}
	if err := os.MkdirAll(absolute, 0700); err != nil {
		return "", err
	}
	if err := os.Chmod(absolute, 0700); err != nil {
		return "", err
	}
	linked, err = safepath.ContainsLink(absolute)
	if err != nil {
		return "", err
	}
	if linked {
		return "", errors.New("privileged Runtime network state root must not contain links")
	}
	return absolute, nil
}

func writeExclusivePrivateFile(path string, body []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	return file.Close()
}

func readPrivateRegularFile(path string, maximum int64) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, errors.New("privileged Runtime network state file must be a regular file")
	}
	if runtime.GOOS != "windows" && before.Mode().Perm()&0077 != 0 {
		return nil, errors.New("privileged Runtime network state file permissions are too broad")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	after, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if after.Mode()&os.ModeSymlink != 0 ||
		!after.Mode().IsRegular() ||
		!os.SameFile(before, opened) ||
		!os.SameFile(opened, after) {
		return nil, errors.New("privileged Runtime network state file changed while opening")
	}
	body, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maximum {
		return nil, errors.New("privileged Runtime network state file exceeds the size limit")
	}
	return body, nil
}

func replacePrivateFile(path string, body []byte) error {
	temp := path + ".tmp"
	if err := os.Remove(temp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := writeExclusivePrivateFile(temp, body); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			_ = os.Remove(temp)
			return errors.New("privileged Runtime network state file must be a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(temp)
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		_ = os.Remove(temp)
		return err
	}
	return os.Chmod(path, 0600)
}
