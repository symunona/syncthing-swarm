package provision

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuf is a bytes.Buffer safe to read while the writer under test may still
// be writing from another goroutine.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// A password prompt carries NO trailing newline. If prefixWriter waits for one,
// the user is asked for a password by a prompt they never see — which is the
// whole failure this guards.
func TestPrefixWriterShowsUnterminatedPrompt(t *testing.T) {
	var out syncBuf
	w := prefixWriter(&out, "  | ")

	if _, err := w.Write([]byte("[sudo: authenticate] Password: ")); err != nil {
		t.Fatalf("write: %v", err)
	}

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), "Password: ") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("prompt never reached the terminal; got %q", out.String())
}

func TestPrefixWriterPrefixesEachLine(t *testing.T) {
	var out syncBuf
	w := prefixWriter(&out, "  | ")
	w.Write([]byte("one\ntw"))
	w.Write([]byte("o\n"))

	want := "  | one\n  | two\n"
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && out.String() != want {
		time.Sleep(10 * time.Millisecond)
	}
	if got := out.String(); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
