package runtimeipc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	HeaderRequestID       = "X-Submux-Request-ID"
	HeaderProtocolVersion = "X-Submux-Protocol-Version"
	HeaderClientVersion   = "X-Submux-Client-Version"
	HeaderContentSize     = "X-Submux-Content-Size"
	HeaderContentSHA256   = "X-Submux-Content-SHA256"

	MaxRequestBytes  = 1 << 20
	MaxResponseBytes = 12 << 20
	MaxJSONDepth     = 32
	MaxJSONString    = 64 << 10
	MaxJSONArray     = 4096
)

var (
	ErrDuplicateJSONField = errors.New("JSON contains a duplicate object field")
	ErrJSONTooDeep        = errors.New("JSON nesting exceeds the protocol limit")
	ErrJSONStringTooLong  = errors.New("JSON string exceeds the protocol limit")
	ErrJSONArrayTooLong   = errors.New("JSON array exceeds the protocol limit")
	ErrJSONTooLarge       = errors.New("JSON body exceeds the protocol limit")
)

func decodeStrictJSON(reader io.Reader, limit int64, destination any) error {
	if reader == nil {
		return errors.New("JSON body is required")
	}
	if limit <= 0 {
		return errors.New("JSON size limit must be positive")
	}
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return fmt.Errorf("read JSON body: %w", err)
	}
	if int64(len(body)) > limit {
		return ErrJSONTooLarge
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return errors.New("JSON body is empty")
	}

	validator := json.NewDecoder(bytes.NewReader(body))
	validator.UseNumber()
	if err := validateJSONValue(validator, 0); err != nil {
		return err
	}
	if token, err := validator.Token(); err != io.EOF {
		if err != nil {
			return fmt.Errorf("validate JSON body: %w", err)
		}
		return fmt.Errorf("JSON body contains trailing value %v", token)
	}

	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode JSON body: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("JSON body contains trailing data")
		}
		return fmt.Errorf("decode trailing JSON data: %w", err)
	}
	return nil
}

func validateJSONValue(decoder *json.Decoder, depth int) error {
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("read JSON token: %w", err)
	}
	switch value := token.(type) {
	case json.Delim:
		if depth >= MaxJSONDepth {
			return ErrJSONTooDeep
		}
		switch value {
		case '{':
			seen := make(map[string]struct{})
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return fmt.Errorf("read JSON object field: %w", err)
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("JSON object field name is not a string")
				}
				if len(key) > MaxJSONString {
					return ErrJSONStringTooLong
				}
				if _, exists := seen[key]; exists {
					return fmt.Errorf("%w: %s", ErrDuplicateJSONField, key)
				}
				seen[key] = struct{}{}
				if err := validateJSONValue(decoder, depth+1); err != nil {
					return err
				}
			}
			if _, err := decoder.Token(); err != nil {
				return fmt.Errorf("close JSON object: %w", err)
			}
		case '[':
			count := 0
			for decoder.More() {
				count++
				if count > MaxJSONArray {
					return ErrJSONArrayTooLong
				}
				if err := validateJSONValue(decoder, depth+1); err != nil {
					return err
				}
			}
			if _, err := decoder.Token(); err != nil {
				return fmt.Errorf("close JSON array: %w", err)
			}
		default:
			return fmt.Errorf("unexpected JSON delimiter %q", value)
		}
	case string:
		if len(value) > MaxJSONString {
			return ErrJSONStringTooLong
		}
	}
	return nil
}
