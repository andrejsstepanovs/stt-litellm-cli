package main

import (
	"testing"
)

func TestCopyToClipboard(t *testing.T) {
	text := "héllo wörld — stt ✓"
	if !copyToClipboard(text) {
		t.Fatal("copyToClipboard returned false")
	}
}
