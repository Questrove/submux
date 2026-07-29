package runtimeipc

import (
	"errors"
	"strings"
	"testing"
)

type strictFixture struct {
	Name string `json:"name"`
}

func TestDecodeStrictJSON(t *testing.T) {
	tests := []struct {
		name string
		body string
		err  error
	}{
		{name: "valid", body: `{"name":"runtime"}`},
		{name: "unknown field", body: `{"name":"runtime","extra":true}`, err: errors.New("unknown")},
		{name: "duplicate field", body: `{"name":"one","name":"two"}`, err: ErrDuplicateJSONField},
		{name: "oversize", body: `{"name":"runtime"}`, err: ErrJSONTooLarge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var fixture strictFixture
			limit := int64(MaxRequestBytes)
			if test.name == "oversize" {
				limit = 4
			}
			err := decodeStrictJSON(strings.NewReader(test.body), limit, &fixture)
			if test.err == nil && err != nil {
				t.Fatalf("decode strict JSON: %v", err)
			}
			if test.err != nil && err == nil {
				t.Fatalf("decode strict JSON succeeded, want %v", test.err)
			}
			if errors.Is(test.err, ErrDuplicateJSONField) && !errors.Is(err, ErrDuplicateJSONField) {
				t.Fatalf("error = %v, want duplicate field", err)
			}
		})
	}
}

func TestDecodeStrictJSONRejectsDeepNesting(t *testing.T) {
	body := strings.Repeat(`[`, MaxJSONDepth+1) + `0` + strings.Repeat(`]`, MaxJSONDepth+1)
	var destination any
	err := decodeStrictJSON(strings.NewReader(body), MaxRequestBytes, &destination)
	if !errors.Is(err, ErrJSONTooDeep) {
		t.Fatalf("error = %v, want %v", err, ErrJSONTooDeep)
	}
}

func TestDecodeStrictJSONAcceptsDepthLimit(t *testing.T) {
	body := strings.Repeat(`[`, MaxJSONDepth) + `0` + strings.Repeat(`]`, MaxJSONDepth)
	var destination any
	if err := decodeStrictJSON(strings.NewReader(body), MaxRequestBytes, &destination); err != nil {
		t.Fatalf("decode JSON at depth limit: %v", err)
	}
}

func TestDecodeStrictJSONRejectsLongArray(t *testing.T) {
	body := "[" + strings.Repeat("0,", MaxJSONArray) + "0]"
	var destination any
	err := decodeStrictJSON(strings.NewReader(body), MaxRequestBytes, &destination)
	if !errors.Is(err, ErrJSONArrayTooLong) {
		t.Fatalf("error = %v, want %v", err, ErrJSONArrayTooLong)
	}
}
