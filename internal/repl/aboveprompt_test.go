package repl

import "testing"

func TestAbovePromptChord(t *testing.T) {
	e := &editor{}
	e.loadKeybindings()
	if _, ok := e.bindingKey(key{b: 0x18}); !ok {
		t.Fatalf("ctrl+x not consumed as prefix")
	}
	if len(e.pendingChord) == 0 {
		t.Fatalf("ctrl+x did not arm the chord")
	}
	if _, ok := e.bindingKey(key{b: 0x01}); !ok {
		t.Fatalf("ctrl+x ctrl+a not bound")
	}
	if !e.abovePrompt {
		t.Fatalf("abovePrompt not toggled")
	}
}
