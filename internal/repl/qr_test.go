package repl

import "testing"

func TestQRMatrix(t *testing.T) {
	m := qrMatrix("https://claude.ai/mobile")
	if m == nil {
		t.Fatal("nil matrix")
	}
	size := len(m)
	// finder corners + centers dark
	for _, c := range [][2]int{{0, 0}, {size - 1, 0}, {0, size - 1}, {3, 3}, {size - 4, 3}, {3, size - 4}} {
		if !m[c[1]][c[0]] {
			t.Fatalf("expected dark module at (%d,%d)", c[0], c[1])
		}
	}
	// finder ring is light at (1,1) diagonal interior edge
	if m[1][1] {
		t.Fatal("finder separator wrong")
	}
	// too-long input returns nil rather than a broken matrix
	if qrMatrix(string(make([]byte, 200))) != nil {
		t.Fatal("oversize input should return nil")
	}
	if len(qrText("https://claude.ai/mobile")) == 0 {
		t.Fatal("qrText empty")
	}
}
