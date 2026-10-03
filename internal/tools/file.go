package tools

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
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

// emittedReads tracks paths whose content was actually emitted by a Read.
// Write/Edit/NotebookEdit re-mark file state without a read, so the
// file_unchanged shortcut must only fire when the last mark came from Read.
var emittedReads = struct {
	sync.Mutex
	paths map[string]bool
}{paths: map[string]bool{}}

func markEmitted(path string) {
	emittedReads.Lock()
	emittedReads.paths[path] = true
	emittedReads.Unlock()
}

func clearEmitted(path string) {
	emittedReads.Lock()
	delete(emittedReads.paths, path)
	emittedReads.Unlock()
}

func wasEmitted(path string) bool {
	emittedReads.Lock()
	defer emittedReads.Unlock()
	return emittedReads.paths[path]
}

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

	// file_unchanged: a plain re-read of a file whose state matches the last
	// read returns a compact marker instead of re-emitting the full content.
	if in.Offset == 0 && in.Limit == 0 && in.Pages == "" {
		if _, ok := c.WasRead(in.FilePath); ok && wasEmitted(in.FilePath) && !c.Stale(in.FilePath) {
			return Result{Content: "File unchanged since last read"}
		}
	}

	ext := strings.ToLower(filepath.Ext(in.FilePath))
	switch ext {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".heic", ".bmp", ".tiff", ".tif":
		return readImage(c, in.FilePath)
	case ".ipynb":
		return readNotebook(c, in.FilePath)
	case ".pdf":
		return readPDF(c, in.FilePath, in.Pages)
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
	markEmitted(in.FilePath)
	return Result{Content: strings.TrimRight(b.String(), "\n")}
}

const (
	readMaxImageBytes = 10 << 20 // real refuses images over ~10 MB
	imageMaxDim       = 2000     // downscale images larger than this
)

// apiImageExts are the formats the API accepts directly; anything else
// (heic/bmp/tiff) is transcoded to PNG when a local converter is available.
var apiImageExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true,
}

// readImage returns the \x00IMAGE\x00 marker the agent loop converts into an
// image content block. Oversized files error; >2000px images are downscaled
// and non-API formats converted to PNG via sips (darwin) or ImageMagick.
func readImage(c *Context, path string) Result {
	fi, err := os.Stat(path)
	if err != nil {
		return Result{Content: "Error: " + err.Error(), IsError: true}
	}
	if fi.Size() > readMaxImageBytes {
		return Result{Content: fmt.Sprintf("Error: image file is %.1f MB, exceeding the %d MB limit; downscale or crop it first", float64(fi.Size())/(1<<20), readMaxImageBytes>>20), IsError: true}
	}
	out := path
	if p, ok := normalizeImage(path); ok {
		out = p
	}
	st, err := os.Stat(out)
	if err != nil {
		return Result{Content: "Error: " + err.Error(), IsError: true}
	}
	c.MarkRead(path, fi.ModTime().UnixNano(), fi.Size())
	markEmitted(path)
	return Result{Content: fmt.Sprintf("\x00IMAGE\x00%s\x00%d", out, st.Size())}
}

// normalizeImage produces a rewritten image file (downscaled to ≤2000px and/or
// transcoded to PNG) in the temp dir and returns its path. ok=false means the
// original file is fine to send or no conversion tool is available.
func normalizeImage(path string) (string, bool) {
	ext := strings.ToLower(filepath.Ext(path))
	transcode := !apiImageExts[ext]
	big := false
	if w, h := imageDims(path); w > imageMaxDim || h > imageMaxDim {
		big = true
	}
	if !big && !transcode {
		return "", false
	}
	outExt := ext
	if transcode {
		outExt = ".png"
	}
	tmp, err := os.CreateTemp("", "claude-go-img-*"+outExt)
	if err != nil {
		return "", false
	}
	dst := tmp.Name()
	tmp.Close()
	// sips (macOS) can both resample and convert formats.
	if sips, err := exec.LookPath("sips"); err == nil {
		var args []string
		if transcode {
			args = append(args, "-s", "format", "png")
		}
		if big {
			args = append(args, "-Z", strconv.Itoa(imageMaxDim))
		}
		args = append(args, path, "--out", dst)
		if exec.Command(sips, args...).Run() == nil {
			return dst, true
		}
	}
	// ImageMagick: "magick" (v7) or "convert" (v6); > only shrinks.
	for _, name := range []string{"magick", "convert"} {
		bin, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		args := []string{path}
		if big {
			args = append(args, "-resize", fmt.Sprintf("%dx%d>", imageMaxDim, imageMaxDim))
		}
		args = append(args, dst)
		if exec.Command(bin, args...).Run() == nil {
			return dst, true
		}
	}
	os.Remove(dst)
	return "", false
}

