// Deferred tools + the ToolSearch discovery tool — extracted from
// mod_0314.js (defer decision + description), mod_0386.js (uGn ToolSearch
// tool, k5n keyword search, PMe select-matching, CWt placeholder), and
// mod_0212.js (name constants).
//
// Real semantics replicated:
//   - Tools marked shouldDefer (and every mcp__* tool) are withheld from the
//     request tools array. Their names are announced in a <system-reminder>
//     instead ("deferred_tools_delta" attachment, MTr text).
//   - A DeferredToolPlaceholder stub {defer_loading:true} is spliced into the
//     tools array second-to-last while undiscovered deferred tools remain
//     (CWt / tengu_deferred_stub_tool default true, client-side "tst" mode).
//   - The model calls ToolSearch with "select:<name>[,<name>...]" or keywords;
//     the tool_result carries tool_reference blocks and the matched schemas
//     join the tools array on subsequent requests (the discovered set).
//   - ToolSearch itself is dropped from the tools array when no deferred
//     tools remain (real: "Tool search disabled: no deferred tools").
//
// Unextracted pieces (honest notes):
//   - "standard" mode (server-side tool search when the backend supports it)
//     is not distinguishable client-side; we always run the client-side lane.
//     ENABLE_TOOL_SEARCH=<falsy> disables the feature entirely.
//   - Real derives the discovered set from declared-tools latches scanning
//     the wire history (txe/declaredTools); we keep an explicit session set.
//   - MCP pending/failed server waits (Ze/Xe) are absent — our MCP pool is
//     static after startup.
//   - The per-tool hooks placement override (p9o) and the
//     tengu_purring_yeti / tengu_shiny_stardust always-defer flag lists
//     (empty by default) are not wired; the literal shouldDefer:!0 set is.
package tools

import (
	"encoding/json"
	"math"
	"os"
	"regexp"
	"sort"
	"strings"

	"claude-go/internal/api"
)

const (
	toolSearchName           = "ToolSearch"                                                                          // nl in mod_0212.js
	deferredPlaceholderName  = "DeferredToolPlaceholder"                                                             // _Ie
	deferredPlaceholderDesc  = "Reserved placeholder that keeps deferred tool loading active; never call this tool." // Wbn
	defaultToolSearchResults = 5                                                                                     // max_results default
)

// shouldDeferTools is the literal shouldDefer:!0 set across the bundle
// (mod_0386/0534/0548/0553/0562/1588-1605). Tools real defers that we do not
// implement (SendUserFile, task-list family, connector/runner tools, memory
// tools, ProposeGoal, artifact checkers) are absent by construction.
var shouldDeferTools = map[string]bool{
	"EnterPlanMode": true, "ExitPlanMode": true,
	"ListMcpResourcesTool": true, "ReadMcpResourceTool": true,
	"ReadMcpResourceDirTool": true,
	"RemoteTrigger":          true,
	"WebFetch":               true, "WebSearch": true, "NotebookEdit": true, "Monitor": true,
	"EnterWorktree": true, "ExitWorktree": true,
	"LSP": true, "TaskStop": true,
	"SendMessage": true, "EndConversation": true,
	"PushNotification": true, "DesignSync": true,
	"CronCreate": true, "CronDelete": true, "CronList": true,
	"ArtifactComments": true, "ArtifactData": true,
}

// searchHints are the extracted searchHint strings for deferred builtins —
// keyword search matches these (k5n) at weight 4.
var searchHints = map[string]string{
	"WebFetch":         "fetch and extract content from a URL",
	"WebSearch":        "search the web for current information",
	"NotebookEdit":     "edit Jupyter notebook cells (.ipynb)",
	"RemoteTrigger":    "manage scheduled cloud agent routines; inspect their run history and logs",
	"EnterPlanMode":    "switch to plan mode to design an approach before coding",
	"ExitPlanMode":     "present plan for approval and start coding (plan mode only)",
	"EnterWorktree":    "create an isolated git worktree and switch into it",
	"ExitWorktree":     "exit a worktree session and return to the original directory",
	"LSP":              "code intelligence (definitions, references, symbols, hover)",
	"TaskStop":         "kill a running background task",
	"SendMessage":      "send messages to agent teammates",
	"PushNotification": "send a notification to the user via terminal and optionally mobile",
	"DesignSync":       "sync local design system components to a claude.ai/design project",
	"CronCreate":       "schedule a recurring or one-shot prompt",
	"CronDelete":       "cancel a scheduled cron job",
	"CronList":         "list active cron jobs",
	"Monitor":          "run a shell command in the background and watch its output",
	"EndConversation":  "end the conversation",
}

