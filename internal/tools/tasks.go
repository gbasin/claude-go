package tools

import (
	"encoding/json"
	"fmt"
	"github.com/gbasin/claude-go/internal/config"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/gbasin/claude-go/internal/api"
)

// Session task list + todos — TaskCreate/Get/Update/List and TodoWrite.
// Real tools are flag-gated; they activate via CLAUDE_CODE_TASK_TOOLS=1 or
// settings.tasks.enabled. Task state persists per-session under the project
// dir so --resume sees it; todos are in-memory like real 2.1.288.

// taskToolsEnabledFor checks the settings gate against a specific project
// dir (ProjectDir below) so --project/--add-dir moves resolve
// tasks.enabled against the actual workspace, not the parent's launch dir.
// The env vars (CLAUDE_CODE_TASK_TOOLS / CLAUDE_CODE_TASK_LIST_ID) force on.
func taskToolsEnabledFor(cwd string) bool {
	if os.Getenv("CLAUDE_CODE_TASK_TOOLS") == "1" || os.Getenv("CLAUDE_CODE_TASK_LIST_ID") != "" {
		return true
	}
	if s := settingStr(cwd, "tasks.enabled"); s == "true" {
		return true
	}
	return settingBool(cwd, "tasks.enabled")
}

// projectDir is the session's workspace, set when the loop builds its
// Context — the settings gate resolves against this, not PWD (the parent
// may launch from elsewhere with --project).
var projectDir atomic.Value // stores string

// SetProjectDir records the workspace for settings gates.
func SetProjectDir(cwd string) { projectDir.Store(cwd) }

// ProjectDir returns the session workspace ("" until the loop starts).
func ProjectDir() string {
	s, _ := projectDir.Load().(string)
	return s
}

func init() {
	// register unconditionally — the wire surface (All) and Dispatch gate
	// on taskToolsEnabledFor(ProjectDir()) at call time, since launch $PWD
	// isn't the project dir under --project/--add-dir
	Register(&Tool{Def: taskDef("TaskCreate", `Create a structured task list item for your current session.`,
		obj(
			prop("subject", "string", "A brief title for the task"),
			prop("description", "string", "What needs to be done"),
			propO("activeForm", "string", `Present continuous form shown in spinner when in_progress (e.g., "Running tests")`),
			propO("metadata", "object", "Arbitrary metadata to attach to the task"),
		), []string{"subject", "description"}), Exec: execTaskCreate})
	Register(&Tool{Def: taskDef("TaskGet", `Retrieve a task by ID.`,
		obj(prop("taskId", "string", "The ID of the task to retrieve")), []string{"taskId"}), Exec: execTaskGet})
	Register(&Tool{Def: taskDef("TaskList", `List all tasks.`,
		obj(), nil), Exec: execTaskList})
	Register(&Tool{Def: taskDef("TaskUpdate", `Update a task's status, subject, dependencies or owner.`,
		obj(
			prop("taskId", "string", "The ID of the task to update"),
			propO("subject", "string", "New subject for the task"),
			propO("description", "string", "New description for the task"),
			propO("activeForm", "string", "Present continuous form for spinner"),
			en("status", "New status for the task", "pending", "in_progress", "completed", "deleted"),
			propO("addBlocks", "array", "Task IDs that this task blocks"),
			propO("addBlockedBy", "array", "Task IDs that block this task"),
			propO("owner", "string", "New owner for the task"),
			propO("metadata", "object", "Metadata to merge"),
		), []string{"taskId"}), Exec: execTaskUpdate})
	todoItem := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"content":    map[string]any{"type": "string", "description": "The todo item content"},
			"status":     map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "completed"}},
			"activeForm": map[string]any{"type": "string", "description": "Present continuous form (e.g. 'Running tests')"},
		},
		"required": []string{"content", "status", "activeForm"},
	}
	todoSchema, _ := json.Marshal(map[string]any{"type": "object",
		"properties": map[string]any{"todos": map[string]any{"type": "array", "items": todoItem}},
		"required":   []string{"todos"}})
	Register(&Tool{Def: api.Tool{Name: "TodoWrite", Description: "Manage the session task checklist.",
		InputSchema: todoSchema}, Exec: execTodoWrite})
}

// ---- schema helpers ----

func prop(name, typ, desc string) map[string]any {
	return map[string]any{"type": typ, "description": desc, "__name": name}
}
func propO(name, typ, desc string) map[string]any { return prop(name, typ, desc) }
func propArr(name, typ, desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": typ}, "description": desc, "__name": name}
}
func en(name, desc string, vals ...string) map[string]any {
	return map[string]any{"type": "string", "enum": vals, "description": desc, "__name": name}
}
func obj(props ...map[string]any) map[string]any {
	p := map[string]any{}
	for _, pr := range props {
		p[pr["__name"].(string)] = deleteName(pr)
	}
	return map[string]any{"type": "object", "properties": p}
}
func deleteName(m map[string]any) map[string]any {
	delete(m, "__name")
	return m
}
func taskDef(name, desc string, schema map[string]any, required []string) api.Tool {
	if required != nil {
		schema["required"] = required
	}
	b, _ := json.Marshal(schema)
	return api.Tool{Name: name, Description: desc, InputSchema: b}
}

