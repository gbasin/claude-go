package termemu

import (
	"strings"
	"testing"
)

func TestBasics(t *testing.T) {
	s := New(40, 6)
	s.Feed([]byte("hello\r\nworld"))
	if got := s.Lines(); len(got) < 2 || got[0] != "hello" || got[1] != "world" {
		t.Fatalf("got %q", got)
	}
}

func TestCursorUpErase(t *testing.T) {
	s := New(40, 6)
	s.Feed([]byte("row1\r\nrow2\r\nrow3"))
	s.Feed([]byte("\x1b[1A\r\x1b[KEDIT"))
	got := s.Lines()
	if got[1] != "EDIT" {
		t.Fatalf("row2 = %q", got[1])
	}
	if got[0] != "row1" || got[2] != "row3" {
		t.Fatalf("wrong rows: %q", got)
	}
}

func TestScrollRegion(t *testing.T) {
	s := New(40, 6)
	s.Feed([]byte("\x1b[1;4r")) // region rows 1-4
	// 8 writes with trailing newline: last scroll pops one — expect 3
	// "line" rows + blank; the LAST LF scrolled its own row up
	for i := 0; i < 8; i++ {
		s.Feed([]byte("line\r\n"))
	}
	got := s.Lines()
	for i := 0; i < 3; i++ {
		if got[i] != "line" {
			t.Fatalf("row %d = %q", i, got[i])
		}
	}
	// row outside the region (index 4) must be untouched/blank
	if len(got) > 4 && got[4] != "" {
		t.Fatalf("row outside region written: %q", got)
	}
}

func TestZoneRepaint(t *testing.T) {
	// emulate: stream writes lines, then a 2-row zone paints at bottom,
	// then repaints — the key compositor pattern
	s := New(40, 10)
	s.Feed([]byte("history line\r\n"))
	// install zone: scroll 2 rows at bottom, pin region 1..8
	s.Feed([]byte("\x1b[r\x1b[10;1H\x1b[2S\x1b[1;8r"))
	s.Feed([]byte("\x1b[9;1H\x1b[2KSPINNER1\x1b[10;1H\x1b[2Kstatus1"))
	// repaint: change spinner row only
	s.Feed([]byte("\x1b[9;1H\x1b[2KSPINNER2"))
	got := s.Lines()
	// the \x1b[2S install scroll pushed "history line" to scrollback
	// (emulator has no scrollback) — assert the zone rows only
	if got[8] != "SPINNER2" || got[9] != "status1" {
		t.Fatalf("got %q", got)
	}
}

func TestCollapseResidue(t *testing.T) {
	// zone shrink: 3-row progress block released — the rows must clear
	s := New(40, 10)
	s.Feed([]byte("hist\r\n"))
	s.Feed([]byte("\x1b[r\x1b[10;1H\x1b[4S\x1b[1;6r"))
	s.Feed([]byte("\x1b[7;1H\x1b[2Kp1\x1b[8;1H\x1b[2Kp2\x1b[9;1H\x1b[2Kp3\x1b[10;1H\x1b[2Kstatus"))
	// collapse to 1 row: clear rows 7-9, re-pin region 1..9, repaint row 10
	s.Feed([]byte("\x1b[7;1H\x1b[2K\x1b[8;1H\x1b[2K\x1b[9;1H\x1b[2K\x1b[1;9r\x1b[10;1H\x1b[2Kstatus"))
	got := s.Lines()
	full := strings.Join(got, "\n")
	if strings.Contains(full, "p1") || strings.Contains(full, "p2") || strings.Contains(full, "p3") {
		t.Fatalf("residue after collapse: %q", got)
	}
	// "hist" scrolled to scrollback during the install scroll — the
	// collapse must leave only the status row painted
	if got[9] != "status" {
		t.Fatalf("got %q", got)
	}
}

func TestScrollbackCapturesHistory(t *testing.T) {
	s := New(40, 4)
	s.Feed([]byte("\x1b[1;4r")) // full region
	for i := 0; i < 7; i++ {
		s.Feed([]byte("line" + string(rune('a'+i)) + "\r\n"))
	}
	hist := s.Scrollback()
	if len(hist) == 0 {
		t.Fatal("no scrollback captured")
	}
	// the earliest lines scrolled off — linea should be in scrollback
	joined := ""
	for _, r := range hist {
		joined += r + "\n"
	}
	if !strings.Contains(joined, "linea") {
		t.Fatalf("scrollback missing earliest rows: %q", hist)
	}
	full := s.History()
	if len(full) != len(hist)+len(s.Lines()) {
		t.Fatalf("History() = %d rows, want sb+grid", len(full))
	}
}

func TestWideCells(t *testing.T) {
	s := New(10, 4)
	// 世 is double-width → occupies two cells; next char lands at col+2
	s.Feed([]byte("世x"))
	got := s.Lines()
	if got[0] != "世x" {
		t.Fatalf("got %q", got)
	}
	if cy, cx := s.Cursor(); cy != 0 || cx != 3 {
		t.Fatalf("cursor = %d,%d — wide glyph must consume 2 cells", cy, cx)
	}
	// overwrite the continuation tail — the head clears too
	s.Feed([]byte("\x1b[1;2H_"))
	got = s.Lines()
	if got[0] != " _x" {
		t.Fatalf("tail overwrite: %q", got)
	}
}

func TestCombiningMerges(t *testing.T) {
	s := New(10, 4)
	s.Feed([]byte("e\xcc\x81x")) // e + combining acute
	if got := s.Lines(); got[0] != "ex" {
		t.Fatalf("got %q", got)
	}
}