func init() {
	Register(&Tool{Def: api.Tool{
		// advisor is a typed server-side block — {type: advisor_20260301,
		// name, model, defer_loading}, no input_schema (verified in the
		// real wire capture). The dispatch loop special-cases it.
		Name: "advisor",
		Raw:  json.RawMessage(`{"type":"advisor_20260301","name":"advisor","model":"claude-fable-5-1","defer_loading":true}`),
	}, Exec: func(c *Context, raw json.RawMessage) Result {
		return Result{Content: "advisor is dispatched by the agent loop", IsError: true}
	}})
	Register(&Tool{Def: api.Tool{
		Name:        toolSearchName,
		Description: toolSearchDescription(),
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"query":{"type":"string","description":"Query to find deferred tools. Use \"select:<tool_name>\" for direct selection, or keywords to search."},` +
			`"max_results":{"type":"number","description":"Maximum number of results to return (default: 5)"}},` +
			`"required":["query"]}`),
	}, Exec: execToolSearch})
}

// toolSearchDescription is sYn() — base text + the InputValidationError
// variant (m; the deferred_tools_delta reminder uses that wording
// unconditionally) + the query-forms tail (p).
func toolSearchDescription() string {
	return "Fetches full schema definitions for deferred tools so they can be called.\n\n" +
		"Deferred tools appear by name in <system-reminder> messages." +
		"***REMOVED***" +
		"***REMOVED***" +
		"***REMOVED***" +
		"Query forms:\n" +
		"- \"select:Read,Edit,Grep\" — fetch these exact tools by name\n" +
		"- \"notebook jupyter\" — keyword search, up to max_results best matches\n" +
		"- \"+slack send\" — require \"slack\" in the name, rank by remaining terms"
}

// toolSearchEnabled gates the whole deferred-tool lane. Real's eIe() picks
// "standard"|"tst"|"tst-auto" from ENABLE_TOOL_SEARCH + backend capability;
// we only implement the client-side lane, so an explicit falsy value turns
// deferral off and everything ships inline.
// Default on: live wire capture (2.1.289, eval 404'd by the mock) shows
// real shipping 12 tools — core set + ToolSearch + DeferredToolPlaceholder —
// with MCP/extended tools deferred behind tool search. An explicit falsy
// ENABLE_TOOL_SEARCH still disables the lane.
func toolSearchEnabled() bool {
	v := os.Getenv("ENABLE_TOOL_SEARCH")
	if v == "" {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "false", "no", "off", "standard":
		return false
	}
	return true
}

// IsDeferredTool mirrors h() in mod_0314.js: MCP tools always defer;
// builtins defer on the literal shouldDefer flag.
func IsDeferredTool(t *Tool) bool {
	if t == nil {
		return false
	}
	if t.IsMCP {
		return true
	}
	return shouldDeferTools[t.Def.Name]
}

// IsDeferredName reports whether a registered tool name is deferred —
// used by the agent loop to refuse un-loaded deferred calls like real's
// InputValidationError.
func IsDeferredName(name string) bool {
	if strings.HasPrefix(name, "mcp__") {
		return true
	}
	return shouldDeferTools[name]
}

// --- session discovery cache (real: declared-tools latch, txe) ---

// MarkDiscovered records that a deferred tool's schema has been served to
// the model; subsequent requests include its full def.
func (c *Context) MarkDiscovered(names ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Discovered == nil {
		c.Discovered = map[string]bool{}
	}
	for _, n := range names {
		c.Discovered[n] = true
	}
}

// IsDiscovered reports whether a tool name's schema has been served.
func (c *Context) IsDiscovered(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Discovered[name]
}

// DeferredPool returns the still-deferred tools: shouldDefer/MCP tools whose
// schemas have not been discovered yet. Empty when the lane is disabled.
func (c *Context) DeferredPool() []*Tool {
	if !toolSearchEnabled() {
		return nil
	}
	var out []*Tool
	for _, t := range All() {
		if IsDeferredTool(t) && !c.IsDiscovered(t.Def.Name) {
			out = append(out, t)
		}
	}
	return out
}

// DeferredNames is the reminder line list for the pool (names only — real
// appends ": whenToUse" on tools that carry it; ours don't).
func (c *Context) DeferredNames() []string {
	var names []string
	for _, t := range c.DeferredPool() {
		names = append(names, t.Def.Name)
	}
	return names
}

// DeferredReminder renders the deferred_tools_delta announcement (MTr).
func DeferredReminder(names []string) string {
	return "The following deferred tools are now available via ToolSearch. " +
		"Their schemas are NOT loaded — calling them directly will fail with InputValidationError. " +
		"Use ToolSearch with query \"select:<name>[,<name>...]\" to load tool schemas before calling them:\n" +
		strings.Join(names, "\n")
}

// RequestDefs is the wire tools array: non-deferred defs, plus the
// DeferredToolPlaceholder stub (second-to-last, real's splice point) and the
// ToolSearch def while undiscovered deferred tools remain.
// subagentTools is real's subagent wire surface (captured): the agent
// gets a trimmed set plus SubagentHandback; the deferred placeholder
// sits after Write for subagents (main's is before it).
var subagentTools = []string{
	"Agent", "Bash", "Edit", "Read", "Skill", "ToolSearch", "Write",
	deferredPlaceholderName, "SubagentHandback", "advisor",
}

func RequestDefs(c *Context) []api.Tool {
	if !toolSearchEnabled() {
		return Defs()
	}
	var out []api.Tool
	var deferred []string
	sub := c.Subagent
	allowed := map[string]bool{}
	for _, n := range c.AllowedTools {
		allowed[n] = true
	}
	for _, t := range All() {
		if len(allowed) > 0 && !allowed[t.Def.Name] && t.Def.Name != "ToolSearch" {
			continue
		}
		if IsDeferredTool(t) && !c.IsDiscovered(t.Def.Name) {
			deferred = append(deferred, t.Def.Name)
			continue
		}
		out = append(out, t.Def)
	}
	if len(deferred) == 0 {
		return out
	}
	// real's wire order (captured): …, Skill, ToolSearch, Workflow,
	// DeferredToolPlaceholder, Write, advisor — the placeholder splices in
	// before Write, advisor rides at the tail.
	ph := api.Tool{
		Name:         deferredPlaceholderName,
		Description:  deferredPlaceholderDesc,
		InputSchema:  json.RawMessage(`{"type":"object","properties":{}}`),
		DeferLoading: true,
	}
	for i, t := range out {
		if t.Name == "Write" {
			out = append(out[:i], append([]api.Tool{ph}, out[i:]...)...)
			break
		}
	}
	// subagents get real's trimmed surface with SubagentHandback
	if sub {
		var sOut []api.Tool
		seen := map[string]bool{}
		for _, n := range subagentTools {
			switch n {
			case deferredPlaceholderName:
				if len(deferred) > 0 {
					sOut = append(sOut, ph)
				}
			default:
				if t, ok := registry[n]; ok {
					sOut = append(sOut, t.Def)
					seen[n] = true
				}
			}
		}
		return sOut
	}
	return out
}

// --- the ToolSearch call (uGn.create.call in mod_0386.js) ---

func execToolSearch(c *Context, raw json.RawMessage) Result {
	var in struct {
		Query      string  `json:"query"`
		MaxResults float64 `json:"max_results"`
	}
	json.Unmarshal(raw, &in)
	max := int(in.MaxResults)
	if max <= 0 {
		max = defaultToolSearchResults
	}
	pool := c.DeferredPool()
	all := All()

	var matches []string
	if m := regexp.MustCompile(`(?i)^select:(.+)$`).FindStringSubmatch(in.Query); m != nil {
		// select:Name1,Name2 — PMe per name: exact-name hit anywhere, else
		// mcp trailing-name match inside the deferred pool.
		for _, want := range strings.Split(m[1], ",") {
			want = strings.TrimSpace(want)
			if want == "" {
				continue
			}
			for _, t := range selectMatch(pool, all, want) {
				if !strSliceContains(matches, t) {
					matches = append(matches, t)
				}
			}
		}
	} else {
		matches = keywordSearch(in.Query, pool, all, max)
	}
	if len(matches) == 0 {
		return Result{Content: "No matching deferred tools found"}
	}
	c.MarkDiscovered(matches...)
	// real's tool_result is a bare list of tool_reference blocks — Refs
	// renders as that; see agent.toolResultBlock.
	return Result{Refs: matches}
}

// selectMatch mirrors PMe: exact name in the pool or in the full tool list
// (an already-loaded tool re-selects harmlessly), else mcp__server__tool
// matching on the trailing tool name.
func selectMatch(pool, all []*Tool, name string) []string {
	for _, t := range pool {
		if t.Def.Name == name {
			return []string{name}
		}
	}
	for _, t := range all {
		if t.Def.Name == name {
			return []string{name}
		}
	}
	var out []string
	for _, t := range pool {
		if strings.HasPrefix(t.Def.Name, "mcp__") {
			parts := strings.SplitN(t.Def.Name, "__", 3)
			if len(parts) == 3 && parts[2] == name {
				out = append(out, t.Def.Name)
			}
		}
	}
	return out
}

// --- k5n keyword search (mod_0386.js) ---
//
// Parts model (pP): builtin names split camelCase/underscore into words;
// mcp names split server/tool on [\s_.]+. Scoring: exact part +10 (+12 mcp),
// part-contains +5 (+6), coarse part (full name / server/tool segment) exact
// +10 (+12), coarse-contains +3 (+4), full-name contains +3, searchHint
// word-boundary regex +4, description regex +2.

type nameParts struct {
	parts       []string
	coarseParts []string
	full        string
	isMCP       bool
}

func splitName(t *Tool) nameParts {
	name := t.Def.Name
	if t.IsMCP || strings.HasPrefix(name, "mcp__") {
		segs := strings.SplitN(name, "__", 3)
		var coarse []string
		if len(segs) == 3 {
			coarse = []string{strings.ToLower(segs[1]), strings.ToLower(segs[2])}
		} else {
			coarse = []string{strings.ToLower(name)}
		}
		var parts []string
		for _, s := range coarse {
			for _, p := range regexp.MustCompile(`[\s_.]+`).Split(s, -1) {
				if p != "" {
					parts = append(parts, p)
				}
			}
		}
		return nameParts{parts, coarse, strings.Join(parts, " "), true}
	}
	// [a-z][A-Z] boundaries then underscores → words
	spaced := regexp.MustCompile(`([a-z])([A-Z])`).ReplaceAllString(name, "$1 $2")
	var parts []string
	for _, p := range strings.Fields(strings.ReplaceAll(strings.ToLower(spaced), "_", " ")) {
		parts = append(parts, p)
	}
	return nameParts{parts, []string{strings.ToLower(name)}, strings.Join(parts, " "), false}
}

func wordBoundaryRE(term string) *regexp.Regexp {
	return regexp.MustCompile(`\b` + regexp.QuoteMeta(term) + `\b`)
}

func strSliceContains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func keywordSearch(query string, pool, all []*Tool, max int) []string {
	q := strings.ToLower(strings.TrimSpace(query))
	// exact name match wins outright (pool first, then all tools)
	for _, list := range [][]*Tool{pool, all} {
		for _, t := range list {
			if strings.ToLower(t.Def.Name) == q {
				return []string{t.Def.Name}
			}
		}
	}
	if strings.HasPrefix(q, "mcp__") && len(q) > 5 {
		var out []string
		for _, t := range pool {
			if strings.HasPrefix(strings.ToLower(t.Def.Name), q) {
				out = append(out, t.Def.Name)
				if len(out) >= max {
					return out
				}
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	terms := strings.Fields(q)
	var required, rank []string
	for _, t := range terms {
		if strings.HasPrefix(t, "+") && len(t) > 1 {
			required = append(required, t[1:])
		} else {
			rank = append(rank, t)
		}
	}
	scoring := terms
	if len(required) > 0 {
		scoring = append(append([]string{}, required...), rank...)
	}
	regexes := map[string]*regexp.Regexp{}
	for _, t := range scoring {
		regexes[t] = wordBoundaryRE(t)
	}

	requiredOK := func(t *Tool) bool {
		np := splitName(t)
		desc := strings.ToLower(t.Def.Description)
		hint := strings.ToLower(searchHints[t.Def.Name])
		for _, req := range required {
			re := regexes[req]
			ok := strSliceContains(np.parts, req) ||
				anyContains(np.parts, req) ||
				strSliceContains(np.coarseParts, req) ||
				anyContains(np.coarseParts, req) ||
				re.MatchString(desc) ||
				(hint != "" && re.MatchString(hint))
			if !ok {
				return false
			}
		}
		return true
	}

	type scored struct {
		name  string
		score int
	}
	var results []scored
	for _, t := range pool {
		if len(required) > 0 && !requiredOK(t) {
			continue
		}
		np := splitName(t)
		desc := strings.ToLower(t.Def.Description)
		hint := strings.ToLower(searchHints[t.Def.Name])
		score := 0
		for _, term := range scoring {
			if strSliceContains(np.parts, term) {
				if np.isMCP {
					score += 12
				} else {
					score += 10
				}
			} else if anyContains(np.parts, term) {
				if np.isMCP {
					score += 6
				} else {
					score += 5
				}
			}
			if strSliceContains(np.coarseParts, term) {
				if np.isMCP {
					score += 12
				} else {
					score += 10
				}
			} else if anyContains(np.coarseParts, term) {
				if np.isMCP {
					score += 4
				} else {
					score += 3
				}
			}
			if strings.Contains(np.full, term) && score == 0 {
				score += 3
			}
			if hint != "" && regexes[term].MatchString(hint) {
				score += 4
			}
			if regexes[term].MatchString(desc) {
				score += 2
			}
		}
		if score > 0 {
			results = append(results, scored{t.Def.Name, score})
		}
	}
	sort.SliceStable(results, func(i, j int) bool { return results[i].score > results[j].score })
	out := make([]string, 0, int(math.Min(float64(len(results)), float64(max))))
	for i := 0; i < len(results) && len(out) < max; i++ {
		out = append(out, results[i].name)
	}
	return out
}

func anyContains(parts []string, term string) bool {
	for _, p := range parts {
		if strings.Contains(p, term) {
			return true
		}
	}
	return false
}
