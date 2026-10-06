package repl

import (
	"regexp"
	"strings"
)

// Minimal zero-dep syntax highlighter for fenced code and diffs — real CC
// ships full grammars; this covers the common cases (comments, strings,
// keywords, numbers, diffs) for the usual languages.

var langKeywords = map[string][]string{
	"go": {"break", "case", "chan", "const", "continue", "default", "defer",
		"else", "fallthrough", "for", "func", "go", "goto", "if", "import",
		"interface", "map", "package", "range", "return", "select", "struct",
		"switch", "type", "var", "nil", "true", "false"},
	"python": {"and", "as", "assert", "async", "await", "break", "class",
		"continue", "def", "del", "elif", "else", "except", "finally", "for",
		"from", "global", "if", "import", "in", "is", "lambda", "nonlocal",
		"not", "or", "pass", "raise", "return", "try", "while", "with",
		"yield", "None", "True", "False", "self"},
	"py": {}, // alias resolved below
	"js": {"async", "await", "break", "case", "catch", "class", "const",
		"continue", "debugger", "default", "delete", "do", "else", "export",
		"extends", "finally", "for", "from", "function", "if", "import", "in",
		"instanceof", "let", "new", "of", "return", "static", "super",
		"switch", "this", "throw", "try", "typeof", "var", "void", "while",
		"with", "yield", "null", "undefined", "true", "false"},
	"ts":  {},
	"tsx": {},
	"jsx": {},
	"rust": {"as", "async", "await", "break", "const", "continue", "crate",
		"dyn", "else", "enum", "extern", "false", "fn", "for", "if", "impl",
		"in", "let", "loop", "match", "mod", "move", "mut", "pub", "ref",
		"return", "self", "Self", "static", "struct", "super", "trait",
		"true", "type", "unsafe", "use", "where", "while"},
	"sh": {"case", "do", "done", "elif", "else", "esac", "fi", "for",
		"function", "if", "in", "then", "until", "while", "echo", "exit",
		"return", "local", "export", "source"},
	"bash": {}, "zsh": {}, "shell": {},
	"json": {"true", "false", "null"},
	"c": {"auto", "break", "case", "char", "const", "continue", "default",
		"do", "double", "else", "enum", "extern", "float", "for", "goto",
		"if", "inline", "int", "long", "register", "restrict", "return",
		"short", "signed", "sizeof", "static", "struct", "switch", "typedef",
		"union", "unsigned", "void", "volatile", "while"},
	"cpp": {}, "cc": {}, "c++": {},
	"java": {"abstract", "boolean", "break", "byte", "case", "catch",
		"char", "class", "continue", "default", "do", "double", "else",
		"enum", "extends", "final", "finally", "float", "for", "if",
		"implements", "import", "instanceof", "int", "interface", "long",
		"native", "new", "package", "private", "protected", "public",
		"return", "short", "static", "super", "switch", "synchronized",
		"this", "throw", "throws", "try", "void", "volatile", "while"},
	"yaml": {"true", "false", "null", "yes", "no"},
}

// token colors — reuse palette vars
// Real's "Monokai Extended" dark syntax palette (mod_1275.js).
var (
	hlKeyword = "\x1b[38;2;249;38;114m"  // keyword/operator — hot pink
	hlString  = "\x1b[38;2;230;219;116m" // string/regexp — yellow
	hlComment = "\x1b[38;2;117;113;94m"  // comment/meta — gray
	hlNumber  = "\x1b[38;2;190;132;255m" // literal/number — purple
	hlBuiltin = "\x1b[38;2;166;226;46m"  // built_in/type — lime
	hlDiffAdd = "\x1b[38;2;105;219;124m" // diffAdded token
	hlDiffDel = "\x1b[38;2;255;168;180m" // diffRemoved token
	hlDiffCtx = "\x1b[2m"
	hlParam   = "\x1b[38;2;253;151;31m"  // params — orange
	hlPunct   = "\x1b[38;2;248;248;242m" // punctuation — off-white
)

func init() {
	// fill aliases
	langKeywords["py"] = langKeywords["python"]
	langKeywords["ts"] = langKeywords["js"]
	langKeywords["tsx"] = langKeywords["js"]
	langKeywords["jsx"] = langKeywords["js"]
	langKeywords["bash"] = langKeywords["sh"]
	langKeywords["zsh"] = langKeywords["sh"]
	langKeywords["shell"] = langKeywords["sh"]
	langKeywords["cpp"] = langKeywords["c"]
	langKeywords["cc"] = langKeywords["c"]
	langKeywords["c++"] = langKeywords["c"]
}

var (
	reNumber  = regexp.MustCompile(`\b\d[\d_]*(\.\d+)?([eE][+-]?\d+)?\b`)
	reWord    = regexp.MustCompile(`\b[A-Za-z_][A-Za-z0-9_]*\b`)
	reSlashCm = regexp.MustCompile(`//.*$|/\*.*?\*/`)
	reHashCm  = regexp.MustCompile(`#.*$`)
	reString  = regexp.MustCompile("`[^`]*`|'(?:[^'\\\\]|\\\\.)*'|\"(?:[^\"\\\\]|\\\\.)*\"")
)

// highlightLine colorizes one line of code in lang; returns "" if the lang
// is unknown (caller emits plain).
func highlightLine(line, lang string) string {
	lang = strings.ToLower(strings.TrimSpace(lang))
	// diff-style lines always highlight regardless of lang
	if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "++") {
		return hlDiffAdd + line + reset
	}
	if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "--") {
		return hlDiffDel + line + reset
	}
	if strings.HasPrefix(line, "@@") {
		return hlBuiltin + line + reset
	}
	kws, ok := langKeywords[lang]
	if !ok {
		return ""
	}
	// shell/python comments use #; c-family uses //
	hashComment := lang == "python" || lang == "py" || lang == "sh" ||
		lang == "bash" || lang == "zsh" || lang == "shell" || lang == "yaml"

	kwSet := map[string]bool{}
	for _, k := range kws {
		kwSet[k] = true
	}

	var out strings.Builder
	i := 0
	for i < len(line) {
		// comment to EOL
		if strings.HasPrefix(line[i:], "//") && !hashComment {
			out.WriteString(hlComment + line[i:] + reset)
			break
		}
		if line[i] == '#' && hashComment {
			out.WriteString(hlComment + line[i:] + reset)
			break
		}
		// string literal
		if m := reString.FindStringIndex(line[i:]); m != nil && m[0] == 0 {
			out.WriteString(hlString + line[i:i+m[1]] + reset)
			i += m[1]
			continue
		}
		// word → keyword/number/plain
		if m := reWord.FindStringIndex(line[i:]); m != nil && m[0] == 0 {
			w := line[i : i+m[1]]
			switch {
			case kwSet[w]:
				out.WriteString(hlKeyword + w + reset)
			case reNumber.MatchString(w):
				out.WriteString(hlNumber + w + reset)
			default:
				out.WriteString(w)
			}
			i += m[1]
			continue
		}
		out.WriteByte(line[i])
		i++
	}
	return out.String()
}
