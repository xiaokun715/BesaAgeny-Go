// Package domain 是与厂商无关的领域类型。
//
// 它是 provider / gateway / agent / context 共同的词汇表，但**不依赖其中任何一个**。
// 厂商形状的差异收敛在 internal/provider 的适配器里。
package domain

import (
	"encoding/json"
	"strings"
)

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

func (r Role) Valid() bool {
	switch r {
	case RoleSystem, RoleUser, RoleAssistant, RoleTool:
		return true
	}
	return false
}

type ContentPart struct {
	Text      string `json:"text,omitempty"`
	ImageURL  string `json:"image_url,omitempty"`
	ImageData string `json:"image_data,omitempty"`
	MediaType string `json:"media_type,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

// Message 是发给模型的一条消息。
//
// ToolCalls 只出现在 assistant 消息上；ToolCallID 只出现在 tool 消息上。
// 二者必须成对回填 —— 多数厂商在配对缺失时直接 400。
type Message struct {
	Role       Role          `json:"role"`
	Content    string        `json:"content,omitempty"`
	Parts      []ContentPart `json:"parts,omitempty"`
	Name       string        `json:"name,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall    `json:"tool_calls,omitempty"`
}

func TextMessage(role Role, text string) Message { return Message{Role: role, Content: text} }
func System(text string) Message                 { return TextMessage(RoleSystem, text) }
func User(text string) Message                   { return TextMessage(RoleUser, text) }
func ToolResult(callID, text string) Message {
	return Message{Role: RoleTool, ToolCallID: callID, Content: text}
}
func AssistantCalls(calls []ToolCall, text string) Message {
	return Message{Role: RoleAssistant, Content: text, ToolCalls: calls}
}

func (m Message) Empty() bool {
	return m.Content == "" && len(m.Parts) == 0 && len(m.ToolCalls) == 0
}

// HasImage 供能力路由判断：有图片就必须选支持 vision 的模型。
func (m Message) HasImage() bool {
	for _, p := range m.Parts {
		if p.ImageURL != "" || p.ImageData != "" {
			return true
		}
	}
	return false
}

type ToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type ToolCall struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	ArgumentsRaw string         `json:"arguments_raw,omitempty"`
	Arguments    map[string]any `json:"arguments,omitempty"`
}

// ParseArguments 解析模型给出的参数；解析失败时保留原始串供排障。
func ParseArguments(raw string) (map[string]any, error) {
	if strings.TrimSpace(raw) == "" {
		return map[string]any{}, nil
	}
	out := map[string]any{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ToolSpecsOf 把工具定义转成模型可见的形状。
func ToolSpecsOf(defs []ToolDefinition) []ToolSpec {
	out := make([]ToolSpec, 0, len(defs))
	for _, d := range defs {
		out = append(out, ToolSpec{Name: d.Name, Description: d.Description, Parameters: d.Parameters})
	}
	return out
}

// ToolDefinition 是“给模型看的工具说明”，定义是数据、工具是行为。
// 放在 domain 是为了让 agent/context 不必 import tool 包。
type ToolDefinition struct {
	Name        string
	Description string
	Parameters  map[string]any
	SideEffect  string
	Category    string
}
