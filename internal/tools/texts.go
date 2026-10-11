package tools

import (
	"encoding/json"

	"github.com/gbasin/claude-go/internal/api"
	"github.com/gbasin/claude-go/internal/extract"
)

// schemasJSON returns the builtin tool schemas: the harvested
// tools.json when an extraction exists, else the community fallback —
// every tool name present with a minimal permissive schema (no
// Anthropic-authored descriptions are vendored in this repo).
func schemasJSON() []byte {
	if b, ok := extract.JSON("tools.json"); ok {
		return b
	}
	return fallbackSchemasJSON()
}

// fallbackSchemaNames is the wire tool name set — functional identifiers
// that callers and the wire format already depend on.
var fallbackSchemaNames = []string{
	"Agent", "Bash", "CronCreate", "CronDelete", "CronList", "DesignSync",
	"Edit", "EnterWorktree", "ExitWorktree", "ListAgents",
	"ListMcpResourcesTool", "LSP", "Monitor", "NotebookEdit",
	"PushNotification", "Read", "ReadMcpResourceDirTool",
	"ReadMcpResourceTool", "RemoteTrigger", "ReportFindings",
	"ScheduleWakeup", "SendMessage", "Skill", "TaskStop", "WebFetch",
	"WebSearch", "Workflow", "Write", "Artifact", "ArtifactComments",
	"ArtifactData", "AskUserQuestion", "EndConversation", "EnterPlanMode",
	"ExitPlanMode", "SendFeedback", "ReadNotifications",
}

var fallbackSchemasJSON = func() func() []byte {
	var all []api.Tool
	for _, n := range fallbackSchemaNames {
		all = append(all, api.Tool{
			Name:        n,
			Description: "claude-go tool " + n,
			InputSchema: json.RawMessage(`{"type":"object"}`),
		})
	}
	b, _ := json.Marshal(all)
	return func() []byte { return b }
}()