// ---- session task store ----

type SessionTask struct {
	ID          string         `json:"id"`
	Subject     string         `json:"subject"`
	Description string         `json:"description"`
	ActiveForm  string         `json:"activeForm,omitempty"`
	Status      string         `json:"status"` // pending|in_progress|completed|deleted
	Owner       string         `json:"owner,omitempty"`
	Blocks      []string       `json:"blocks,omitempty"`
	BlockedBy   []string       `json:"blockedBy,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

var (
	tasksMu   sync.Mutex
	tasksList []SessionTask
	taskSeq   int
)

func nextTaskID() string {
	taskSeq++
	return fmt.Sprintf("%d", taskSeq) // real IDs are numeric ("Task #N")
}

func findTask(id string) *SessionTask {
	for i := range tasksList {
		if tasksList[i].ID == id {
			return &tasksList[i]
		}
	}
	return nil
}

func execTaskCreate(c *Context, raw json.RawMessage) Result {
	var in struct {
		Subject    string         `json:"subject"`
		Desc       string         `json:"description"`
		ActiveForm string         `json:"activeForm"`
		Metadata   map[string]any `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{Content: "invalid input: " + err.Error(), IsError: true}
	}
	if in.Subject == "" || in.Desc == "" {
		return Result{Content: "subject and description are required", IsError: true}
	}
	tasksMu.Lock()
	t := SessionTask{ID: nextTaskID(), Subject: in.Subject, Description: in.Desc,
		ActiveForm: in.ActiveForm, Status: "pending", Metadata: in.Metadata}
	// real's TaskCreated hook — exit 2 prevents creation (stderr to model)
	if c.FireHook != nil {
		if h := c.FireHook("TaskCreated", map[string]any{
			"task_id": t.ID, "task_subject": t.Subject,
			"task_description": t.Description,
		}); h.Block {
			tasksMu.Unlock()
			return Result{Content: "Task creation blocked by TaskCreated hook: " + h.Reason, IsError: true}
		}
	}
	tasksList = append(tasksList, t)
	persistTasks(c)
	tasksMu.Unlock()
	return Result{Content: fmt.Sprintf("Task #%s created successfully: %s", t.ID, t.Subject)}
}

