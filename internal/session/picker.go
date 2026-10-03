package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
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
	Preview  string
}

// List returns sessions for cwd, newest first.
func List(cwd string) ([]Info, error) {
	return listDir(dir(cwd))
}

// ListAll returns sessions across every project dir under
// ~/.claude/projects, newest first (the picker's all-projects mode).
func ListAll() ([]Info, error) {
	home, _ := os.UserHomeDir()
	root := filepath.Join(home, ".claude", "projects")
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
		out = append(out, Info{
			ID:       id,
			Dir:      d,
			Path:     p,
			Slug:     filepath.Base(d),
			Modified: fi.ModTime(),
			Title:    ReadTitle(d, id),
			Preview:  firstUserText(p),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	return out, nil
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
