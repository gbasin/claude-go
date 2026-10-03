package tools

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func init() {
	Register(&Tool{Def: loadSchema("Read"), Exec: execRead})
	Register(&Tool{Def: loadSchema("Write"), Exec: execWrite})
	Register(&Tool{Def: loadSchema("Edit"), Exec: execEdit})
	Register(&Tool{Def: loadSchema("NotebookEdit"), Exec: execNotebookEdit})
}

const (
	readDefaultLimit = 2000
	readMaxLineLen   = 2000
)

// ---------- Read ----------

type readInput struct {
	FilePath string `json:"file_path"`
	Offset   int    `json:"offset"`
	Limit    int    `json:"limit"`
	Pages    string `json:"pages"`
}

func execRead(c *Context, raw json.RawMessage) Result {
	var in readInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{Content: "invalid input: " + err.Error(), IsError: true}
	}
	if !filepath.IsAbs(in.FilePath) {
		return Result{Content: "file_path must be an absolute path", IsError: true}
	}
	fi, err := os.Stat(in.FilePath)
	if err != nil {
		return Result{Content: "Error: " + err.Error(), IsError: true}
	}
	if fi.IsDir() {
		return Result{Content: "Error: is a directory: " + in.FilePath, IsError: true}
	}

	ext := strings.ToLower(filepath.Ext(in.FilePath))
	switch ext {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		return readImage(in.FilePath)
	case ".ipynb":
		return readNotebook(c, in.FilePath)
	case ".pdf":
		return Result{Content: "PDF support requires the pages parameter; this build returns raw bytes only. Use Bash `pdftotext` if available.", IsError: true}
	}

	f, err := os.Open(in.FilePath)
	if err != nil {
		return Result{Content: "Error: " + err.Error(), IsError: true}
	}
	defer f.Close()

	limit := in.Limit
	if limit <= 0 {
		limit = readDefaultLimit
	}
	offset := in.Offset
	if offset < 0 {
		offset = 0
	}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var b strings.Builder
	lineNo := 0
	emitted := 0
	for sc.Scan() {
		lineNo++
		if lineNo < offset {
			continue
		}
		if emitted >= limit {
			break
		}
		line := sc.Text()
		if len(line) > readMaxLineLen {
			line = line[:readMaxLineLen] + "…"
		}
		fmt.Fprintf(&b, "%6d\t%s\n", lineNo, line)
		emitted++
	}
	if err := sc.Err(); err != nil {
		return Result{Content: "Error: " + err.Error(), IsError: true}
	}
	if emitted == 0 {
		if lineNo == 0 {
			return Result{Content: "File is empty: " + in.FilePath}
		}
		return Result{Content: fmt.Sprintf("Offset %d is beyond end of file (%d lines)", offset, lineNo), IsError: true}
	}
	st, _ := f.Stat()
	if st != nil {
		c.MarkRead(in.FilePath, st.ModTime().UnixNano(), st.Size())
	}
	return Result{Content: strings.TrimRight(b.String(), "\n")}
}

func readImage(path string) Result {
	b, err := os.ReadFile(path)
	if err != nil {
		return Result{Content: "Error: " + err.Error(), IsError: true}
	}
	// Return a marker the agent loop converts into an image content block.
	return Result{Content: fmt.Sprintf("\x00IMAGE\x00%s\x00%d", path, len(b))}
}

type nbCell struct {
	CellType  string `json:"cell_type"`
	Source    any    `json:"source"`
	Outputs   any    `json:"outputs,omitempty"`
	Metadata  any    `json:"metadata,omitempty"`
	ExecCount any    `json:"execution_count,omitempty"`
	ID        string `json:"id,omitempty"`
}

type nbFile struct {
	Cells []nbCell `json:"cells"`
}

