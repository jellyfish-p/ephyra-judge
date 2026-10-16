//go:build !linux

package sandbox

import (
	"context"
	"strings"
	"testing"
)

func TestDefaultHostUnavailable(t *testing.T) {
	h := DefaultHost()
	_, err := h.NewSession(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "linux") {
		t.Fatalf("error = %v", err)
	}
}
