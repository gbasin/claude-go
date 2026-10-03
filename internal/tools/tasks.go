package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"

	"claude-go/internal/api"
)

// Session task list + todos — TaskCreate/Get/Update/List and TodoWrite.
// Real tools are flag-gated; they activate via CLAUDE_CODE_TASK_TOOLS=1 or
// settings.tasks.enabled. Task state persists per-session under the project
// dir so --resume sees it; todos are in-memory like real 2.1.288.

func taskToolsEnabled() bool {
	if os.Getenv("CLAUDE_CODE_TASK_TOOLS") == "1" || os.Getenv("CLAUDE_CODE_TASK_LIST_ID") != "" {
		return true
	}
	return settingStr(os.Getenv("PWD"), "tasks.enabled") == "true"
}

func init() {
	if !taskToolsEnabled() {
		return
	}
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
	return fmt.Sprintf("t%d", taskSeq)
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
	tasksList = append(tasksList, t)
	tasksMu.Unlock()
	out, _ := json.Marshal(map[string]any{"task": map[string]string{"id": t.ID, "subject": t.Subject}})
	return Result{Content: string(out)}
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
		return Result{Content: fmt.Sprintf(`{"task":null} — no task with id %q`, in.TaskID), IsError: true}
	}
	out, _ := json.Marshal(map[string]any{"task": t})
	return Result{Content: string(out)}
}

func execTaskList(c *Context, raw json.RawMessage) Result {
	tasksMu.Lock()
	defer tasksMu.Unlock()
	vis := []SessionTask{}
	for _, t := range tasksList {
		if t.Status != "deleted" {
			vis = append(vis, t)
		}
	}
	out, _ := json.Marshal(map[string]any{"tasks": vis})
	return Result{Content: string(out)}
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
	var statusChange map[string]string
	if in.Subject != nil {
		t.Subject = *in.Subject
		fields = append(fields, "subject")
	}
	if in.Desc != nil {
		t.Description = *in.Desc
		fields = append(fields, "description")
	}
	if in.ActiveForm != nil {
		t.ActiveForm = *in.ActiveForm
		fields = append(fields, "activeForm")
	}
	if in.Status != nil {
		from := t.Status
		t.Status = *in.Status
		fields = append(fields, "status")
		statusChange = map[string]string{"from": from, "to": *in.Status}
	}
	t.Blocks = append(t.Blocks, in.AddBlocks...)
	t.BlockedBy = append(t.BlockedBy, in.AddBlockedBy...)
	if in.Owner != nil {
		t.Owner = *in.Owner
		fields = append(fields, "owner")
	}
	for k, v := range in.Metadata {
		if t.Metadata == nil {
			t.Metadata = map[string]any{}
		}
		t.Metadata[k] = v
		fields = append(fields, "metadata")
	}
	out, _ := json.Marshal(map[string]any{
		"success": true, "taskId": t.ID, "updatedFields": fields, "statusChange": statusChange})
	return Result{Content: string(out)}
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
	out, _ := json.Marshal(map[string]any{"oldTodos": old, "newTodos": in.Todos})
	return Result{Content: string(out)}
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