func readNotebook(c *Context, path string) Result {
	b, err := os.ReadFile(path)
	if err != nil {
		return Result{Content: "Error: " + err.Error(), IsError: true}
	}
	var nb nbFile
	if err := json.Unmarshal(b, &nb); err != nil {
		return Result{Content: "Error: invalid notebook JSON: " + err.Error(), IsError: true}
	}
	var sb strings.Builder
	for i, cell := range nb.Cells {
		id := cell.ID
		if id == "" {
			id = fmt.Sprintf("cell-%d", i)
		}
		fmt.Fprintf(&sb, "<cell id=%q type=%q>\n%s\n", id, cell.CellType, nbSource(cell.Source))
		if cell.Outputs != nil {
			ob, _ := json.Marshal(cell.Outputs)
			fmt.Fprintf(&sb, "<outputs>\n%s\n</outputs>\n", string(ob))
		}
		fmt.Fprintf(&sb, "</cell>\n")
	}
	st, _ := os.Stat(path)
	if st != nil {
		c.MarkRead(path, st.ModTime().UnixNano(), st.Size())
	}
	return Result{Content: sb.String()}
}

func nbSource(src any) string {
	switch v := src.(type) {
	case string:
		return v
	case []any:
		var sb strings.Builder
		for _, l := range v {
			if s, ok := l.(string); ok {
				sb.WriteString(s)
			}
		}
		return sb.String()
	}
	return ""
}

// ---------- Write ----------

type writeInput struct {
	FilePath string `json:"file_path"`
	Content  string `json:"content"`
}

func execWrite(c *Context, raw json.RawMessage) Result {
	defer func() {}()
	Checkpoints.Snapshot(inFilePath(c, raw))
	var in writeInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{Content: "invalid input: " + err.Error(), IsError: true}
	}
	if !filepath.IsAbs(in.FilePath) {
		return Result{Content: "file_path must be an absolute path", IsError: true}
	}
	if _, err := os.Stat(in.FilePath); err == nil {
		if _, ok := c.WasRead(in.FilePath); !ok {
			return Result{Content: "Error: file exists and has not been Read in this conversation. Read it first.", IsError: true}
		}
	}
	if dir := filepath.Dir(in.FilePath); dir != "" {
		os.MkdirAll(dir, 0o755)
	}
	if err := os.WriteFile(in.FilePath, []byte(in.Content), 0o644); err != nil {
		return Result{Content: "Error: " + err.Error(), IsError: true}
	}
	st, _ := os.Stat(in.FilePath)
	if st != nil {
		c.MarkRead(in.FilePath, st.ModTime().UnixNano(), st.Size())
	}
	return Result{Content: "File written successfully"}
}

// ---------- Edit ----------

