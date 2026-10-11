package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"github.com/gbasin/claude-go/internal/config"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Info describes a saved session for the resume picker.
type Info struct {
	ID       string
	Dir      string // project dir containing <ID>.jsonl
	Path     string // full transcript path
	Slug     string // project slug (e.g. "-Users-x-Code-foo"); display suffix in all-projects mode
	Modified time.Time
	Title    string // custom title from <Dir>/<ID>/custom-title.json, if any
	Size     int64  // transcript bytes — real's meta row shows it
	Branch   string // git branch at session time ("" → HEAD)
	Preview  string
}

// List returns sessions for cwd, newest first.
func List(cwd string) ([]Info, error) {
	return listDir(dir(cwd))
}

// ListAll returns sessions across every project dir under
// ~/.claude/projects, newest first (the picker's all-projects mode).
func ListAll() ([]Info, error) {
	root := filepath.Join(config.ClaudeDir(), "projects")
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []Info
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		infos, err := listDir(filepath.Join(root, e.Name()))
		if err != nil {
			continue
		}
		out = append(out, infos...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	return out, nil
}

func listDir(d string) ([]Info, error) {
	ents, err := os.ReadDir(d)
	if err != nil {
		return nil, err
	}
	var out []Info
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".jsonl")
		p := filepath.Join(d, e.Name())
		// one pass over the transcript for everything the filter needs
		scan := scanInfo(p)
		title := ReadTitle(d, id)
		if title == "" {
			title = scan.aiTitle
		}
		// real's picker needs customTitle‖aiTitle‖messages‖firstPrompt —
		// init-only and bookkeeping-only transcripts never list
		if title == "" && scan.preview == "" && !scan.hasConversation {
			continue
		}
		// model-ended sessions (ended-by-model record) are filtered
		if scan.endedByModel {
			continue
		}
		// continued-in marks this transcript superseded by another
		// session — hidden when the continuation exists
		if scan.continuedIn != "" {
			if _, err := os.Stat(filepath.Join(d, scan.continuedIn+".jsonl")); err == nil {
				continue
			}
		}
		out = append(out, Info{
			ID:       id,
			Dir:      d,
			Path:     p,
			Slug:     filepath.Base(d),
			Modified: fi.ModTime(),
			Size:     fi.Size(),
			Branch:   gitBranchFor(d),
			Title:    title,
			Preview:  scan.preview,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	return out, nil
}

// infoScan is what listDir extracts from one transcript pass.
type infoScan struct {
	preview         string
	aiTitle         string
	hasConversation bool
	endedByModel    bool
	continuedIn     string
}

// scanInfo single-passes a transcript: first visible user text, ai-title
// records, user/assistant presence, ended-by-model, and the tail
// continued-in marker.
func scanInfo(path string) infoScan {
	var out infoScan
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		var e struct {
			Type        string `json:"type"`
			AITitle     string `json:"aiTitle"`
			ContinuedIn string `json:"continuedInSessionId"`
			IsSidechain bool   `json:"isSidechain"`
			IsMeta      bool   `json:"isMeta"`
			Message     struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		switch e.Type {
		case "ai-title":
			if e.AITitle != "" {
				out.aiTitle = e.AITitle
			}
			continue
		case "ended-by-model":
			out.endedByModel = true
			continue
		case "continued-in":
			out.continuedIn = e.ContinuedIn
			continue
		}
		if e.IsSidechain || e.IsMeta {
			continue
		}
		switch e.Message.Role {
		case "user", "assistant":
			out.hasConversation = true
		}
		if out.preview == "" && e.Message.Role == "user" {
			if t := visibleText(e.Message.Content); t != "" {
				out.preview = trunc(t, 80)
			}
		}
	}
	return out
}

// SlugPath renders a project slug back into a path-ish display form
// ("-Users-x-Code-foo" -> "/Users/x/Code/foo"). Ambiguous when real path
// components contain dashes — display only.
func SlugPath(slug string) string {
	return strings.ReplaceAll(slug, "-", "/")
}

// Label returns the picker's display label for a session: the custom title
// when set, else the first-user-message preview.
func (in Info) Label() string {
	if in.Title != "" {
		return in.Title
	}
	return in.Preview
}

func firstUserText(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 4<<20)
	for sc.Scan() {
		var e struct {
			Type    string `json:"type"`
			Message struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Message.Role != "user" {
			continue
		}
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(e.Message.Content, &blocks) != nil {
			var s string
			if json.Unmarshal(e.Message.Content, &s) == nil {
				return trunc(s, 80)
			}
			continue
		}
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" && !hiddenText(b.Text) {
				return trunc(b.Text, 80)
			}
		}
	}
	return ""
}

// hiddenText reports text that should never be shown as a session preview
// (system reminders, bash command wrappers).
func hiddenText(t string) bool {
	return strings.HasPrefix(t, "<system-reminder>") || strings.HasPrefix(t, "<bash-")
}

// PreviewBlock returns up to maxLines lines previewing a transcript for the
// picker's preview pane: the first user message text, then the last
// assistant text, like CC's resume preview.
func PreviewBlock(path string, maxLines int) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var firstUser, lastAsst string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		var e struct {
			Type        string `json:"type"`
			IsSidechain bool   `json:"isSidechain"`
			IsMeta      bool   `json:"isMeta"`
			Message     struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.IsSidechain || e.IsMeta {
			continue
		}
		switch e.Message.Role {
		case "user":
			if firstUser == "" {
				if t := visibleText(e.Message.Content); t != "" {
					firstUser = t
				}
			}
		case "assistant":
			if t := visibleText(e.Message.Content); t != "" {
				lastAsst = t
			}
		}
	}
	var lines []string
	if firstUser != "" {
		for i, l := range strings.Split(firstUser, "\n") {
			if i == 0 {
				lines = append(lines, "❯ "+l)
			} else {
				lines = append(lines, "  "+l)
			}
		}
	}
	if lastAsst != "" {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, strings.Split(lastAsst, "\n")...)
	}
	if len(lines) > maxLines && maxLines > 0 {
		lines = append(lines[:maxLines-1], "…")
	}
	return lines
}

// visibleText extracts displayable message text (string content or text
// blocks), skipping hidden wrapper text for user messages.
func visibleText(content json.RawMessage) string {
	var s string
	if json.Unmarshal(content, &s) == nil {
		if hiddenText(s) {
			return ""
		}
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" && !hiddenText(b.Text) {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func trunc(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// Print lists sessions in plain form (for non-TTY).
func Print(cwd string) {
	infos, err := List(cwd)
	if err != nil || len(infos) == 0 {
		fmt.Println("  no sessions found for this directory")
		return
	}
	for i, in := range infos {
		if i >= 20 {
			break
		}
		id := in.ID
		if len(id) > 8 {
			id = id[:8]
		}
		fmt.Printf("  %s  %s  %s\n", in.Modified.Format("2006-01-02 15:04"), id, in.Label())
	}
}

// gitBranchFor returns the project dir's current branch (session files
// store none — the picker meta shows the checked-out one).
func gitBranchFor(dir string) string {
	out, err := exec.Command("git", "-C", dir, "branch", "--show-current").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
