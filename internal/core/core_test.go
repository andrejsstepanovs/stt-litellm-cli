package core

import (
	"testing"
)

func TestCopyToClipboard(t *testing.T) {
	text := "héllo wörld — stt ✓"
	if !CopyToClipboard(text) {
		t.Fatal("copyToClipboard returned false")
	}
}
