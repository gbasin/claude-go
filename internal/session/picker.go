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
	Modified time.Time
	Preview  string
}

// List returns sessions for cwd, newest first.
func List(cwd string) ([]Info, error) {
	ents, err := os.ReadDir(dir(cwd))
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
		out = append(out, Info{
			ID:       strings.TrimSuffix(e.Name(), ".jsonl"),
			Modified: fi.ModTime(),
			Preview:  firstUserText(filepath.Join(dir(cwd), e.Name())),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	return out, nil
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
			if b.Type == "text" && b.Text != "" && !strings.HasPrefix(b.Text, "<system-reminder>") && !strings.HasPrefix(b.Text, "<bash-") {
				return trunc(b.Text, 80)
			}
		}
	}
	return ""
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
		fmt.Printf("  %s  %s  %s\n", in.Modified.Format("2006-01-02 15:04"), in.ID[:8], in.Preview)
	}
}
