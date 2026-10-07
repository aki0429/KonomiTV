package arib_test

import (
	"github.com/aki0429/KonomiTV/server-go/internal/tsinfo/arib"
	"testing"
)

// This assertion also verifies the exported signature at compile time.
var _ func([]byte) string = arib.Decode

func TestDecodePublicAPIStartsWithIndependentState(t *testing.T) {
	if got := arib.Decode([]byte{0x0e, 0x41}); got != "Ａ" {
		t.Fatalf("got %q", got)
	}
	if got := arib.Decode([]byte{0x46, 0x7c}); got != "日" {
		t.Fatalf("state leaked: %q", got)
	}
	if got := arib.Decode(nil); got != "" {
		t.Fatalf("nil input = %q", got)
	}
}