type editInput struct {
	FilePath   string `json:"file_path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

func execEdit(c *Context, raw json.RawMessage) Result {
	Checkpoints.Snapshot(inFilePath(c, raw))
	var in editInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{Content: "invalid input: " + err.Error(), IsError: true}
	}
	if !filepath.IsAbs(in.FilePath) {
		return Result{Content: "file_path must be an absolute path", IsError: true}
	}
	if _, ok := c.WasRead(in.FilePath); !ok {
		return Result{Content: "Error: file has not been Read in this conversation. Read it first.", IsError: true}
	}
	stale := c.Stale(in.FilePath)
	if in.OldString == in.NewString {
		return Result{Content: "Error: new_string must be different from old_string", IsError: true}
	}
	b, err := os.ReadFile(in.FilePath)
	if err != nil {
		return Result{Content: "Error: " + err.Error(), IsError: true}
	}
	content := string(b)
	n := strings.Count(content, in.OldString)
	if n == 0 {
		return Result{Content: "Error: old_string not found in file", IsError: true}
	}
	if n > 1 && !in.ReplaceAll {
		return Result{Content: fmt.Sprintf("Error: old_string occurs %d times; use replace_all or add context for uniqueness", n), IsError: true}
	}
	if in.ReplaceAll {
		content = strings.ReplaceAll(content, in.OldString, in.NewString)
	} else {
		content = strings.Replace(content, in.OldString, in.NewString, 1)
	}
	if err := os.WriteFile(in.FilePath, []byte(content), 0o644); err != nil {
		return Result{Content: "Error: " + err.Error(), IsError: true}
	}
	st, _ := os.Stat(in.FilePath)
	if st != nil {
		c.MarkRead(in.FilePath, st.ModTime().UnixNano(), st.Size())
	}
	msg := "The file " + in.FilePath + " has been updated successfully."
	if stale {
		msg += " (note: the file had been modified on disk since you last read it — the edit applied cleanly, but the file contains other changes not in your context. You may want to Read it again.)"
	}
	if in.ReplaceAll {
		return Result{Content: "All occurrences were successfully replaced." + staleSuffix(stale)}
	}
	return Result{Content: msg}
}

func staleSuffix(stale bool) string {
	if !stale {
		return ""
	}
	return " (note: the file had been modified on disk since you last read it.)"
}

// ---------- NotebookEdit ----------

type nbEditInput struct {
	NotebookPath string `json:"notebook_path"`
	CellID       string `json:"cell_id"`
	NewSource    string `json:"new_source"`
	CellType     string `json:"cell_type"`
	EditMode     string `json:"edit_mode"` // replace|insert|delete
}

func execNotebookEdit(c *Context, raw json.RawMessage) Result {
	var in nbEditInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{Content: "invalid input: " + err.Error(), IsError: true}
	}
	b, err := os.ReadFile(in.NotebookPath)
	if err != nil {
		return Result{Content: "Error: " + err.Error(), IsError: true}
	}
	var nb map[string]any
	if err := json.Unmarshal(b, &nb); err != nil {
		return Result{Content: "Error: invalid notebook: " + err.Error(), IsError: true}
	}
	if _, ok := c.WasRead(in.NotebookPath); !ok {
		return Result{Content: "Error: notebook has not been Read in this conversation. Read it first.", IsError: true}
	}
	cells, _ := nb["cells"].([]any)
	mode := in.EditMode
	if mode == "" {
		mode = "replace"
	}
	idx := -1
	for i, cv := range cells {
		if m, ok := cv.(map[string]any); ok && m["id"] == in.CellID {
			idx = i
			break
		}
	}
	switch mode {
	case "replace":
		if idx < 0 {
			return Result{Content: "cell not found: " + in.CellID, IsError: true}
		}
		m := cells[idx].(map[string]any)
		m["source"] = in.NewSource
		if in.CellType != "" {
			m["cell_type"] = in.CellType
		}
	case "insert":
		if in.CellType == "" {
			return Result{Content: "Error: cell_type is required when inserting a cell", IsError: true}
		}
		newCell := map[string]any{"cell_type": in.CellType, "source": in.NewSource, "metadata": map[string]any{}}
		at := 0 // omitted cell_id inserts at the beginning (real semantics)
		if in.CellID != "" {
			if idx < 0 {
				return Result{Content: "cell not found: " + in.CellID, IsError: true}
			}
			at = idx + 1
		}
		cells = append(cells[:at], append([]any{newCell}, cells[at:]...)...)
	case "delete":
		if idx < 0 {
			return Result{Content: "cell not found: " + in.CellID, IsError: true}
		}
		cells = append(cells[:idx], cells[idx+1:]...)
	}
	nb["cells"] = cells
	out, _ := json.MarshalIndent(nb, "", " ")
	if err := os.WriteFile(in.NotebookPath, out, 0o644); err != nil {
		return Result{Content: "Error: " + err.Error(), IsError: true}
	}
	if st, err := os.Stat(in.NotebookPath); err == nil {
		c.MarkRead(in.NotebookPath, st.ModTime().UnixNano(), st.Size())
	}
	return Result{Content: "Notebook updated"}
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

var _ = bytes.MinRead

func inFilePath(c *Context, raw json.RawMessage) string {
	var in struct {
		FilePath string `json:"file_path"`
	}
	json.Unmarshal(raw, &in)
	if !filepath.IsAbs(in.FilePath) {
		in.FilePath = filepath.Join(c.CWD, in.FilePath)
	}
	return in.FilePath
}

// fileSHA returns the sha256 hex of a file or "".
func fileSHA(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
