package tools

import (
	"encoding/json"
	"fmt"
	"github.com/gbasin/claude-go/internal/api"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	_ "unsafe"
)

// Glob / Grep / LS / MultiEdit / NotebookRead — real's deferred file tools.
// They're absent from the wire until ToolSearch serves their schema.

func init() {
	Register(&Tool{Def: toolDefFrom(schemaGlob), Exec: execGlob})
	Register(&Tool{Def: toolDefFrom(schemaGrep), Exec: execGrep})
	Register(&Tool{Def: toolDefFrom(schemaLS), Exec: execLS})
	Register(&Tool{Def: toolDefFrom(schemaMultiEdit), Exec: execMultiEdit})
	Register(&Tool{Def: toolDefFrom(schemaNotebookRead), Exec: execNotebookRead})
}

const schemaGlob = `{"name":"Glob","description":"Fast file pattern matching tool that works with any codebase size\n\n- Supports glob patterns like \"**/*.js\" or \"src/**/*.ts\"\n- Returns matching file paths sorted by modification time\n- Use this tool when you need to find files by name patterns\n- When you are doing an open ended search that may require multiple rounds of globbing and grepping, use the Agent tool instead (if available)","input_schema":{"type":"object","properties":{"pattern":{"type":"string","description":"The glob pattern to match files against"},"path":{"type":"string","description":"The directory to search in. If not specified, the current working directory will be used. IMPORTANT: Omit this field to use the default directory."}},"required":["pattern"]}}`

const schemaGrep = `{"name":"Grep","description":"A powerful search tool built on ripgrep\n\n  Usage:\n  - ALWAYS use Grep for search tasks. NEVER invoke ` + "`grep`" + ` or ` + "`rg`" + ` as a Bash command. The Grep tool has been optimized for correct permissions and access.\n  - Supports full regex syntax (e.g., \"log.*Error\", \"function\\s+\\w+\")\n  - Filter files with glob parameter (e.g., \"*.js\", \"**/*.tsx\") or type parameter (e.g., \"js\", \"py\", \"rust\")\n  - Output modes: \"content\" shows matching lines, \"files_with_matches\" shows only file paths (default), \"count\" shows match counts\n  - Pattern syntax: Uses ripgrep (not grep) - literal braces need escaping (use ` + "`interface\\{\\}`" + ` to find ` + "`interface{}`" + ` in Go code)\n  - Multiline matching: By default patterns match within single lines only. For cross-line patterns like ` + "`struct \\{[\\s\\S]*?field`" + `, use ` + "`multiline: true`" + `\n","input_schema":{"type":"object","properties":{"pattern":{"type":"string","description":"The regular expression pattern to search for"},"path":{"type":"string","description":"File or directory to search in (rg PATH). Defaults to current working directory."},"glob":{"type":"string","description":"Glob pattern to filter files (e.g. \"*.js\", \"*.{ts,tsx}\")"},"type":{"type":"string","description":"File type to search for (rg --type)"},"output_mode":{"type":"string","description":"Output mode: \"content\" shows matching lines, \"files_with_matches\" shows file paths (default), \"count\" shows match counts","enum":["content","files_with_matches","count"]},"-i":{"type":"boolean","description":"Case insensitive search"},"-n":{"type":"boolean","description":"Show line numbers in output (requires output_mode: content)"},"-A":{"type":"number","description":"Number of lines to show after each match"},"-B":{"type":"number","description":"Number of lines to show before each match"},"-C":{"type":"number","description":"Number of lines to show before and after each match"},"-e":{"type":"boolean","description":"Enable extended regex pattern syntax"},"head_limit":{"type":"number","description":"Limit output to first N matching lines (for output_mode: content)"},"offset":{"type":"number","description":"Skip first N matches when paginating"},"multiline":{"type":"boolean","description":"Enable multiline mode where the pattern can match across lines"},"context":{"type":"number","description":"Alias for -C"}},"required":["pattern"]}}`

const schemaLS = `{"name":"LS","description":"Lists files and directories in a given path. The path parameter must be an absolute path, not a relative path. You can optionally provide an array of glob patterns to ignore with the ignore parameter.","input_schema":{"type":"object","properties":{"path":{"type":"string","description":"The absolute path to the directory to list (must be absolute, not relative)"},"ignore":{"type":"array","description":"List of glob patterns to ignore","items":{"type":"string"}}},"required":["path"]}}`

const schemaMultiEdit = `{"name":"MultiEdit","description":"This is a tool for making multiple edits to a single file in one operation. It is built on top of the Edit tool and allows you to perform multiple find-and-replace operations efficiently. Prefer this tool over the Edit tool when you need to make multiple edits to the same file.","input_schema":{"type":"object","properties":{"file_path":{"type":"string","description":"The absolute path to the file to modify"},"edits":{"type":"array","items":{"type":"object","properties":{"old_string":{"type":"string","description":"The text to replace"},"new_string":{"type":"string","description":"The text to replace it with"},"replace_all":{"type":"boolean","description":"Replace all occurrences of old_string (default false)"}},"required":["old_string","new_string"]},"description":"Array of edit operations to perform sequentially on the file"}},"required":["file_path","edits"]}}`

