package api

import "encoding/json"

// Wire types matching the Anthropic Messages API as used by Claude Code 2.1.288.

type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or []ContentBlock
}

func TextMessage(role, text string) Message {
	b, _ := json.Marshal([]ContentBlock{{Type: "text", Text: text}})
	return Message{Role: role, Content: b}
}

func BlocksMessage(role string, blocks []ContentBlock) Message {
	b, _ := json.Marshal(blocks)
	return Message{Role: role, Content: b}
}

type ContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"` // tool_result content
	IsError   bool            `json:"is_error,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	Source    *ImageSource    `json:"source,omitempty"`
	// cache_control on system/tool blocks
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

type CacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

type SystemBlock struct {
	Type         string        `json:"type"`
	Text         string        `json:"text"`
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type Request struct {
	Model             string          `json:"model"`
	MaxTokens         int             `json:"max_tokens"`
	Messages          []Message       `json:"messages"`
	System            []SystemBlock   `json:"system,omitempty"`
	Tools             []Tool          `json:"tools,omitempty"`
	Metadata          json.RawMessage `json:"metadata,omitempty"`
	Thinking          json.RawMessage `json:"thinking,omitempty"`
	ContextManagement json.RawMessage `json:"context_management,omitempty"`
	Safeguards        json.RawMessage `json:"safeguards,omitempty"`
	OutputConfig      json.RawMessage `json:"output_config,omitempty"`
	Stream            bool            `json:"stream"`
}

// SSE event types from stream=true
type StreamEvent struct {
	Type         string        `json:"type"`
	Index        int           `json:"index,omitempty"`
	ContentBlock *ContentBlock `json:"content_block,omitempty"`
	Delta        *Delta        `json:"delta,omitempty"`
	Message      *Response     `json:"message,omitempty"`
	Usage        *Usage        `json:"usage,omitempty"`
	Error        *APIError     `json:"error,omitempty"`
}

type Delta struct {
	Type         string `json:"type"`
	Text         string `json:"text,omitempty"`
	PartialJSON  string `json:"partial_json,omitempty"`
	Thinking     string `json:"thinking,omitempty"`
	Signature    string `json:"signature,omitempty"`
	StopReason   string `json:"stop_reason,omitempty"`
	StopSequence string `json:"stop_sequence,omitempty"`
}

type Response struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"`
	Role         string         `json:"role"`
	Content      []ContentBlock `json:"content"`
	Model        string         `json:"model"`
	StopReason   string         `json:"stop_reason"`
	StopSequence string         `json:"stop_sequence"`
	Usage        Usage          `json:"usage"`
}

type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

type APIError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type ImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}
