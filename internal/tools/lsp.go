package tools

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/gbasin/claude-go/internal/lsp"
)

func init() {
	Register(&Tool{Def: loadSchema("LSP"), Exec: execLSP})
}

type lspIn struct {
	Operation string `json:"operation"`
	FilePath  string `json:"filePath"`
	Line      int    `json:"line"`
	Character int    `json:"character"`
	Query     string `json:"query"`
}

func execLSP(c *Context, raw json.RawMessage) Result {
	var in lspIn
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{Content: "invalid input: " + err.Error(), IsError: true}
	}
	root := c.CWD
	if in.FilePath != "" && !filepath.IsAbs(in.FilePath) {
		in.FilePath = filepath.Join(c.CWD, in.FilePath)
	}
	client := lsp.For(in.FilePath, root)
	if client == nil {
		return Result{Content: fmt.Sprintf(
			"No LSP server available for %s (install gopls/typescript-language-server/pylsp/etc.)",
			filepath.Ext(in.FilePath)), IsError: true}
	}
	locStr := func(l lsp.Location) string {
		return fmt.Sprintf("%s:%d:%d", strings.TrimPrefix(l.URI, "file://"),
			l.Range.Start.Line+1, l.Range.Start.Character+1)
	}
	switch in.Operation {
	case "goToDefinition", "findReferences":
		method := "textDocument/definition"
		if in.Operation == "findReferences" {
			method = "textDocument/references"
		}
		locs, err := client.Locations(method, in.FilePath, in.Line, in.Character)
		if err != nil {
			return Result{Content: "lsp: " + err.Error(), IsError: true}
		}
		var b strings.Builder
		for _, l := range locs {
			b.WriteString(locStr(l) + "\n")
		}
		if b.Len() == 0 {
			return Result{Content: "no results"}
		}
		return Result{Content: b.String()}
	case "hover":
		s, err := client.Hover(in.FilePath, in.Line, in.Character)
		if err != nil {
			return Result{Content: "lsp: " + err.Error(), IsError: true}
		}
		if s == "" {
			s = "no hover info"
		}
		return Result{Content: s}
	case "documentSymbol":
		res, err := client.Symbols(in.FilePath)
		if err != nil {
			return Result{Content: "lsp: " + err.Error(), IsError: true}
		}
		return Result{Content: string(res)}
	case "workspaceSymbol":
		res, err := client.WorkspaceSymbols(in.Query)
		if err != nil {
			return Result{Content: "lsp: " + err.Error(), IsError: true}
		}
		return Result{Content: string(res)}
	case "goToImplementation":
		locs, err := client.Locations("textDocument/implementation", in.FilePath, in.Line, in.Character)
		if err != nil {
			return Result{Content: "lsp: " + err.Error(), IsError: true}
		}
		var b strings.Builder
		for _, l := range locs {
			b.WriteString(locStr(l) + "\n")
		}
		if b.Len() == 0 {
			return Result{Content: "no results"}
		}
		return Result{Content: b.String()}
	case "prepareCallHierarchy":
		res, err := client.Call("textDocument/prepareCallHierarchy", map[string]any{
			"textDocument": map[string]string{"uri": "file://" + in.FilePath},
			"position":     map[string]int{"line": in.Line - 1, "character": in.Character - 1},
		})
		if err != nil {
			return Result{Content: "lsp: " + err.Error(), IsError: true}
		}
		return Result{Content: string(res)}
	case "incomingCalls", "outgoingCalls":
		item, err := client.Call("textDocument/prepareCallHierarchy", map[string]any{
			"textDocument": map[string]string{"uri": "file://" + in.FilePath},
			"position":     map[string]int{"line": in.Line - 1, "character": in.Character - 1},
		})
		if err != nil {
			return Result{Content: "lsp: " + err.Error(), IsError: true}
		}
		var items []json.RawMessage
		json.Unmarshal(item, &items)
		if len(items) == 0 {
			return Result{Content: "no results"}
		}
		var it any
		json.Unmarshal(items[0], &it)
		method := "callHierarchy/incomingCalls"
		if in.Operation == "outgoingCalls" {
			method = "callHierarchy/outgoingCalls"
		}
		res, err := client.Call(method, map[string]any{"item": it})
		if err != nil {
			return Result{Content: "lsp: " + err.Error(), IsError: true}
		}
		return Result{Content: string(res)}
	case "diagnostics":
		res, err := client.Diagnostics(in.FilePath)
		if err != nil {
			return Result{Content: "lsp: " + err.Error(), IsError: true}
		}
		if len(res) == 0 {
			res = []byte("[]")
		}
		return Result{Content: string(res)}
	}
	return Result{Content: "unknown operation: " + in.Operation, IsError: true}
}
