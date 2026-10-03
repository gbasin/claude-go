package api

import "encoding/json"

// Wire types matching the Anthropic Messages API as used by Claude Code 2.1.288.

type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or []ContentBlock
	// output_config may appear on turn-scoped system messages (per-turn control).
	OutputConfig json.RawMessage `json:"output_config,omitempty"`
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
	// redacted_thinking carries opaque data that MUST round-trip verbatim
	Data string `json:"data,omitempty"`
	// citations, encrypted_content etc. round-trip via passthrough
	Citations        json.RawMessage `json:"citations,omitempty"`
	EncryptedContent json.RawMessage `json:"encrypted_content,omitempty"`
	EncryptedIndex   json.RawMessage `json:"encrypted_index,omitempty"`
	Container        json.RawMessage `json:"container,omitempty"`
	Display          string          `json:"display,omitempty"`
	// cache_control on system/tool blocks
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

type CacheControl struct {
	Type  string `json:"type"`
	TTL   string `json:"ttl,omitempty"`
	Scope string `json:"scope,omitempty"`
}

type SystemBlock struct {
	Type         string        `json:"type"`
	Text         string        `json:"text"`
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
	// Raw, when set, is the complete wire object (server-side tools like
	// web_search_20250305 have no input_schema and carry "type").
	Raw json.RawMessage `json:"-"`
}

func (t Tool) MarshalJSON() ([]byte, error) {
	if len(t.Raw) > 0 {
		return t.Raw, nil
	}
	type plain Tool
	return json.Marshal(plain(t))
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
	InputTokens              int    `json:"input_tokens"`
	OutputTokens             int    `json:"output_tokens"`
	CacheCreationInputTokens int    `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int    `json:"cache_read_input_tokens"`
	Iterations               int    `json:"iterations,omitempty"`
	InferenceGeo             string `json:"inference_geo,omitempty"`
	ServiceTier              string `json:"service_tier,omitempty"`
	Speed                    string `json:"speed,omitempty"`
	OutputTokensDetails      *struct {
		ThinkingTokens int `json:"thinking_tokens,omitempty"`
	} `json:"output_tokens_details,omitempty"`
	CacheCreation *struct {
		Ephemeral1hInputTokens int `json:"ephemeral_1h_input_tokens,omitempty"`
		Ephemeral5mInputTokens int `json:"ephemeral_5m_input_tokens,omitempty"`
	} `json:"cache_creation,omitempty"`
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