// imageDims returns pixel dimensions, or 0,0 when no probe tool exists.
func imageDims(path string) (w, h int) {
	if sips, err := exec.LookPath("sips"); err == nil {
		if b, err := exec.Command(sips, "-g", "pixelWidth", "-g", "pixelHeight", path).Output(); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				line = strings.TrimSpace(line)
				if v, ok := strings.CutPrefix(line, "pixelWidth:"); ok {
					w, _ = strconv.Atoi(strings.TrimSpace(v))
				}
				if v, ok := strings.CutPrefix(line, "pixelHeight:"); ok {
					h, _ = strconv.Atoi(strings.TrimSpace(v))
				}
			}
			return w, h
		}
	}
	for _, name := range []string{"identify", "magick"} {
		bin, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		var args []string
		if name == "magick" {
			args = append(args, "identify")
		}
		args = append(args, "-format", "%w %h", path)
		if b, err := exec.Command(bin, args...).Output(); err == nil {
			fmt.Sscanf(string(b), "%d %d", &w, &h)
			return w, h
		}
	}
	return 0, 0
}

// ---------- PDF ----------

const (
	pdfMaxPagesPerRequest = 20 // real caps `pages` at 20 pages/request
	pdfRequirePagesOver   = 10 // pages param required for PDFs over this
)

var pagesRe = regexp.MustCompile(`^(\d+)(?:-(\d+))?$`)

func parsePages(s string) (first, last int, err error) {
	m := pagesRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, 0, fmt.Errorf("invalid pages range %q (expected e.g. \"1-5\" or \"3\")", s)
	}
	first, _ = strconv.Atoi(m[1])
	if m[2] == "" {
		last = first
	} else {
		last, _ = strconv.Atoi(m[2])
	}
	if first < 1 || last < first {
		return 0, 0, fmt.Errorf("invalid pages range %q", s)
	}
	return first, last, nil
}

// pdfPageCount returns the page count via pdfinfo, or 0 when unknown.
func pdfPageCount(path string) int {
	bin, err := exec.LookPath("pdfinfo")
	if err != nil {
		return 0
	}
	b, err := exec.Command(bin, path).Output()
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "Pages:"); ok {
			n, _ := strconv.Atoi(strings.TrimSpace(v))
			return n
		}
	}
	return 0
}

func readPDF(c *Context, path, pages string) Result {
	bin, err := exec.LookPath("pdftotext")
	if err != nil {
		return Result{Content: "PDF reading requires pdftotext (brew install poppler)", IsError: true}
	}
	total := pdfPageCount(path)
	first, last := 0, 0
	if pages == "" {
		if total > pdfRequirePagesOver {
			return Result{Content: fmt.Sprintf("This PDF has %d pages. The pages parameter is required for PDFs over %d pages (e.g. \"1-%d\").", total, pdfRequirePagesOver, pdfRequirePagesOver), IsError: true}
		}
	} else {
		if first, last, err = parsePages(pages); err != nil {
			return Result{Content: "Error: " + err.Error(), IsError: true}
		}
		if n := last - first + 1; n > pdfMaxPagesPerRequest {
			return Result{Content: fmt.Sprintf("Error: page range %q spans %d pages; maximum %d pages per request.", pages, n, pdfMaxPagesPerRequest), IsError: true}
		}
	}
	var args []string
	if first > 0 {
		args = append(args, "-f", strconv.Itoa(first), "-l", strconv.Itoa(last))
	}
	args = append(args, path, "-") // "-" writes extracted text to stdout
	out, err := exec.CommandContext(c.Ctx, bin, args...).Output()
	if err != nil {
		return Result{Content: "Error: pdftotext failed: " + err.Error(), IsError: true}
	}
	if st, serr := os.Stat(path); serr == nil {
		c.MarkRead(path, st.ModTime().UnixNano(), st.Size())
	}
	markEmitted(path)
	text := strings.TrimSpace(string(out))
	if text == "" {
		return Result{Content: fmt.Sprintf("No text extracted from %s (pages %s). The PDF may be image-only/scanned.", path, orDefault(pages, "all"))}
	}
	return Result{Content: text}
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
	markEmitted(path)
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
	if c.OnFileChanged != nil {
		c.OnFileChanged(in.FilePath, "change")
	}
	st, _ := os.Stat(in.FilePath)
	if st != nil {
		c.MarkRead(in.FilePath, st.ModTime().UnixNano(), st.Size())
	}
	clearEmitted(in.FilePath)
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
	clearEmitted(in.FilePath)
	if c.OnFileChanged != nil {
		c.OnFileChanged(in.FilePath, "change")
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
	clearEmitted(in.NotebookPath)
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