func execTaskGet(c *Context, raw json.RawMessage) Result {
	var in struct {
		TaskID string `json:"taskId"`
	}
	json.Unmarshal(raw, &in)
	tasksMu.Lock()
	defer tasksMu.Unlock()
	t := findTask(in.TaskID)
	if t == nil {
		return Result{Content: "Task not found", IsError: true}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Task #%s: %s\nStatus: %s\nDescription: %s\n", t.ID, t.Subject, t.Status, t.Description)
	if len(t.BlockedBy) > 0 {
		fmt.Fprintf(&b, "Blocked by: #%s\n", strings.Join(t.BlockedBy, ", #"))
	}
	if len(t.Blocks) > 0 {
		fmt.Fprintf(&b, "Blocks: #%s\n", strings.Join(t.Blocks, ", #"))
	}
	if t.Owner != "" {
		fmt.Fprintf(&b, "Owner: %s\n", t.Owner)
	}
	return Result{Content: strings.TrimRight(b.String(), "\n")}
}

func execTaskList(c *Context, raw json.RawMessage) Result {
	tasksMu.Lock()
	defer tasksMu.Unlock()
	if len(tasksList) == 0 {
		return Result{Content: "No tasks yet."}
	}
	var b strings.Builder
	for _, t := range tasksList {
		if t.Status == "deleted" {
			continue
		}
		fmt.Fprintf(&b, "#%s ☐ %s", t.ID, t.Subject)
		if t.Status == "in_progress" {
			b.WriteString(" ← in progress")
		} else if t.Status == "completed" {
			b.WriteString(" (completed)")
		}
		if t.Owner != "" {
			fmt.Fprintf(&b, " · owner %s", t.Owner)
		}
		b.WriteString("\n")
	}
	return Result{Content: strings.TrimRight(b.String(), "\n")}
}

func execTaskUpdate(c *Context, raw json.RawMessage) Result {
	var in struct {
		TaskID       string         `json:"taskId"`
		Subject      *string        `json:"subject"`
		Desc         *string        `json:"description"`
		ActiveForm   *string        `json:"activeForm"`
		Status       *string        `json:"status"`
		AddBlocks    []string       `json:"addBlocks"`
		AddBlockedBy []string       `json:"addBlockedBy"`
		Owner        *string        `json:"owner"`
		Metadata     map[string]any `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{Content: "invalid input: " + err.Error(), IsError: true}
	}
	tasksMu.Lock()
	defer tasksMu.Unlock()
	t := findTask(in.TaskID)
	if t == nil {
		return Result{Content: fmt.Sprintf("no task with id %q", in.TaskID), IsError: true}
	}
	var fields []string
	completed := false
	if in.Subject != nil && t.Subject != *in.Subject {
		t.Subject = *in.Subject
		fields = append(fields, "subject")
	}
	if in.Desc != nil && t.Description != *in.Desc {
		t.Description = *in.Desc
		fields = append(fields, "description")
	}
	if in.ActiveForm != nil && t.ActiveForm != *in.ActiveForm {
		t.ActiveForm = *in.ActiveForm
		fields = append(fields, "activeForm")
	}
	if in.Status != nil && t.Status != *in.Status {
		t.Status = *in.Status
		fields = append(fields, "status")
		completed = *in.Status == "completed"
	}
	for _, id := range in.AddBlocks {
		if !strSliceHas(t.Blocks, id) {
			t.Blocks = append(t.Blocks, id)
			fields = append(fields, "blocks")
		}
	}
	for _, id := range in.AddBlockedBy {
		if !strSliceHas(t.BlockedBy, id) {
			t.BlockedBy = append(t.BlockedBy, id)
			fields = append(fields, "blockedBy")
		}
	}
	if in.Owner != nil && t.Owner != *in.Owner {
		t.Owner = *in.Owner
		fields = append(fields, "owner")
	}
	for k, v := range in.Metadata {
		if t.Metadata == nil {
			t.Metadata = map[string]any{}
		}
		if v == nil {
			delete(t.Metadata, k) // real: null deletes the key
		} else {
			t.Metadata[k] = v
		}
		fields = append(fields, "metadata")
	}
	persistTasks(c)
	msg := fmt.Sprintf("Updated task #%s", t.ID)
	if len(fields) > 0 {
		msg += " " + strings.Join(fields, ", ")
	}
	if completed {
		msg += "\n\nTask completed. Call TaskList now to see what's ready to work on next."
	}
	return Result{Content: msg}
}

// ---- todos (in-memory per real semantics) ----

type todo struct {
	Content    string `json:"content"`
	Status     string `json:"status"` // pending|in_progress|completed
	ActiveForm string `json:"activeForm"`
}

var (
	todosMu   sync.Mutex
	todosList []todo
)

func execTodoWrite(c *Context, raw json.RawMessage) Result {
	var in struct {
		Todos []todo `json:"todos"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{Content: "invalid input: " + err.Error(), IsError: true}
	}
	for _, t := range in.Todos {
		switch t.Status {
		case "pending", "in_progress", "completed":
		default:
			return Result{Content: fmt.Sprintf("invalid todo status %q", t.Status), IsError: true}
		}
	}
	todosMu.Lock()
	old := todosList
	// real clears the list once all items complete
	allDone := len(in.Todos) > 0
	for _, t := range in.Todos {
		if t.Status != "completed" {
			allDone = false
		}
	}
	if allDone {
		todosList = nil
	} else {
		todosList = in.Todos
	}
	todosMu.Unlock()
	_ = old
	return Result{Content: "Todos have been modified successfully. Ensure that you continue to use the todo list to track your progress. Please proceed with the current tasks if applicable"}
}

// TodosText renders the live todo list for the REPL status area.
func TodosText() string {
	todosMu.Lock()
	defer todosMu.Unlock()
	if len(todosList) == 0 {
		return ""
	}
	var b strings.Builder
	for _, t := range todosList {
		mark := "○"
		if t.Status == "in_progress" {
			mark = "◐"
		} else if t.Status == "completed" {
			mark = "●"
		}
		fmt.Fprintf(&b, "%s %s\n", mark, t.Content)
	}
	return strings.TrimRight(b.String(), "\n")
}

// persistTasks writes one <id>.json per task + a .highwatermark sequence file —
// the real ~/.claude/tasks/<sid>/ layout. tasks.json is kept as a merged
// index for older readers.
func persistTasks(c *Context) {
	dir := filepath.Join(config.ClaudeDir(), "tasks", c.Session)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	var maxID int
	for _, t := range tasksList {
		b, _ := json.MarshalIndent(t, "", "  ")
		os.WriteFile(filepath.Join(dir, t.ID+".json"), b, 0o644)
		if n, _ := strconv.Atoi(t.ID); n > maxID {
			maxID = n
		}
	}
	os.WriteFile(filepath.Join(dir, ".highwatermark"), []byte(strconv.Itoa(maxID)), 0o644)
	b, _ := json.Marshal(tasksList)
	os.WriteFile(filepath.Join(dir, "tasks.json"), b, 0o644)
}

func strSliceHas(l []string, s string) bool {
	for _, v := range l {
		if v == s {
			return true
		}
	}
	return false
}
