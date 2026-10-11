// Package chattools brokers read-only chat-platform tool calls from the Pi
// extension to pi-chat's durable session bindings and platform adapters.
package chattools

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/tigersoldier/pi-chat/internal/store"
)

const (
	ProtocolVersion   = 1
	ExtensionVersion  = "v1"
	HealthCommand     = "pi-chat-tools-health"
	HealthDescription = "pi-chat-tools:" + ExtensionVersion

	ToolReadThread  = "chat_read_thread"
	ToolReadMessage = "chat_read_message"
	ToolGetUser     = "chat_get_user"
)

// ProviderKey selects an adapter and credential surface from the stored
// session binding, never from model-controlled arguments.
type ProviderKey struct {
	Provider    string
	Surface     string
	WorkspaceID string
}

// Provider executes one allowlisted read operation against the bound app
// installation. Implementations must validate platform references and return
// sanitized, bounded text without logging its contents.
type Provider interface {
	Call(ctx context.Context, binding store.SessionToolBinding, tool string, arguments json.RawMessage) (ToolResult, error)
}

// ToolResult is compact text suitable for the Pi tool result.
type ToolResult struct {
	Text      string
	Truncated bool
}

// CallError carries a safe public error for the model. The private cause is
// intentionally omitted from its Error string and from broker responses.
type CallError struct {
	Code    string
	Message string
	Cause   error
}

func (e *CallError) Error() string {
	if e == nil {
		return "chat tool call failed"
	}
	return e.Message
}

func toolCapability(tool string) (string, bool) {
	switch tool {
	case ToolReadThread:
		return "read_thread", true
	case ToolReadMessage:
		return "read_message", true
	case ToolGetUser:
		return "read_user", true
	default:
		return "", false
	}
}

func bindingAllows(binding store.SessionToolBinding, capability string) bool {
	return slices.Contains(binding.Capabilities, capability)
}

type request struct {
	Version     int             `json:"version"`
	SessionPath string          `json:"sessionPath"`
	CallID      string          `json:"callId"`
	Tool        string          `json:"tool"`
	Arguments   json.RawMessage `json:"arguments"`
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type wireError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type response struct {
	OK        bool          `json:"ok"`
	CallID    string        `json:"callId,omitempty"`
	Content   []textContent `json:"content,omitempty"`
	Truncated bool          `json:"truncated,omitempty"`
	Error     *wireError    `json:"error,omitempty"`
}
