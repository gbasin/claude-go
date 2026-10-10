package browsermcp

import (
	"strings"
	"testing"
)

func TestLowerASCII(t *testing.T) {
	if got := lowerASCII("AbC-Ωx"); got != "abc-Ωx" {
		t.Fatalf("lowerASCII = %q", got)
	}
}

func TestStringsTrimSuffix(t *testing.T) {
	// strips the suffix repeatedly, not once
	if got := strings_TrimSuffix("example.com...", "."); got != "example.com" {
		t.Fatalf("TrimSuffix repeated = %q", got)
	}
	if got := strings_TrimSuffix("example.com", "."); got != "example.com" {
		t.Fatalf("TrimSuffix = %q", got)
	}
	if got := strings_TrimSuffix("abc", ""); got != "abc" {
		t.Fatalf("empty suffix should be a no-op, got %q", got)
	}
}

func TestSanitizeString(t *testing.T) {
	if got := sanitizeString("a\nb\tc\x00d", 50); got != "a b c d" {
		t.Fatalf("control chars: %q", got)
	}
	long := strings.Repeat("x", 60)
	if got := sanitizeString(long, 10); got != strings.Repeat("x", 10)+"…" {
		t.Fatalf("cap+ellipsis: %q len=%d", got, len(got))
	}
	if got := sanitizeString("  pad  ", 50); got != "pad" {
		t.Fatalf("trim: %q", got)
	}
}

func TestSanitizeExtension(t *testing.T) {
	e, ok := sanitizeExtension(map[string]any{
		"deviceId": "dev-123", "name": "Chrome on Mac",
		"osPlatform": "darwin", "connectedAt": float64(1000),
	})
	if !ok || e.DeviceID != "dev-123" || e.ConnectedAt != 1000 {
		t.Fatalf("valid ext rejected: %+v %v", e, ok)
	}
	if _, ok := sanitizeExtension(map[string]any{"deviceId": "has spaces!"}); ok {
		t.Fatal("invalid deviceId accepted")
	}
	if _, ok := sanitizeExtension(map[string]any{}); ok {
		t.Fatal("missing deviceId accepted")
	}
}

func TestSanitizeBridgeNotices(t *testing.T) {
	in := []any{
		map[string]any{"kind": "RATE_LIMITED", "domain": "EXAMPLE.com."},
		map[string]any{"kind": "no-kind-here!"},   // bad kind
		map[string]any{"kind": "x", "domain": 42}, // non-string domain
		"not-a-map", // junk
		map[string]any{"kind": "perm", "domain": "a.b"}, // ok
		map[string]any{"kind": "rate_limited", "domain": "example.com"},
	}
	got := sanitizeBridgeNotices(in, nil)
	// entry 1 and 6 dedupe to the same kind:domain slot (lowercased, dot stripped)
	if len(got) != 2 {
		t.Fatalf("notices: %+v", got)
	}
	if got[0].Kind != "rate_limited" || got[0].Domain != "example.com" {
		t.Fatalf("first: %+v", got[0])
	}
	if got[1].Kind != "perm" || got[1].Domain != "a.b" {
		t.Fatalf("second: %+v", got[1])
	}
}

func TestSanitizeBridgeNoticesCap(t *testing.T) {
	var in []any
	for i := 0; i < 8; i++ {
		in = append(in, map[string]any{
			"kind": "k", "domain": strings.Repeat("a", i+1) + ".x"})
	}
	var warns []string
	got := sanitizeBridgeNotices(in, func(s string) { warns = append(warns, s) })
	if len(got) > 4 {
		t.Fatalf("cap not enforced: %d", len(got))
	}
	if len(warns) == 0 {
		t.Fatal("cap should warn")
	}
}

func TestNormalizeBridgeResponse(t *testing.T) {
	// result passthrough
	out := normalizeBridgeResponse(map[string]any{"result": map[string]any{"x": 1}})
	if _, ok := out["result"]; !ok {
		t.Fatalf("result lost: %v", out)
	}
	// error passthrough
	out = normalizeBridgeResponse(map[string]any{"error": "boom"})
	if _, ok := out["error"]; !ok {
		t.Fatalf("error lost: %v", out)
	}
	// content → result, is_error → error
	out = normalizeBridgeResponse(map[string]any{"content": "c"})
	if _, ok := out["result"]; !ok {
		t.Fatalf("content→result: %v", out)
	}
	out = normalizeBridgeResponse(map[string]any{"content": "c", "is_error": true})
	if _, ok := out["error"]; !ok {
		t.Fatalf("content→error: %v", out)
	}
	// notices get re-sanitized and dropped when invalid
	out = normalizeBridgeResponse(map[string]any{
		"result": 1, "notices": []any{map[string]any{"kind": "BAD KIND!!"}}})
	if _, has := out["notices"]; has {
		t.Fatalf("invalid notices should be dropped: %v", out)
	}
}

func TestPickExtension(t *testing.T) {
	e1 := extensionInfo{DeviceID: "a"}
	e2 := extensionInfo{DeviceID: "b"}
	e3 := extensionInfo{DeviceID: "c"}

	if r := pickExtension(nil, "", nil); r.kind != "none" {
		t.Fatalf("empty: %+v", r)
	}
	// persisted id wins
	if r := pickExtension([]extensionInfo{e1, e2}, "b", nil); r.kind != "select" || r.reason != "persisted" || r.extension.DeviceID != "b" {
		t.Fatalf("persisted: %+v", r)
	}
	// single ext selected with reason "only"
	if r := pickExtension([]extensionInfo{e1}, "", nil); r.reason != "only" {
		t.Fatalf("only: %+v", r)
	}
	// single live local ext auto-selects
	if r := pickExtension([]extensionInfo{e1, e2, e3}, "", map[string]string{"b": "live"}); r.reason != "local" || r.extension.DeviceID != "b" {
		t.Fatalf("local: %+v", r)
	}
	// ambiguous → ask, local first then rest
	r := pickExtension([]extensionInfo{e1, e2, e3}, "", map[string]string{"a": "live", "b": "dead"})
	if r.kind != "ask" || len(r.candidates) != 3 || r.candidates[0].DeviceID != "a" {
		t.Fatalf("ask: %+v", r)
	}
}

func TestOsNameForPlatform(t *testing.T) {
	for p, want := range map[string]string{
		"darwin": "macOS", "win32": "Windows", "windows": "Windows",
		"linux": "Linux", "freebsd": "Linux"} {
		if got := osNameForPlatform(p); got != want {
			t.Fatalf("%s → %q, want %q", p, got, want)
		}
	}
}

func TestSmallHelpers(t *testing.T) {
	if orNone("") != "none" || orNone("x") != "x" {
		t.Fatal("orNone")
	}
	if minInt(3, 5) != 3 || minInt(5, 3) != 3 {
		t.Fatal("minInt")
	}
	if got := toMapSlice([]any{map[string]any{"a": 1}, "junk", map[string]any{"b": 2}}); len(got) != 2 {
		t.Fatalf("toMapSlice: %v", got)
	}
}
