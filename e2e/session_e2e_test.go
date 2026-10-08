//go:build e2e

// session_e2e_test.go — transcript persistence coverage:
//   - ~/.claude/projects/<slug>/<sid>.jsonl written in print mode
//   - record fields (type, uuid, parentUuid chain, sessionId, message)
//   - --session-id honored (file named <sid>.jsonl)
//   - -r <sid> resume: continuation lands in the SAME transcript —
//     the new turn appends to <sid>.jsonl parentUuid-chained to the
//     loaded branch's leaf (real's -r semantics)
//   - -c continue (pins current shadowed-by-new-transcript behavior)
//   - --fork-session writes a forkedFrom marker into the new transcript
//   - --no-session-persistence writes nothing
package e2e

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// TestTranscriptWritten: -p writes a valid JSONL transcript with a
// parentUuid-linked user/assistant chain under the test HOME.
func TestTranscriptWritten(t *testing.T) {
	s := newSession(t)
	sid := newUUID()
	r := s.run(t, "--mock", "-p", "transcript check", "--session-id", sid)
	s.mustOK(t, r)

	path := s.transcriptPath(t, sid)
	if path == "" {
		t.Fatalf("no transcript for %s; found: %v", sid, s.transcriptPaths(t))
	}
	recs := readRecords(t, path)
	if len(recs) == 0 {
		t.Fatal("transcript is empty")
	}
	var sawUser, sawAssistant bool
	for _, rec := range recs {
		// every record: uuid + sessionId + timestamp present
		if u, _ := rec["uuid"].(string); u == "" {
			t.Fatalf("record missing uuid: %v", rec)
		}
		if rec["sessionId"] != sid {
			t.Fatalf("record sessionId mismatch: %v", rec["sessionId"])
		}
		if ts, _ := rec["timestamp"].(string); ts == "" {
			t.Fatalf("record missing timestamp: %v", rec)
		}
		typ, _ := rec["type"].(string)
		if typ == "user" || typ == "assistant" {
			if typ == "user" {
				sawUser = true
			} else {
				sawAssistant = true
			}
			msg, _ := rec["message"].(map[string]any)
			if msg["role"] == nil || msg["content"] == nil {
				t.Fatalf("%s record missing message role/content: %v", typ, rec)
			}
		}
	}
	if !sawUser || !sawAssistant {
		t.Fatalf("expected user+assistant records, saw user=%v assistant=%v\n%v",
			sawUser, sawAssistant, recs)
	}
	// chain walk: every parentUuid must reference an earlier record's uuid
	for i, rec := range recs {
		pu, _ := rec["parentUuid"].(string)
		if pu == "" {
			continue
		}
		found := false
		for j := 0; j < i; j++ {
			if recs[j]["uuid"] == pu {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("record %d parentUuid %q has no earlier record", i, pu)
		}
	}
	raw := mustReadFile(t, path)
	contains(t, raw, "transcript check", "transcript content")
}

// TestSessionIDFlag: --session-id names the transcript file.
func TestSessionIDFlag(t *testing.T) {
	s := newSession(t)
	sid := "11111111-2222-4333-8444-555555555555"
	r := s.run(t, "--mock", "-p", "sid check", "--session-id", sid)
	s.mustOK(t, r)
	if p := s.transcriptPath(t, sid); p == "" {
		t.Fatalf("expected transcript %s.jsonl; found %v", sid, s.transcriptPaths(t))
	}
}

// TestResume: -r <sid> loads prior messages and continues — real continues
// the SAME transcript: the follow-up turn appends to <sid>.jsonl with
// parentUuid chained to the loaded branch's leaf.
func TestResume(t *testing.T) {
	s := newSession(t)
	sid := newUUID()
	r := s.run(t, "--mock", "-p", "seed-prompt-e2e", "--session-id", sid)
	s.mustOK(t, r)

	r2 := s.run(t, "--mock", "-r", sid, "-p", "follow-up-e2e")
	s.mustOK(t, r2)
	contains(t, r2.stderr, "resumed session", "resume notice")

	paths := s.transcriptPaths(t)
	if len(paths) != 1 {
		t.Fatalf("resume should continue the same transcript; found %v", paths)
	}
	raw := mustReadFile(t, paths[0])
	contains(t, raw, "seed-prompt-e2e", "continued transcript keeps history")
	contains(t, raw, "follow-up-e2e", "continued transcript gets the new turn")

	// the follow-up's user record is parentUuid-chained to the seed leaf —
	// real links the branch rather than starting a new root
	type rec struct {
		UUID       string `json:"uuid"`
		ParentUUID string `json:"parentUuid"`
		Text       string `json:"-"`
	}
	uuids := map[string]bool{}
	var followParent string
	for _, line := range strings.Split(raw, "\n") {
		var r rec
		if json.Unmarshal([]byte(line), &r) != nil || r.UUID == "" {
			continue
		}
		uuids[r.UUID] = true
		if strings.Contains(line, "follow-up-e2e") && followParent == "" {
			followParent = r.ParentUUID
		}
	}
	if followParent == "" || !uuids[followParent] {
		t.Fatalf("follow-up not chained into the resumed branch (parentUuid=%q)", followParent)
	}
}

// TestResumeDropsTurn: --resume-drops-turn <uuid> is real's safety guard —
// resuming at an anchor discards the tail; the flag declares that tail is
// exactly the declared turn. Wrong declaration → refused.
func TestResumeDropsTurn(t *testing.T) {
	s := newSession(t)
	sid := newUUID()
	r := s.run(t, "--mock", "-p", "guard-turn-one", "--session-id", sid)
	s.mustOK(t, r)
	r = s.run(t, "--mock", "-r", sid, "-p", "guard-turn-two")
	s.mustOK(t, r)

	// pull uuids: turn1's assistant reply (the anchor) + turn2's user prompt
	var asst1, user2 string
	recs := readRecords(t, s.transcriptPath(t, sid))
	for i, rec := range recs {
		line, _ := json.Marshal(rec)
		if strings.Contains(string(line), "guard-turn-one") && i+1 < len(recs) {
			asst1, _ = recs[i+1]["uuid"].(string) // the reply after turn one
		}
		if strings.Contains(string(line), "guard-turn-two") {
			user2, _ = rec["uuid"].(string)
		}
	}
	if asst1 == "" || user2 == "" {
		t.Fatalf("missing anchor/turn uuids in transcript (asst=%q user=%q)", asst1, user2)
	}

	// guard satisfied: the discarded tail is exactly turn two
	r = s.run(t, "--mock", "-r", sid, "--resume-session-at", asst1,
		"--resume-drops-turn", user2, "-p", "redo-turn-two")
	s.mustOK(t, r)
	contains(t, r.stderr, "resumed session", "guarded resume notice")

	// guard refused: declared id isn't the tail's turn
	r = s.run(t, "--mock", "-r", sid, "--resume-session-at", asst1,
		"--resume-drops-turn", asst1, "-p", "redo-turn-two")
	contains(t, r.stderr, "not attributable to turn", "guard refusal")

	// wiring: drops-turn without resume-session-at is rejected outright
	r = s.run(t, "--mock", "-r", sid, "--resume-drops-turn", user2, "-p", "x")
	contains(t, r.stderr, "requires --resume-session-at", "wiring error")
	if r.code == 0 {
		t.Fatal("drops-turn without resume-session-at should exit nonzero")
	}
}

// TestRewindFiles: --rewind-files <user-msg-uuid> restores tracked files
// to their state at that message — real's standalone resume op.
func TestRewindFiles(t *testing.T) {
	s := newSession(t)
	sid := newUUID()
	track := s.writeFile("track.txt", "v1")

	// scripted Read+Write (Write refuses unread files) → snapshot + backup
	r := s.run(t, "--mock", "-p",
		`calls:[{"name":"Read","input":{"file_path":"`+track+`"}},`+
			`{"name":"Write","input":{"file_path":"`+track+`","content":"v2"}}]`,
		"--session-id", sid, "--permission-mode", "bypassPermissions")
	s.mustOK(t, r)
	contains(t, mustReadFile(t, filepath.Join(s.cwd, "track.txt")), "v2", "write applied")

	// anchor: the user prompt that kicked the turn
	var userUUID, asstUUID string
	for _, rec := range readRecords(t, s.transcriptPath(t, sid)) {
		if rec["type"] == "user" && userUUID == "" {
			userUUID, _ = rec["uuid"].(string)
		}
		if rec["type"] == "assistant" && asstUUID == "" {
			asstUUID, _ = rec["uuid"].(string)
		}
	}
	if userUUID == "" || asstUUID == "" {
		t.Fatal("transcript missing user/assistant uuids")
	}

	// anchor is a user message → rewinds, file back to v1, exits clean
	r = s.run(t, "-r", sid, "--rewind-files", userUUID)
	s.mustOK(t, r)
	contains(t, r.stdout, "Files rewound to state at message", "rewind output")
	contains(t, mustReadFile(t, filepath.Join(s.cwd, "track.txt")), "v1", "file rewound")

	// refusals — non-user anchor, missing --resume, prompt attached
	r = s.run(t, "-r", sid, "--rewind-files", asstUUID)
	contains(t, r.stderr, "not a user message", "assistant anchor refused")
	r = s.run(t, "--rewind-files", userUUID)
	contains(t, r.stderr, "requires --resume", "no-resume refused")
	r = s.run(t, "-r", sid, "--rewind-files", userUUID, "-p", "x")
	contains(t, r.stderr, "standalone operation", "prompt refused")
}

// TestContinue: -c resumes the latest session — session.Latest skips
// transcripts with no conversation records, so the fresh empty .jsonl
// created this session can't shadow the real latest.
func TestContinue(t *testing.T) {
	s := newSession(t)
	sid := newUUID()
	r := s.run(t, "--mock", "-p", "cont-seed", "--session-id", sid)
	s.mustOK(t, r)

	r2 := s.run(t, "--mock", "-c", "-p", "cont-follow")
	s.mustOK(t, r2)
	contains(t, r2.stderr, "resumed session "+sid, "continue notice")
	contains(t, r2.stdout, "Mock response to: cont-follow", "follow-up output")
	// the prior session file is still intact
	contains(t, mustReadFile(t, s.transcriptPath(t, sid)), "cont-seed", "source transcript")
}

// TestForkSession: -r <sid> --fork-session writes a new transcript stamped
// with forkedFrom and carrying the forked chain.
func TestForkSession(t *testing.T) {
	s := newSession(t)
	sid := newUUID()
	r := s.run(t, "--mock", "-p", "fork-seed", "--session-id", sid)
	s.mustOK(t, r)

	r2 := s.run(t, "--mock", "-r", sid, "--fork-session", "-p", "fork-child")
	s.mustOK(t, r2)
	contains(t, r2.stderr, "forked session", "fork notice")

	var forked string
	for _, p := range s.transcriptPaths(t) {
		if filepath.Base(p) != sid+".jsonl" {
			forked = p
		}
	}
	if forked == "" {
		t.Fatalf("no forked transcript; found %v", s.transcriptPaths(t))
	}
	recs := readRecords(t, forked)
	srcCount := 0
	for _, rec := range readRecords(t, s.transcriptPath(t, sid)) {
		if typ, _ := rec["type"].(string); typ == "user" || typ == "assistant" {
			srcCount++
		}
	}
	forkCount := 0
	sawFork := false
	for _, rec := range recs {
		if typ, _ := rec["type"].(string); typ == "user" || typ == "assistant" {
			forkCount++
		}
		if ff, ok := rec["forkedFrom"].(map[string]any); ok && ff["sessionId"] == sid {
			sawFork = true
		}
	}
	if !sawFork {
		t.Fatalf("no forkedFrom{sessionId:%s} record in %s", sid, forked)
	}
	if forkCount < srcCount {
		t.Fatalf("forked transcript has %d messages < source %d", forkCount, srcCount)
	}
	contains(t, mustReadFile(t, forked), "fork-child", "forked transcript")

	// the copied history must be loadable — resuming the fork sees the
	// seed turn (the old marker-only write lost it; audit finding #1)
	forkID := strings.TrimSuffix(filepath.Base(forked), ".jsonl")
	r3 := s.run(t, "--mock", "-r", forkID, "-p", "again")
	s.mustOK(t, r3)
	contains(t, r3.stderr, "resumed session "+forkID, "fork resume")
	var resumed int
	for _, rec := range readRecords(t, forked) {
		if typ, _ := rec["type"].(string); typ == "user" || typ == "assistant" {
			resumed++
		}
	}
	if resumed < srcCount+2 {
		t.Fatalf("resumed fork has %d message records, want >= %d", resumed, srcCount+2)
	}
}

// TestNoSessionPersistence: --no-session-persistence writes nothing.
func TestNoSessionPersistence(t *testing.T) {
	s := newSession(t)
	r := s.run(t, "--mock", "-p", "ephemeral", "--no-session-persistence")
	s.mustOK(t, r)
	if paths := s.transcriptPaths(t); len(paths) != 0 {
		t.Fatalf("expected no transcripts, found %v", paths)
	}
}

func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
