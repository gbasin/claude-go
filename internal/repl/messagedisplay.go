package repl

import (
	"crypto/rand"
	"fmt"
	"strings"
	"time"

	"claude-go/internal/hooks"
)

// msgDisplay implements real's MessageDisplay hook lane (mod_1378.js):
// assistant text deltas are batched at line boundaries; each batch fires
// a hook whose hookSpecificOutput.displayContent replaces ONLY what is
// rendered — the stored message the model sees is untouched.
//
// Payload: turn_id, message_id (uuid per text run, stable across
// flushes), index (zero-based batch), final, delta. Real fires sync and
// caps flushes ~10/s; a failing/slow hook renders the original delta.
type msgDisplay struct {
	fire    func(payload map[string]any) hooks.Result
	render  func(string)
	turnID  string
	msgID   string
	pending strings.Builder
	index   int
	lastAt  time.Time
}

const msgDisplayMinFlush = 100 * time.Millisecond

func newMsgDisplay(fire func(map[string]any) hooks.Result, render func(string)) *msgDisplay {
	return &msgDisplay{fire: fire, render: render, turnID: newUUIDShort(), msgID: newUUIDShort()}
}

// Delta accumulates raw text; complete lines (through the last \n)
// flush as one batch. Mid-line tails hold until more text arrives or
// Finalize runs.
func (m *msgDisplay) Delta(t string) {
	if m == nil {
		return
	}
	m.pending.WriteString(t)
	s := m.pending.String()
	i := strings.LastIndexByte(s, '\n')
	if i < 0 {
		return
	}
	batch := s[:i+1]
	m.pending.Reset()
	m.pending.WriteString(s[i+1:])
	m.flush(batch, false)
}

// Finalize emits whatever remains as the last batch (final:true) and
// starts a new message_id — real flushes the tail even mid-line.
func (m *msgDisplay) Finalize() {
	if m == nil || m.pending.Len() == 0 {
		return
	}
	s := m.pending.String()
	m.pending.Reset()
	m.flush(s, true)
	m.msgID = newUUIDShort()
}

// NewTurn rotates the turn/message ids — real's newTurn().
func (m *msgDisplay) NewTurn() {
	if m == nil {
		return
	}
	m.Finalize()
	m.turnID, m.msgID = newUUIDShort(), newUUIDShort()
	m.index = 0
}

func (m *msgDisplay) flush(delta string, final bool) {
	if m.fire == nil || delta == "" {
		return
	}
	if !final {
		// real throttles intermediate flushes (10/s); the tail renders
		// raw if we skip — accumulate instead of dropping
		if d := time.Since(m.lastAt); d < msgDisplayMinFlush {
			time.Sleep(msgDisplayMinFlush - d)
		}
	}
	m.lastAt = time.Now()
	res := m.fire(map[string]any{
		"turn_id":    m.turnID,
		"message_id": m.msgID,
		"index":      m.index,
		"final":      final,
		"delta":      delta,
	})
	m.index++
	if res.DisplayContent != "" {
		delta = res.DisplayContent
	}
	m.render(delta)
}

func newUUIDShort() string {
	var b [16]byte
	rand.Read(b[:])
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
