package voice

import (
	"strconv"
	"strings"
	"testing"
)

func stubLocal(t *testing.T) {
	t.Helper()
	oldS, oldW := isSSH, isWSL
	isSSH = func() bool { return false }
	isWSL = func() bool { return false }
	t.Cleanup(func() { isSSH, isWSL = oldS, oldW })
}

func stubProbes(t *testing.T, has map[string]bool) {
	t.Helper()
	old := probeVersion
	probeVersion = func(name string) bool { return has[name] }
	t.Cleanup(func() { probeVersion = old })
}

func TestAvailabilityRemote(t *testing.T) {
	stubLocal(t)
	isSSH = func() bool { return true }
	stubProbes(t, map[string]bool{"sox": true, "rec": true})
	av := CheckRecordingAvailability()
	if av.Available || !strings.Contains(av.Reason, "no audio device is available in this environment") {
		t.Fatalf("remote: %+v", av)
	}
}

func TestAvailabilityNoSox(t *testing.T) {
	stubLocal(t)
	stubProbes(t, map[string]bool{"brew": true}) // brew only
	av := CheckRecordingAvailability()
	if av.Available || av.Reason != "Voice mode requires SoX for audio recording. Install it with: brew install sox" {
		t.Fatalf("brew hint: %+v", av)
	}
}

func TestAvailabilityNoSoxNoBrew(t *testing.T) {
	stubLocal(t)
	stubProbes(t, map[string]bool{})
	av := CheckRecordingAvailability()
	if av.Available || !strings.Contains(av.Reason, "Install SoX manually") {
		t.Fatalf("manual hint: %+v", av)
	}
}

func TestAvailabilitySoxNoRec(t *testing.T) {
	stubLocal(t)
	stubProbes(t, map[string]bool{"sox": true}) // sox present, rec broken
	av := CheckRecordingAvailability()
	if av.Available || !strings.Contains(av.Reason, "Run Claude Code on a machine with a microphone") {
		t.Fatalf("no-mic: %+v", av)
	}
}

func TestAvailabilityOK(t *testing.T) {
	stubLocal(t)
	stubProbes(t, map[string]bool{"sox": true, "rec": true})
	av := CheckRecordingAvailability()
	if !av.Available {
		t.Fatalf("expected available: %+v", av)
	}
}

func TestAvailabilityWSL(t *testing.T) {
	stubLocal(t)
	isWSL = func() bool { return true }
	stubProbes(t, map[string]bool{"sox": true}) // sox ok, rec fails
	av := CheckRecordingAvailability()
	if av.Available || !strings.Contains(av.Reason, "WSL") {
		t.Fatalf("wsl: %+v", av)
	}
}

func TestCheckVoiceDependencies(t *testing.T) {
	stubProbes(t, map[string]bool{"sox": true})
	d := CheckVoiceDependencies()
	if !d.Available || len(d.Missing) != 0 {
		t.Fatalf("%+v", d)
	}
	stubProbes(t, map[string]bool{"brew": true})
	d = CheckVoiceDependencies()
	if d.Available || d.Missing[0] != "sox (rec command)" || d.InstallCommand != "brew install sox" {
		t.Fatalf("%+v", d)
	}
}

func TestAudioLevel(t *testing.T) {
	if AudioLevel(nil) != 0 {
		t.Fatal("empty chunk")
	}
	// full-scale square wave: rms = 32767 → level = sqrt(min(rms/2000,1)) = 1
	loud := make([]byte, 2000)
	for i := 0; i+1 < len(loud); i += 2 {
		loud[i], loud[i+1] = 0xff, 0x7f
	}
	if got := AudioLevel(loud); got != 1 {
		t.Fatalf("loud level = %v", got)
	}
	// silence → 0
	if got := AudioLevel(make([]byte, 2000)); got != 0 {
		t.Fatalf("silent level = %v", got)
	}
	// modest signal should be > 0.01 (the hadAudioSignal threshold)
	mid := make([]byte, 2000)
	for i := 0; i+1 < len(mid); i += 2 {
		mid[i], mid[i+1] = 0x00, 0x01 // ~256 amplitude
	}
	if got := AudioLevel(mid); got <= 0.01 || got >= 1 {
		t.Fatalf("mid level = %v", got)
	}
}

func TestKeyterms(t *testing.T) {
	kt := Keyterms("/home/u/my-project", []string{"FooBar_test.go", "foo.go"})
	if kt[0] != "MCP" || kt[len(baseKeyterms)] != "my-project" {
		t.Fatalf("kt = %v", kt)
	}
	seen := map[string]bool{}
	for _, k := range kt {
		if seen[k] {
			t.Fatalf("dup keyterm %q in %v", k, kt)
		}
		seen[k] = true
	}
	if !seen["Foo"] || !seen["Bar"] { // camelCase + separator split
		t.Fatalf("expected split terms in %v", kt)
	}
}

func TestSanitizeKeyterms(t *testing.T) {
	got := SanitizeKeyterms([]string{"a,b", "MCP", "MCP", " x  y ", "ünïcode", ""})
	if got != "a b,MCP,x y,ncode" {
		t.Fatalf("got %q", got)
	}
	// 1024-char cap
	var many []string
	for i := 0; i < 200; i++ {
		many = append(many, strings.Repeat("x", 10)+string(rune('a'+i%26))+strings.Repeat("y", 5)+strconv.Itoa(i))
	}
	if got := SanitizeKeyterms(many); len(got) > 1024 {
		t.Fatalf("len %d", len(got))
	}
}

func TestLanguageCode(t *testing.T) {
	cases := []struct{ in, code, fell string }{
		{"", "en", ""},
		{"en", "en", ""},
		{"English", "en", ""},
		{"español", "es", ""},
		{"pt-BR", "pt", ""},
		{"Klingon", "en", "Klingon"},
		{"日本語", "ja", ""},
	}
	for _, c := range cases {
		code, fell := LanguageCode(c.in)
		if code != c.code || fell != c.fell {
			t.Errorf("LanguageCode(%q) = %q,%q want %q,%q", c.in, code, fell, c.code, c.fell)
		}
	}
}