const schemaNotebookRead = `{"name":"NotebookRead","description":"Reads a Jupyter notebook (.ipynb file) and returns all of the cells with their outputs.","input_schema":{"type":"object","properties":{"notebook_path":{"type":"string","description":"The absolute path to the Jupyter notebook file to read (must be absolute, not relative)"},"cell_id":{"type":"string","description":"The ID of a specific cell to read. If not provided, all cells will be read."}},"required":["notebook_path"]}}`

// --- Glob ---

type globIn struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
}

func execGlob(c *Context, raw json.RawMessage) Result {
	var in globIn
	if json.Unmarshal(raw, &in) != nil || in.Pattern == "" {
		return Result{Content: "pattern is required", IsError: true}
	}
	dir := in.Path
	if dir == "" {
		dir = c.CWD
	}
	var matches []string
	// real sorts by mtime desc — walk and collect
	type fm struct {
		p string
		t time.Time
	}
	var found []fm
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			if d != nil && strings.HasPrefix(d.Name(), ".git") {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return nil
		}
		if globMatch(in.Pattern, rel) || globMatch(in.Pattern, filepath.Base(p)) {
			if fi, err := d.Info(); err == nil {
				found = append(found, fm{p, fi.ModTime()})
			}
		}
		return nil
	})
	sort.Slice(found, func(i, j int) bool { return found[i].t.After(found[j].t) })
	for _, f := range found {
		matches = append(matches, f.p)
	}
	if len(matches) == 0 {
		return Result{Content: "no files matched"}
	}
	return Result{Content: strings.Join(matches, "\n")}
}

// globMatch handles ** and * patterns (path/filepath.Match lacks **).
func globMatch(pat, name string) bool {
	// normalize
	pat = filepath.ToSlash(pat)
	name = filepath.ToSlash(name)
	if pp := strings.Split(pat, "**"); len(pp) > 1 {
		// ** crosses directories
		idx := 0
		rest := name
		for i, part := range pp {
			part = strings.Trim(part, "/")
			if part == "" {
				continue
			}
			ok, sub := matchGlobPrefix(part, rest)
			if i == len(pp)-1 {
				// last part must match the tail
				for {
					if ok {
						return true
					}
					slash := strings.Index(rest, "/")
					if slash < 0 {
						return false
					}
					rest = rest[slash+1:]
					ok, _ = matchGlobPrefix(part, rest)
				}
			}
			if !ok {
				return false
			}
			_ = idx
			rest = sub
		}
		return true
	}
	ok, _ := filepath.Match(pat, name)
	return ok
}

func matchGlobPrefix(pat, s string) (bool, string) {
	// match pat against a prefix segment boundary of s
	ok, _ := filepath.Match(pat, s)
	if ok {
		return true, ""
	}
	// try matching pat as a leading path component
	parts := strings.SplitN(s, "/", 2)
	if ok2, _ := filepath.Match(pat, parts[0]); ok2 && len(parts) == 2 {
		return true, parts[1]
	}
	return false, s
}

// --- Grep ---

type grepIn struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	Glob       string `json:"glob"`
	Type       string `json:"type"`
	OutputMode string `json:"output_mode"`
	CaseI      bool   `json:"-i"`
	LineNo     bool   `json:"-n"`
	After      int    `json:"-A"`
	Before     int    `json:"-B"`
	Context    int    `json:"-C"`
	ContextAlt int    `json:"context"`
	HeadLimit  int    `json:"head_limit"`
	Offset     int    `json:"offset"`
	Multiline  bool   `json:"multiline"`
}

func execGrep(c *Context, raw json.RawMessage) Result {
	var in grepIn
	if json.Unmarshal(raw, &in) != nil || in.Pattern == "" {
		return Result{Content: "pattern is required", IsError: true}
	}
	path := in.Path
	if path == "" {
		path = c.CWD
	}
	args := []string{"--no-heading", "--color=never"}
	if in.CaseI {
		args = append(args, "-i")
	}
	if in.LineNo || in.OutputMode == "content" {
		args = append(args, "-n")
	}
	if in.After > 0 {
		args = append(args, "-A", fmt.Sprint(in.After))
	}
	if in.Before > 0 {
		args = append(args, "-B", fmt.Sprint(in.Before))
	}
	if cc := in.Context + in.ContextAlt; cc > 0 {
		args = append(args, "-C", fmt.Sprint(cc))
	}
	if in.Glob != "" {
		args = append(args, "-g", in.Glob)
	}
	if in.Type != "" {
		args = append(args, "-t", in.Type)
	}
	if in.Multiline {
		args = append(args, "-U", "--multiline-dotall")
	}
	switch in.OutputMode {
	case "files_with_matches", "":
		args = append(args, "-l")
	case "count":
		args = append(args, "-c")
	}
	if in.HeadLimit > 0 {
		args = append(args, "-m", fmt.Sprint(in.HeadLimit))
	}
	args = append(args, "--", in.Pattern, path)
	out, err := exec.Command("rg", args...).CombinedOutput()
	if err != nil {
		if len(out) == 0 {
			return Result{Content: "no matches"}
		}
		// rg exits 1 on no match; other errors surface the message
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return Result{Content: "no matches"}
		}
		return Result{Content: strings.TrimSpace(string(out)), IsError: true}
	}
	if in.Offset > 0 {
		lines := strings.Split(string(out), "\n")
		if in.Offset < len(lines) {
			lines = lines[in.Offset:]
		}
		out = []byte(strings.Join(lines, "\n"))
	}
	return Result{Content: strings.TrimRight(string(out), "\n")}
}

