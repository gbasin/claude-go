package repl

// keybindings.json remap layer (see mod_0245 default table + mod_0643 docs).
//
// File: ~/.claude/keybindings.json
//
//	{"bindings":[
//	  {"context":"Chat","bindings":{"ctrl+e":"chat:externalEditor","ctrl+u":null}}
//	]}
//
// Real validates against 27 contexts (mod_0245 KEYBINDING_CONTEXTS);
// we accept all of them, but only Chat/Global have live handlers —
// bindings for dialog contexts are stored (they'd consume the key in
// that dialog, matching real's inert-on-unknown semantics) and do
// nothing at the prompt.
// Contexts supported here: Chat (prompt editor) and Global. A string value
// rebinds the key to an action; null unbinds the default. Chords are
// space-separated keystrokes ("ctrl+x ctrl+k") with a 3s timeout between
// keystrokes. Canonical keystroke form is modifiers in fixed order joined
// with '+' plus a key name: "ctrl+alt+shift+cmd+<name>".

import (
	"encoding/json"
	"fmt"
	"github.com/gbasin/claude-go/internal/config"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

var kbModAliases = map[string]int{
	"ctrl": modCtrl, "control": modCtrl,
	"alt": modAlt, "opt": modAlt, "option": modAlt,
	"meta":  modAlt, // alt and meta are identical in terminals
	"shift": modShift,
	"cmd":   modCmd, "command": modCmd, "super": modCmd, "win": modCmd,
}

var kbSpecialKeys = map[string]bool{
	"escape": true, "esc": true, "enter": true, "return": true, "tab": true,
	"space": true, "backspace": true, "bksp": true, "delete": true,
	"insert": true, "up": true, "down": true, "left": true, "right": true,
	"home": true, "end": true, "pageup": true, "pagedown": true,
	"wheelup": true, "wheeldown": true, "capslock": true, "clear": true,
}

// kbContexts — real's 29 valid context names (mod_0266 v9t for 296).
// Bindings in contexts we don't render are stored but never fire at
// the prompt; dialogs resolve them through dialogAction/sessionKB.
var kbContexts = map[string]bool{
	"Global": true, "Chat": true, "Autocomplete": true,
	"Confirmation": true, "Help": true, "Transcript": true,
	"HistorySearch": true, "Task": true, "ThemePicker": true,
	"Settings": true, "Tabs": true, "Attachments": true,
	"Footer": true, "AbovePrompt": true, "AbovePromptInput": true,
	"AbovePromptSelect": true, "Pane": true, "PaneField": true,
	"MessageSelector": true, "DiffDialog": true, "DiffPanel": true,
	"FeedbackSurvey": true, "ModelPicker": true, "EffortSlider": true,
	"Select": true, "Plugin": true, "Scroll": true, "Agents": true,
	"ProactivityMenu": true,
}

// kbActiveCtxs — the prompt editor's resolution order (real builds
// [focused contexts…, handler's own context, Global]).
var kbActiveCtxs = []string{"Chat", "Global"}

// kbReserved — non-rebindable keys (ctrl+m==enter, ctrl+[==esc,
// ctrl+i==tab, ctrl+h==backspace are terminal aliases; ctrl+z is job-control
// suspend, handled before the binding layer sees it).
var kbReserved = map[string]string{
	"ctrl+c":   "interrupts Claude",
	"ctrl+d":   "exits Claude",
	"ctrl+m":   "is the Enter key in terminals",
	"ctrl+[":   "is the Escape key in terminals",
	"ctrl+i":   "is the Tab key in terminals",
	"ctrl+h":   "is the Backspace key in terminals",
	"capslock": "is not a terminal key",
	"ctrl+z":   "is terminal suspend",
}

// dispatchKb resolves a canonical chord in ONE context — vimKey uses it
// to keep Chat/Global bindings live inside normal/visual modes (real's
// keymap stays active under the vim state machine).
func (e *editor) dispatchKb(k key, ctx, cs string) (*readResult, bool) {
	act, ok := e.kb[ctx+"\x00"+cs]
	if !ok {
		return nil, false
	}
	if strings.HasPrefix(act, "command:") {
		return &readResult{line: act, ok: true}, true
	}
	if f, ok := kbActions[act]; ok {
		return f(e), true
	}
	return nil, true // bound to an unknown/inert action — consumed dead
}

// kbActions maps action strings to editor behavior.
var kbActions = map[string]func(*editor) *readResult{
	"app:interrupt":   func(e *editor) *readResult { return e.interruptKey() },
	"app:exit":        func(e *editor) *readResult { return e.exitKey() },
	"app:toggleTodos": func(e *editor) *readResult { e.toggleTodos(); return nil },
	"app:toggleTranscript": func(e *editor) *readResult {
		e.toggleExpand()
		return nil
	},
	"history:search":      func(e *editor) *readResult { e.startSearch(); return nil },
	"chat:cancel":         func(e *editor) *readResult { return e.cancelKey() },
	"chat:clearInput":     func(e *editor) *readResult { e.clearInput(); return nil },
	"chat:cycleMode":      func(e *editor) *readResult { e.cyclePermMode(); return nil },
	"chat:killAgents":     func(e *editor) *readResult { e.killAgents(); return nil },
	"chat:externalEditor": func(e *editor) *readResult { return e.externalEditor() },
	"chat:imagePaste":     func(e *editor) *readResult { return e.imagePaste() },
	"chat:submit":         func(e *editor) *readResult { return e.submitKey() },
	"chat:newline":        func(e *editor) *readResult { e.insert('\n'); return nil },
	"chat:undo":           func(e *editor) *readResult { e.undo(); return nil },
	"chat:sendNow":        func(e *editor) *readResult { return e.sendNow() },
	// real's mt/ht: up/down move the cursor between the draft's wrapped
	// rows first; history pages only at the top/bottom row
	"history:previous": func(e *editor) *readResult {
		if !e.cursorUp() {
			e.histPrev()
		}
		return nil
	},
	"history:next": func(e *editor) *readResult {
		if !e.cursorDown() {
			e.histNext()
		}
		return nil
	},
	"chat:clearScreen": func(e *editor) *readResult { fmt.Print("\x1b[2J\x1b[H"); e.redraw(); return nil },
	"chat:queueSubmit": func(e *editor) *readResult {
		return e.queueSubmitKey()
	},
	"chat:modelPicker":    func(e *editor) *readResult { return e.modelPickerKey() },
	"chat:fastMode":       func(e *editor) *readResult { e.toggleFastMode(); return nil },
	"chat:thinkingToggle": func(e *editor) *readResult { e.toggleThinking(); return nil },
	"chat:stash":          func(e *editor) *readResult { e.stashOrPop(); return nil },
	// real: ctrl+y is chat:defaultToNewerModel — it returns false when the
	// kill-ring is non-empty so the editor's yank still runs (mod_1600
	// `if(H9e(k.state)!=="")return!1`). The newer-model upsell has no
	// surface here, so the empty-ring case is a no-op.
	"chat:defaultToNewerModel": func(e *editor) *readResult { e.yankKill(); return nil },
	"app:toggleBrief":          func(e *editor) *readResult { toggleBrief(); return nil },
	// voice:pushToTalk — real binds space in Chat; the space-press path is
	// handled in insertKey (printable chars aren't keystrokes). This entry
	// lets users rebind to a modifier combo via keybindings.json.
	"voice:pushToTalk": func(e *editor) *readResult { e.voiceToggle(); return nil },
	// abovePrompt — real's collapsed-activity band above the composer:
	// ctrl+x ctrl+a expands/collapses it, ctrl+x tab moves focus there
	"abovePrompt:toggle": func(e *editor) *readResult { e.abovePromptToggle(); return nil },
	"abovePrompt:focus":  func(e *editor) *readResult { e.abovePromptFocus(); return nil },
}

// kbValidActions — real's full valid-action registry (mod_0266 ZDe,
// 296). Validation checks the ACTION NAME against this set, not against
// kbActions (which is only the prompt editor's handler table): a known
// action with no handler on a surface is still a legal binding — it
// resolves through dialogAction wherever that context is dispatched.
var kbValidActions = func() map[string]bool {
	names := []string{
		"app:interrupt", "app:exit", "app:toggleTodos", "app:toggleTranscript",
		"app:toggleBrief", "app:toggleReplTab", "app:toggleDiffNoiseFilter",
		"app:diffFileListUp", "app:diffFileListDown", "app:toggleDiffPreSession",
		"app:cycleDiffBase", "app:toggleTerminal", "app:redraw", "app:openArtifact",
		"strip:jump1", "strip:jump2", "strip:jump3", "strip:jump4", "strip:jump5",
		"strip:jump6", "strip:jump7", "strip:jump8", "strip:jump9",
		"strip:next", "strip:previous", "strip:tabNext", "strip:tabPrevious",
		"strip:viewAll", "strip:toggle", "strip:new",
		"history:search", "history:previous", "history:next",
		"chat:cancel", "chat:killAgents", "chat:cycleMode", "chat:cycleProactivity",
		"chat:attentionUp", "chat:attentionDown", "chat:modelPicker",
		"chat:increaseEffort", "chat:decreaseEffort", "chat:fastMode",
		"chat:defaultToNewerModel", "chat:thinkingToggle", "chat:workflowKeywordToggle",
		"chat:submit", "chat:queueSubmit", "chat:sendNow", "chat:newline",
		"chat:undo", "chat:externalEditor", "chat:stash", "chat:imagePaste",
		"chat:clearInput", "chat:clearScreen",
		"autocomplete:accept", "autocomplete:dismiss", "autocomplete:previous", "autocomplete:next",
		"confirm:yes", "confirm:no", "confirm:previous", "confirm:next",
		"confirm:nextField", "confirm:previousField", "confirm:cycleMode", "confirm:toggle",
		"tabs:next", "tabs:previous",
		"transcript:toggleShowAll", "transcript:exit",
		"historySearch:next", "historySearch:accept", "historySearch:cancel",
		"historySearch:execute", "historySearch:cycleScope",
		"task:background",
		"theme:toggleSyntaxHighlighting", "theme:editCustom",
		"help:dismiss",
		"proactivityMenu:previousMode", "proactivityMenu:nextMode",
		"attachments:next", "attachments:previous", "attachments:remove", "attachments:exit",
		"footer:up", "footer:down", "footer:next", "footer:previous",
		"footer:openSelected", "footer:clearSelection", "footer:close", "footer:dismiss",
		"abovePrompt:toggle", "abovePrompt:focus", "abovePrompt:next", "abovePrompt:previous",
		"abovePrompt:press", "abovePrompt:leave", "abovePrompt:highlightNext", "abovePrompt:highlightPrevious",
		"pane:scrollUp", "pane:scrollDown", "pane:pageUp", "pane:pageDown",
		"pane:top", "pane:bottom", "pane:grow", "pane:shrink", "pane:close",
		"pane:next", "pane:previous",
		"diff:dismiss", "diff:previousSource", "diff:nextSource", "diff:back",
		"diff:previousFile", "diff:nextFile",
		"feedbackSurvey:bad", "feedbackSurvey:fine", "feedbackSurvey:good",
		"modelPicker:decreaseEffort", "modelPicker:increaseEffort", "modelPicker:thisSessionOnly",
		"effortSlider:decreaseEffort", "effortSlider:increaseEffort",
		"effortSlider:toggleUltracode", "effortSlider:thisSessionOnly",
		"select:next", "select:previous", "select:pageUp", "select:pageDown",
		"select:first", "select:last", "select:accept", "select:cancel",
		"plugin:toggle", "plugin:install", "plugin:favorite", "plugin:cycleMarketplace",
		"permission:toggleDebug",
		"settings:search", "settings:retry", "settings:periodDay",
		"settings:periodWeek", "settings:sortByTokens",
		"voice:pushToTalk",
		"scroll:pageUp", "scroll:pageDown", "scroll:lineUp", "scroll:lineDown",
		"scroll:top", "scroll:bottom", "scroll:halfPageUp", "scroll:halfPageDown",
		"scroll:fullPageUp", "scroll:fullPageDown",
		"selection:copy", "selection:clear", "selection:extendLeft", "selection:extendRight",
		"selection:extendUp", "selection:extendDown", "selection:extendLineStart", "selection:extendLineEnd",
		"agents:switchView", "agents:togglePin", "agents:find",
		"agents:nextGroup", "agents:previousGroup", "agents:rename", "agents:setGroup",
	}
	m := make(map[string]bool, len(names)+8)
	for _, n := range names {
		m[n] = true
	}
	// plus the legacy names real still accepts (mod_0266 Re/Z)
	for n := range kbLegacyActions {
		m[n] = true
	}
	return m
}()

// kbLegacyActions — real's renamed-action aliases (mod_0266 Re):
// keybindings.json written for an older version still resolves — the
// legacy name maps to its current action at load time.
var kbLegacyActions = map[string]string{
	"messageSelector:up":     "select:previous",
	"messageSelector:down":   "select:next",
	"messageSelector:top":    "select:first",
	"messageSelector:bottom": "select:last",
	"messageSelector:select": "select:accept",
	"diff:viewDetails":       "select:accept",
}

// defaultKeybindings mirrors the relevant rows of the CC binding table.
var defaultKeybindings = []struct {
	context string
	chord   string
	action  string
}{
	{"Global", "ctrl+c", "app:interrupt"},
	{"Global", "ctrl+d", "app:exit"},
	{"Global", "ctrl+t", "app:toggleTodos"},
	{"Global", "ctrl+o", "app:toggleTranscript"},
	{"Global", "ctrl+r", "history:search"},
	{"Chat", "escape", "chat:cancel"},
	{"Chat", "ctrl+l", "chat:clearInput"},
	{"Chat", "ctrl+x ctrl+k", "chat:killAgents"},
	{"Chat", "ctrl+x ctrl+e", "chat:externalEditor"},
	{"Chat", "ctrl+x ctrl+a", "abovePrompt:toggle"},
	{"Chat", "ctrl+x tab", "abovePrompt:focus"},
	{"Chat", "shift+tab", "chat:cycleMode"},
	{"Chat", "ctrl+g", "chat:externalEditor"},
	{"Chat", "ctrl+v", "chat:imagePaste"},
	{"Chat", "alt+v", "chat:imagePaste"},
	// rest of the real Chat/Global table
	{"Global", "ctrl+shift+b", "app:toggleBrief"},
	{"Chat", "ctrl+j", "chat:newline"},
	{"Chat", "ctrl+_", "chat:undo"},
	{"Chat", "ctrl+-", "chat:undo"},
	{"Chat", "ctrl+s", "chat:stash"},
	{"Chat", "ctrl+y", "chat:defaultToNewerModel"},
	{"Chat", "meta+p", "chat:modelPicker"},
	{"Chat", "meta+o", "chat:fastMode"},
	{"Chat", "meta+t", "chat:thinkingToggle"},
	{"Chat", "ctrl+x enter", "chat:queueSubmit"},
	{"Chat", "ctrl+x ctrl+s", "chat:sendNow"},
	{"Chat", "ctrl+enter", "chat:sendNow"},
	{"Chat", "space", "voice:pushToTalk"}, // real's default (mod_0245)
	{"Chat", "up", "history:previous"},
	{"Chat", "down", "history:next"},
	{"Chat", "cmd+k", "chat:clearScreen"},
	{"Chat", "meta+w", "chat:workflowKeywordToggle"}, // bound, inert for us
	{"Global", "ctrl+]", "app:openArtifact"},         // bound, inert for us
	// Transcript context defaults (real's mod_0266 table) — user remaps
	// resolve through dialogAction; the dialog switch stays the fallback
	{"Transcript", "ctrl+e", "transcript:toggleShowAll"},
	{"Transcript", "ctrl+c", "transcript:exit"},
	{"Transcript", "escape", "transcript:exit"},
	{"Transcript", "q", "transcript:exit"},
	{"Transcript", "ctrl+u", "scroll:halfPageUp"},
	{"Transcript", "ctrl+d", "scroll:halfPageDown"},
	{"Transcript", "ctrl+b", "scroll:fullPageUp"},
	{"Transcript", "ctrl+f", "scroll:fullPageDown"},
	{"Transcript", "ctrl+n", "scroll:lineDown"},
	{"Transcript", "ctrl+p", "scroll:lineUp"},
	{"Transcript", "g", "scroll:top"},
	{"Transcript", "shift+g", "scroll:bottom"},
	{"Transcript", "j", "scroll:lineDown"},
	{"Transcript", "k", "scroll:lineUp"},
	{"Transcript", "up", "scroll:lineUp"},
	{"Transcript", "down", "scroll:lineDown"},
	{"Transcript", "home", "scroll:top"},
	{"Transcript", "end", "scroll:bottom"},
	{"Transcript", "space", "scroll:fullPageDown"},
	{"Transcript", "b", "scroll:fullPageUp"},
	// Select context (real's pickers) — up/down/j/k/ctrl+n/p/home/end
	{"Select", "up", "select:previous"},
	{"Select", "down", "select:next"},
	{"Select", "j", "select:next"},
	{"Select", "k", "select:previous"},
	{"Select", "ctrl+n", "select:next"},
	{"Select", "ctrl+p", "select:previous"},
	{"Select", "pageup", "select:pageUp"},
	{"Select", "pagedown", "select:pageDown"},
	{"Select", "home", "select:first"},
	{"Select", "end", "select:last"},
	{"Select", "enter", "select:accept"},
	{"Select", "escape", "select:cancel"},
	{"Global", "ctrl+up", "app:diffFileListUp"},     // bound, inert for us
	{"Global", "ctrl+down", "app:diffFileListDown"}, // bound, inert for us
	{"Global", "meta+up", "app:diffFileListUp"},
	{"Global", "meta+down", "app:diffFileListDown"},
	// Remaining real defaults (mod_0266 w1). These never fire at the
	// prompt — kbActiveCtxs is [Chat Global] — but sessionKB mirrors
	// them so each dialog's dialogAction(ctx, k) resolves real's own
	// keys plus any user remap on top. Editor-agent + dialog owners
	// wire the dialogAction call into each surface's key loop.
	{"Chat", "ctrl+shift+-", "chat:undo"},
	{"Chat", "ctrl+shift+_", "chat:undo"},
	{"DiffPanel", "ctrl+x b", "app:cycleDiffBase"},
	{"FeedbackSurvey", "ctrl+x 1", "feedbackSurvey:bad"},
	{"FeedbackSurvey", "ctrl+x 2", "feedbackSurvey:fine"},
	{"FeedbackSurvey", "ctrl+x 3", "feedbackSurvey:good"},
	{"Autocomplete", "tab", "autocomplete:accept"},
	{"Autocomplete", "escape", "autocomplete:dismiss"},
	{"Autocomplete", "up", "autocomplete:previous"},
	{"Autocomplete", "down", "autocomplete:next"},
	{"Settings", "escape", "confirm:no"},
	{"Settings", "up", "select:previous"},
	{"Settings", "down", "select:next"},
	{"Settings", "k", "select:previous"},
	{"Settings", "j", "select:next"},
	{"Settings", "ctrl+p", "select:previous"},
	{"Settings", "ctrl+n", "select:next"},
	{"Settings", "home", "select:first"},
	{"Settings", "end", "select:last"},
	{"Settings", "pageup", "select:pageUp"},
	{"Settings", "pagedown", "select:pageDown"},
	{"Settings", "space", "select:accept"},
	{"Settings", "enter", "select:accept"},
	{"Settings", "/", "settings:search"},
	{"Settings", "r", "settings:retry"},
	{"Settings", "d", "settings:periodDay"},
	{"Settings", "w", "settings:periodWeek"},
	{"Settings", "t", "settings:sortByTokens"},
	{"Settings", "ctrl+u", "scroll:halfPageUp"},
	{"Settings", "ctrl+d", "scroll:halfPageDown"},
	{"Confirmation", "enter", "confirm:yes"},
	{"Confirmation", "escape", "confirm:no"},
	{"Confirmation", "up", "confirm:previous"},
	{"Confirmation", "down", "confirm:next"},
	{"Confirmation", "tab", "confirm:nextField"},
	{"Confirmation", "space", "confirm:toggle"},
	{"Confirmation", "shift+tab", "confirm:cycleMode"},
	{"Tabs", "tab", "tabs:next"},
	{"Tabs", "shift+tab", "tabs:previous"},
	{"Tabs", "right", "tabs:next"},
	{"Tabs", "left", "tabs:previous"},
	{"HistorySearch", "ctrl+r", "historySearch:next"},
	{"HistorySearch", "escape", "historySearch:accept"},
	{"HistorySearch", "tab", "historySearch:accept"},
	{"HistorySearch", "ctrl+c", "historySearch:cancel"},
	{"HistorySearch", "enter", "historySearch:execute"},
	{"HistorySearch", "ctrl+s", "historySearch:cycleScope"},
	{"Task", "ctrl+x ctrl+b", "task:background"},
	{"Task", "ctrl+b", "task:background"},
	{"ThemePicker", "ctrl+t", "theme:toggleSyntaxHighlighting"},
	{"ThemePicker", "ctrl+e", "theme:editCustom"},
	{"Scroll", "pageup", "scroll:pageUp"},
	{"Scroll", "pagedown", "scroll:pageDown"},
	{"Scroll", "wheelup", "scroll:lineUp"},
	{"Scroll", "wheeldown", "scroll:lineDown"},
	{"Scroll", "ctrl+home", "scroll:top"},
	{"Scroll", "ctrl+end", "scroll:bottom"},
	{"Scroll", "ctrl+shift+c", "selection:copy"},
	{"Scroll", "cmd+c", "selection:copy"},
	{"Scroll", "shift+left", "selection:extendLeft"},
	{"Scroll", "shift+right", "selection:extendRight"},
	{"Scroll", "shift+up", "selection:extendUp"},
	{"Scroll", "shift+down", "selection:extendDown"},
	{"Scroll", "shift+home", "selection:extendLineStart"},
	{"Scroll", "shift+end", "selection:extendLineEnd"},
	{"Help", "escape", "help:dismiss"},
	{"Attachments", "right", "attachments:next"},
	{"Attachments", "left", "attachments:previous"},
	{"Attachments", "backspace", "attachments:remove"},
	{"Attachments", "delete", "attachments:remove"},
	{"Attachments", "down", "attachments:exit"},
	{"Attachments", "escape", "attachments:exit"},
	{"Footer", "up", "footer:up"},
	{"Footer", "ctrl+p", "footer:up"},
	{"Footer", "down", "footer:down"},
	{"Footer", "ctrl+n", "footer:down"},
	{"Footer", "right", "footer:next"},
	{"Footer", "left", "footer:previous"},
	{"Footer", "enter", "footer:openSelected"},
	{"Footer", "escape", "footer:clearSelection"},
	{"Footer", "x", "footer:close"},
	{"AbovePrompt", "tab", "abovePrompt:next"},
	{"AbovePrompt", "right", "abovePrompt:next"},
	{"AbovePrompt", "shift+tab", "abovePrompt:previous"},
	{"AbovePrompt", "left", "abovePrompt:previous"},
	{"AbovePrompt", "enter", "abovePrompt:press"},
	{"AbovePrompt", "space", "abovePrompt:press"},
	{"AbovePrompt", "escape", "abovePrompt:leave"},
	{"AbovePrompt", "up", "pane:scrollUp"},
	{"AbovePrompt", "down", "pane:scrollDown"},
	{"AbovePrompt", "pageup", "pane:pageUp"},
	{"AbovePrompt", "pagedown", "pane:pageDown"},
	{"AbovePrompt", "home", "pane:top"},
	{"AbovePrompt", "end", "pane:bottom"},
	{"AbovePromptInput", "tab", "abovePrompt:next"},
	{"AbovePromptInput", "down", "abovePrompt:next"},
	{"AbovePromptInput", "shift+tab", "abovePrompt:previous"},
	{"AbovePromptInput", "up", "abovePrompt:previous"},
	{"AbovePromptInput", "enter", "abovePrompt:press"},
	{"AbovePromptInput", "escape", "abovePrompt:leave"},
	{"AbovePromptSelect", "tab", "abovePrompt:next"},
	{"AbovePromptSelect", "shift+tab", "abovePrompt:previous"},
	{"AbovePromptSelect", "down", "abovePrompt:highlightNext"},
	{"AbovePromptSelect", "up", "abovePrompt:highlightPrevious"},
	{"AbovePromptSelect", "enter", "abovePrompt:press"},
	{"AbovePromptSelect", "escape", "abovePrompt:leave"},
	{"Pane", "tab", "abovePrompt:next"},
	{"Pane", "shift+tab", "abovePrompt:previous"},
	{"Pane", "enter", "abovePrompt:press"},
	{"Pane", "escape", "abovePrompt:leave"},
	{"Pane", "up", "pane:scrollUp"},
	{"Pane", "down", "pane:scrollDown"},
	{"Pane", "pageup", "pane:pageUp"},
	{"Pane", "pagedown", "pane:pageDown"},
	{"Pane", "home", "pane:top"},
	{"Pane", "end", "pane:bottom"},
	{"Pane", "ctrl+x left", "pane:grow"},
	{"Pane", "ctrl+x up", "pane:grow"},
	{"Pane", "ctrl+x right", "pane:shrink"},
	{"Pane", "ctrl+x down", "pane:shrink"},
	{"Pane", "ctrl+x x", "pane:close"},
	{"PaneField", "ctrl+x x", "pane:close"},
	{"DiffDialog", "escape", "diff:dismiss"},
	{"DiffDialog", "left", "diff:previousSource"},
	{"DiffDialog", "right", "diff:nextSource"},
	{"DiffDialog", "up", "diff:previousFile"},
	{"DiffDialog", "down", "diff:nextFile"},
	{"DiffDialog", "j", "diff:nextFile"},
	{"DiffDialog", "k", "diff:previousFile"},
	{"DiffDialog", "pageup", "scroll:pageUp"},
	{"DiffDialog", "pagedown", "scroll:pageDown"},
	{"DiffDialog", "space", "scroll:fullPageDown"},
	{"DiffDialog", "shift+space", "scroll:fullPageUp"},
	{"DiffDialog", "b", "scroll:fullPageUp"},
	{"DiffDialog", "g", "scroll:top"},
	{"DiffDialog", "shift+g", "scroll:bottom"},
	{"DiffDialog", "home", "scroll:top"},
	{"DiffDialog", "end", "scroll:bottom"},
	{"ModelPicker", "left", "modelPicker:decreaseEffort"},
	{"ModelPicker", "right", "modelPicker:increaseEffort"},
	{"ModelPicker", "s", "modelPicker:thisSessionOnly"},
	{"EffortSlider", "left", "effortSlider:decreaseEffort"},
	{"EffortSlider", "right", "effortSlider:increaseEffort"},
	{"EffortSlider", "tab", "effortSlider:toggleUltracode"},
	{"EffortSlider", "s", "effortSlider:thisSessionOnly"},
	{"Plugin", "space", "plugin:toggle"},
	{"Plugin", "i", "plugin:install"},
	{"Plugin", "f", "plugin:favorite"},
	{"Plugin", "ctrl+s", "plugin:cycleMarketplace"},
	{"Agents", "ctrl+s", "agents:switchView"},
	{"Agents", "ctrl+t", "agents:togglePin"},
	{"Agents", "ctrl+f", "agents:find"},
	{"Agents", "ctrl+r", "agents:rename"},
	{"Agents", "ctrl+up", "agents:previousGroup"},
	{"Agents", "meta+up", "agents:previousGroup"},
	{"Agents", "ctrl+down", "agents:nextGroup"},
	{"Agents", "meta+down", "agents:nextGroup"},
}

// keystroke renders a decoded key into canonical stroke form, or "" if the
// key isn't bindable (plain printable text, paste content, unknown seq).
func keystroke(k key) string {
	var name string
	mods := int(k.mod) & 0xF
	if int(k.mod)&32 != 0 { // kitty meta bit -> alt (identical in terminals)
		mods |= modAlt
	}
	switch {
	case k.paste != "":
		return ""
	case k.seq != "":
		switch k.seq {
		case "[A":
			name = "up"
		case "[B":
			name = "down"
		case "[C":
			name = "right"
		case "[D":
			name = "left"
		case "[H", "[1~", "[7~":
			name = "home"
		case "[F", "[4~", "[8~":
			name = "end"
		case "[Z":
			name = "tab"
			mods |= modShift
		case "[2~":
			name = "insert"
		case "[3~":
			name = "delete"
		case "[5~":
			name = "pageup"
		case "[6~":
			name = "pagedown"
		default:
			return ""
		}
	case k.b == 0x1b:
		name = "escape"
	case k.b == '\r' || k.b == '\n':
		if k.r == '\n' {
			return "" // decoded newline insert (alt+enter), not a keystroke
		}
		name = "enter"
	case k.b == '\t':
		name = "tab"
	case k.b == ' ':
		if mods == 0 {
			return ""
		}
		name = "space"
	case k.b == 0x7f || k.b == 8:
		name = "backspace"
	case k.b >= 1 && k.b <= 26:
		name = string(rune('a' + k.b - 1))
		mods |= modCtrl
	case k.b == 0x1f:
		name = "-"
		mods |= modCtrl // ctrl+_ / ctrl+- — real binds both to chat:undo
	default:
		r := k.r
		if r == 0 && k.b >= 32 && k.b < 127 {
			r = rune(k.b)
		}
		if r == 0 || mods == 0 {
			return "" // plain printable text isn't a bindable stroke
		}
		if r == ' ' {
			name = "space"
		} else {
			if r >= 'A' && r <= 'Z' {
				r += 32
			}
			name = string(r)
		}
	}
	var sb strings.Builder
	if mods&modCtrl != 0 {
		sb.WriteString("ctrl+")
	}
	if mods&modAlt != 0 {
		sb.WriteString("alt+")
	}
	if mods&modShift != 0 {
		sb.WriteString("shift+")
	}
	if mods&modCmd != 0 {
		sb.WriteString("cmd+")
	}
	sb.WriteString(name)
	return sb.String()
}

// parseKeystroke canonicalizes one keystroke ("ctrl+shift+p"). Returns ""
// with a warning on invalid input. Non-modifier junk tokens are dropped with
// a warning, matching CC's validation message.
func parseKeystroke(s string) (string, []string) {
	var warns []string
	mods, name := 0, ""
	for _, tok := range strings.Split(s, "+") {
		t := strings.ToLower(strings.TrimSpace(tok))
		if t == "" {
			continue
		}
		if m, ok := kbModAliases[t]; ok {
			mods |= m
			continue
		}
		if kbSpecialKeys[t] || len([]rune(t)) == 1 || isFKey(t) {
			name = t
			continue
		}
		warns = append(warns, fmt.Sprintf("%q is not a modifier, so %q applies to %q instead", t, s, t))
	}
	if name == "" {
		warns = append(warns, fmt.Sprintf("invalid keystroke %q", s))
		return "", warns
	}
	var sb strings.Builder
	if mods&modCtrl != 0 {
		sb.WriteString("ctrl+")
	}
	if mods&modAlt != 0 {
		sb.WriteString("alt+")
	}
	if mods&modShift != 0 {
		sb.WriteString("shift+")
	}
	if mods&modCmd != 0 {
		sb.WriteString("cmd+")
	}
	sb.WriteString(name)
	return sb.String(), warns
}

func isFKey(s string) bool {
	if len(s) < 2 || s[0] != 'f' {
		return false
	}
	for _, ch := range s[1:] {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

// parseChord canonicalizes a space-separated chord ("ctrl+x ctrl+k").
func parseChord(s string) (string, []string) {
	var warns []string
	var parts []string
	for _, tok := range strings.Fields(s) {
		cs, w := parseKeystroke(tok)
		warns = append(warns, w...)
		if cs == "" {
			return "", warns
		}
		parts = append(parts, cs)
	}
	return strings.Join(parts, " "), warns
}

// loadKeybindings builds the effective binding map: defaults first, then
// ~/.claude/keybindings.json applied over them (null unbinds).
func (e *editor) loadKeybindings() {
	e.kb = map[string]string{}
	e.kbPrefix = map[string]bool{}
	e.kbUnbound = map[string]bool{}
	var warns []string
	for _, d := range defaultKeybindings {
		cs, w := parseChord(d.chord)
		warns = append(warns, w...)
		if cs != "" {
			e.kb[d.context+"\x00"+cs] = d.action
		}
	}
	if b, err := os.ReadFile(filepath.Join(config.ClaudeDir(), "keybindings.json")); err == nil {
		warns = append(warns, e.applyKeybindingFile(b)...)
	}
	for cs := range e.kb {
		ctx, chord, _ := strings.Cut(cs, "\x00")
		parts := strings.Split(chord, " ")
		for i := 1; i < len(parts); i++ {
			e.kbPrefix[ctx+"\x00"+strings.Join(parts[:i], " ")] = true
		}
	}
	// session mirror — dialogs/pickers without an editor reference resolve
	// user remaps through this (real's dialogs read the same keymap)
	sessionKB.Store(map[string]string(e.kb))
	e.kbResolve = map[string]string{}
	// display-resolution map (real's qa()/Y_): action -> the chord the UI
	// should name in hints. Defaults claim first so user-added extra
	// bindings don't shadow the canonical label; unbound actions fall
	// back to the hardcoded name.
	e.kbResolve = map[string]string{}
	for _, d := range defaultKeybindings {
		ck := d.context + "\x00" + d.action
		if _, ok := e.kbResolve[ck]; !ok {
			cs, _ := parseChord(d.chord)
			if _, bound := e.kb[d.context+"\x00"+cs]; bound {
				e.kbResolve[ck] = cs
			}
		}
	}
	var extra []string
	for cs, act := range e.kb {
		extra = append(extra, cs+"\x00"+act)
	}
	sort.Strings(extra)
	for _, ea := range extra {
		cs, act, _ := strings.Cut(ea, "\x00")
		ctx, chord, _ := strings.Cut(cs, "\x00")
		k := ctx + "\x00" + act
		if _, ok := e.kbResolve[k]; !ok {
			e.kbResolve[k] = chord
		}
	}
	if len(warns) > 0 {
		// one summary line to stderr — real shows a dismissible notice,
		// not 27 lines of stderr noise. Detail stays behind debug envs.
		fmt.Fprintf(os.Stderr, "[keybindings] %d validation issue(s) (CLAUDE_GO_DEBUG_KB=1 for detail)\n", len(warns))
		if os.Getenv("CLAUDE_GO_DEBUG_KB") != "" {
			for _, w := range warns {
				fmt.Fprintf(os.Stderr, "[keybindings] %s\n", w)
			}
		}
	}
}

func (e *editor) applyKeybindingFile(b []byte) []string {
	var warns []string
	var file struct {
		Bindings []struct {
			Context  string             `json:"context"`
			Bindings map[string]*string `json:"bindings"`
		} `json:"bindings"`
	}
	if err := json.Unmarshal(b, &file); err != nil {
		return []string{fmt.Sprintf("invalid JSON in keybindings.json: %v", err)}
	}
	if file.Bindings == nil {
		return []string{`keybindings.json must have a "bindings" array`}
	}
	for _, cb := range file.Bindings {
		if !kbContexts[cb.Context] {
			var names []string
			for n := range kbContexts {
				names = append(names, n)
			}
			sort.Strings(names)
			warns = append(warns, fmt.Sprintf("Unknown context %q — Valid contexts: %s", cb.Context, strings.Join(names, ", ")))
			continue
		}
		for kstr, act := range cb.Bindings {
			cs, w := parseChord(kstr)
			warns = append(warns, w...)
			if cs == "" {
				continue
			}
			if first := strings.Split(cs, " ")[0]; kbReserved[first] != "" {
				warns = append(warns, fmt.Sprintf("%q may not work: %s", kstr, kbReserved[first]))
			}
			ck := cb.Context + "\x00" + cs
			if act == nil {
				delete(e.kb, ck) // null unbinds the default
				e.kbUnbound[ck] = true
				continue
			}
			// real rejects unknown action names with invalid_action and
			// DROPS the binding — the default key behavior survives
			// (a typo doesn't kill the key). Validation is against the
			// action-name registry (kbValidActions), not the handler
			// table — a known action with no handler in a surface is
			// still legal. command:<name> executes a slash command and
			// is always accepted.
			a := *act
			if !strings.HasPrefix(a, "command:") {
				if cur, ok := kbLegacyActions[a]; ok {
					a = cur // real remaps renamed actions at load (mod_0266 Re)
				}
				if !kbValidActions[a] {
					warns = append(warns,
						fmt.Sprintf("%s: invalid_action %q — this binding is ignored", kstr, a))
					continue
				}
			}
			delete(e.kbUnbound, ck)
			e.kb[ck] = a
		}
	}
	return warns
}

// resolvedChord returns the display chord for an action in a context —
// real's qa()/Y_ resolver used by footer hints and "(ctrl+o to expand)".
func (e *editor) resolvedChord(ctx, action, fallback string) string {
	if e.kbResolve != nil {
		if c, ok := e.kbResolve[ctx+"\x00"+action]; ok {
			return c
		}
	}
	return fallback
}

// bindingKey dispatches a key through the keybinding layer. Returns
// (result, consumed); consumed=false means fall through to default handling.
// busyAction resolves a key to an action name for the mid-turn input
// queue — same context+chord rules as bindingKey, but without dispatch
// (the busy goroutine only needs to know if the key means sendNow etc).
func (e *editor) busyAction(k key) string {
	s := keystroke(k)
	if s == "" {
		e.busyChord = nil
		return ""
	}
	if len(e.busyChord) > 0 && time.Since(e.busyChordAt) >= 3*time.Second {
		e.busyChord = nil
	}
	cand := s
	if len(e.busyChord) > 0 {
		cand = strings.Join(append(append([]string{}, e.busyChord...), s), " ")
	}
	e.busyChord = nil
	for _, ctx := range kbActiveCtxs {
		if e.kbPrefix[ctx+"\x00"+cand] {
			e.busyChord = strings.Fields(cand)
			e.busyChordAt = time.Now()
			return "" // chord prefix — consumed, awaiting the next stroke
		}
	}
	for _, ctx := range kbActiveCtxs {
		if a, ok := e.kb[ctx+"\x00"+cand]; ok {
			return a
		}
	}
	if cand != s {
		// chord missed — retry the stroke standalone
		for _, ctx := range kbActiveCtxs {
			if a, ok := e.kb[ctx+"\x00"+s]; ok {
				return a
			}
		}
	}
	return ""
}

func (e *editor) bindingKey(k key) (*readResult, bool) {
	if e.kb == nil {
		return nil, false
	}
	if len(e.pendingChord) > 0 && time.Since(e.chordAt) >= 3*time.Second {
		e.pendingChord = nil
	}
	s := keystroke(k)
	if s == "" {
		if len(e.pendingChord) > 0 {
			e.pendingChord = nil // non-stroke key breaks the chord
		}
		return nil, false
	}
	hadPending := len(e.pendingChord) > 0
	cand := s
	if hadPending {
		cand = strings.Join(append(append([]string{}, e.pendingChord...), s), " ")
	}
	e.pendingChord = nil
	// real's resolution list at the prompt: focused contexts (none for the
	// editor), own context Chat, then Global — first context with a match
	// wins; unbound within a context consumes the key.
	act, found, consumed := "", false, false
	// chord-prefix detection runs across ALL active contexts before
	// exact-match resolution (mod_0245)
	for _, ctx := range kbActiveCtxs {
		if e.kbPrefix[ctx+"\x00"+cand] {
			e.pendingChord = strings.Fields(cand)
			e.chordAt = time.Now()
			return nil, true
		}
	}
	for _, ctx := range kbActiveCtxs {
		ck := ctx + "\x00" + cand
		if a, ok := e.kb[ck]; ok {
			act, found = a, true
			break
		}
		if e.kbUnbound[ck] {
			consumed = true
			break
		}
	}
	if found {
		if f := kbActions[act]; f != nil {
			return f(e), true
		}
		if strings.HasPrefix(act, "command:") {
			// command:<name> executes a slash command as if typed
			return &readResult{line: "/" + strings.TrimPrefix(act, "command:"), ok: true}, true
		}
		// unknown action name: real stores the binding and the key goes
		// inert — consumed, no handler
		return nil, true
	}
	if consumed {
		return nil, true
	}
	if hadPending {
		return e.bindingKey(k) // chord missed: retry stroke standalone
	}
	return nil, false
}

// sessionKB is the process-level copy of the loaded keymap — modal
// surfaces (transcript view, pickers) resolve remaps through it without
// holding the editor.
var sessionKB atomic.Value // map[string]string

// dialogAction resolves a key in a named context — first the context's
// table, then Global. Returns the action name or "".
func dialogAction(ctx string, k key) string {
	kb, _ := sessionKB.Load().(map[string]string)
	if kb == nil {
		return ""
	}
	s := keystroke(k)
	if s == "" {
		return ""
	}
	if a, ok := kb[ctx+"\x00"+s]; ok {
		return a
	}
	return kb["Global\x00"+s]
}

// maybeReloadKB is the live watcher — real watches keybindings.json and
// hot-reloads; we stat it on the key path (throttled ~1s) and rebuild the
// binding map on mtime change. sessionKB gets the fresh map through the
// same loadKeybindings path, so dialogs see the remap immediately.
func (e *editor) maybeReloadKB() {
	if time.Since(e.kbLastCheck) < time.Second {
		return
	}
	e.kbLastCheck = time.Now()
	fi, err := os.Stat(filepath.Join(config.ClaudeDir(), "keybindings.json"))
	if err != nil {
		return
	}
	if e.kbMtime.IsZero() {
		e.kbMtime = fi.ModTime()
		return // seeded at load — no reload on the first stat
	}
	if fi.ModTime().After(e.kbMtime) {
		e.kbMtime = fi.ModTime()
		e.loadKeybindings()
	}
}
