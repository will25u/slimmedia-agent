package protocol

import (
	"strings"
	"testing"
)

func TestCheckJSONDepth_Valid(t *testing.T) {
	cases := []string{
		`{}`,
		`[]`,
		`{"a": 1, "b": "hello"}`,
		`{"nested": {"level1": {"level2": {"level3": "value"}}}}`,
		`[1, [2, [3, [4]]]]`,
	}

	for _, tc := range cases {
		if err := CheckJSONDepth([]byte(tc), 10); err != nil {
			t.Fatalf("expected valid depth for %q, got: %v", tc, err)
		}
	}
}

func TestCheckJSONDepth_ExceedsLimit(t *testing.T) {
	// Build nested JSON of depth 5
	nested := strings.Repeat(`{"k":`, 5) + `"v"` + strings.Repeat(`}`, 5)
	if err := CheckJSONDepth([]byte(nested), 4); err == nil {
		t.Fatal("expected depth check to fail for nesting level 5 with limit 4, got nil")
	}

	// Nested arrays
	nestedArr := strings.Repeat(`[`, 70) + strings.Repeat(`]`, 70)
	if err := CheckJSONDepth([]byte(nestedArr), DefaultMaxJSONDepth); err == nil {
		t.Fatal("expected default limit check to fail for nesting level 70, got nil")
	}
}

func TestUnmarshalWithDepthLimit(t *testing.T) {
	type Sample struct {
		K string `json:"k"`
	}
	var s Sample
	if err := UnmarshalWithDepthLimit([]byte(`{"k": "val"}`), &s, 5); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if s.K != "val" {
		t.Fatalf("expected val, got %q", s.K)
	}

	deep := strings.Repeat(`{"k":`, 10) + `"v"` + strings.Repeat(`}`, 10)
	if err := UnmarshalWithDepthLimit([]byte(deep), &s, 5); err == nil {
		t.Fatal("expected unmarshal to fail on deep nesting, got nil")
	}
}
