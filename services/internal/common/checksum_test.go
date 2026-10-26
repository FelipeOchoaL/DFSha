package common

import (
	"strings"
	"testing"
)

func TestSHA256Hex(t *testing.T) {
	got := SHA256Hex([]byte("hello world"))
	want := "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"
	if got != want {
		t.Fatalf("SHA256Hex() = %q, want %q", got, want)
	}
}

func TestSHA256HexReader(t *testing.T) {
	got, err := SHA256HexReader(strings.NewReader("hello world"))
	if err != nil {
		t.Fatalf("SHA256HexReader() error = %v", err)
	}

	want := "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"
	if got != want {
		t.Fatalf("SHA256HexReader() = %q, want %q", got, want)
	}
}