// --- LS ---

type lsIn struct {
	Path   string   `json:"path"`
	Ignore []string `json:"ignore"`
}

func execLS(c *Context, raw json.RawMessage) Result {
	var in lsIn
	if json.Unmarshal(raw, &in) != nil || in.Path == "" {
		return Result{Content: "path is required", IsError: true}
	}
	if !filepath.IsAbs(in.Path) {
		return Result{Content: "path must be absolute", IsError: true}
	}
	ents, err := os.ReadDir(in.Path)
	if err != nil {
		return Result{Content: err.Error(), IsError: true}
	}
	var out []string
	for _, e := range ents {
		skip := false
		for _, ig := range in.Ignore {
			if ok, _ := filepath.Match(ig, e.Name()); ok {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		n := e.Name()
		if e.IsDir() {
			n += "/"
		}
		out = append(out, n)
	}
	return Result{Content: strings.Join(out, "\n")}
}

// --- MultiEdit ---

type multiEditIn struct {
	FilePath string `json:"file_path"`
	Edits    []struct {
		Old        string `json:"old_string"`
		New        string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	} `json:"edits"`
}

func execMultiEdit(c *Context, raw json.RawMessage) Result {
	var in multiEditIn
	if json.Unmarshal(raw, &in) != nil || in.FilePath == "" || len(in.Edits) == 0 {
		return Result{Content: "file_path and edits are required", IsError: true}
	}
	p := in.FilePath
	if !filepath.IsAbs(p) {
		p = filepath.Join(c.CWD, p)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return Result{Content: err.Error(), IsError: true}
	}
	content := string(b)
	applied := 0
	for i, e := range in.Edits {
		if e.Old == e.New {
			return Result{Content: fmt.Sprintf("edit %d: old_string and new_string are identical", i+1), IsError: true}
		}
		if e.ReplaceAll {
			if !strings.Contains(content, e.Old) {
				return Result{Content: fmt.Sprintf("edit %d: old_string not found", i+1), IsError: true}
			}
			content = strings.ReplaceAll(content, e.Old, e.New)
			applied++
			continue
		}
		n := strings.Count(content, e.Old)
		if n == 0 {
			return Result{Content: fmt.Sprintf("edit %d: old_string not found", i+1), IsError: true}
		}
		if n > 1 {
			return Result{Content: fmt.Sprintf("edit %d: old_string found %d times — use replace_all or more context", i+1, n), IsError: true}
		}
		content = strings.Replace(content, e.Old, e.New, 1)
		applied++
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		return Result{Content: err.Error(), IsError: true}
	}
	return Result{Content: fmt.Sprintf("Applied %d edits to %s", applied, p)}
}

// --- NotebookRead ---

func execNotebookRead(c *Context, raw json.RawMessage) Result {
	var in struct {
		Path   string `json:"notebook_path"`
		CellID string `json:"cell_id"`
	}
	if json.Unmarshal(raw, &in) != nil || in.Path == "" {
		return Result{Content: "notebook_path is required", IsError: true}
	}
	p := in.Path
	if !filepath.IsAbs(p) {
		p = filepath.Join(c.CWD, p)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return Result{Content: err.Error(), IsError: true}
	}
	var nb struct {
		Cells []struct {
			ID       string   `json:"id"`
			CellType string   `json:"cell_type"`
			Source   []string `json:"source"`
			Outputs  []struct {
				Text []string `json:"text"`
			} `json:"outputs"`
		} `json:"cells"`
	}
	if json.Unmarshal(b, &nb) != nil {
		return Result{Content: "not a valid notebook", IsError: true}
	}
	var sb strings.Builder
	for _, cell := range nb.Cells {
		if in.CellID != "" && cell.ID != in.CellID {
			continue
		}
		sb.WriteString(fmt.Sprintf("[%s] %s\n", cell.ID, cell.CellType))
		for _, ln := range cell.Source {
			sb.WriteString("  " + ln)
		}
		for _, o := range cell.Outputs {
			for _, ln := range o.Text {
				sb.WriteString("  out: " + ln)
			}
		}
		sb.WriteString("\n")
	}
	return Result{Content: sb.String()}
}

func toolDefFrom(raw string) api.Tool {
	var t api.Tool
	json.Unmarshal([]byte(raw), &t)
	return t
}
