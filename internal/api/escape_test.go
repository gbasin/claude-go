package api

import "testing"

func TestUnescapeHTMLSeq(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"a":"\u003ctag\u003e"}`, `{"a":"<tag>"}`},
		{`{"a":"x & \u0026 y"}`, `{"a":"x & & y"}`},
		{`{"a":"\\u003c"}`, `{"a":"\\u003c"}`}, // literal backslash text — untouched
		{`{"a":"plain"}`, `{"a":"plain"}`},
		{`{"a":"\u003C\u003E\u0026"}`, `{"a":"<>&"}`}, // case-insensitive
		{`{"a":"\u0041"}`, `{"a":"\u0041"}`},          // other escapes pass through
	}
	for _, c := range cases {
		if got := string(unescapeHTMLSeq([]byte(c.in))); got != c.want {
			t.Errorf("unescapeHTMLSeq(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
