package rag

import "testing"

func TestParse(t *testing.T) {
	h := parseSearch([]any{int64(2), "k1", []any{"kind", "lore", "text", "hello", "score", "0.3"}, "k2", []any{"kind", "memory", "text", "x", "score", "0.1"}})
	if len(h) != 2 || h[0].Text != "x" {
		t.Fatal(h)
	}
}
