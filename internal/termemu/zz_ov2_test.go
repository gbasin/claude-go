package termemu

import (
	"fmt"
	"os"
	"testing"
)

func TestOverlayInRaw(t *testing.T) {
	raw, err := os.ReadFile("/tmp/apdump")
	if err != nil {
		t.Skip("no dump")
	}
	s := New(120, 40)
	s.Feed(raw)
	for i, ln := range s.Lines() {
		fmt.Printf("  %02d %q\n", i, ln)
	}
}
