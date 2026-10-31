package openai

import (
	"encoding/json"

	"github.com/robdevops/signalbot/internal/llm"
)

// message is an llm.Message in the API's JSON.
type message llm.Message

// MarshalJSON sends a message with images as content parts: the text, then
// each image.
func (m message) MarshalJSON() ([]byte, error) {
	plain := struct {
		Role       string         `json:"role"`
		Content    any            `json:"content"`
		ToolCalls  []llm.ToolCall `json:"tool_calls,omitempty"`
		ToolCallID string         `json:"tool_call_id,omitempty"`
	}{m.Role, m.Content, m.ToolCalls, m.ToolCallID}
	if len(m.Images) > 0 {
		parts := []any{map[string]string{"type": "text", "text": m.Content}}
		for _, u := range m.Images {
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]string{"url": u}})
		}
		plain.Content = parts
	}
	return json.Marshal(plain)
}
