// Package mcp ports services/mcp_service: a streamable-HTTP MCP server
// exposing the Macro tool catalog to external MCP clients. The Rust
// implementation served `ai_tools::tools_for(AiHost::Mcp)` through rmcp;
// here the tool surface is abstracted behind ToolCatalog + ToolCaller ports
// so the Go toolset port can drop in without touching transport.
package mcp

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ToolSpec describes one MCP tool on the wire (mirrors the rmcp Tool +
// ToolAnnotations built from ai_toolset definitions).
type ToolSpec struct {
	Name        string
	Title       string
	Description string
	// InputSchema must marshal to a JSON Schema object (2020-12 draft, since
	// the go-sdk validates against it).
	InputSchema map[string]any
	// Annotations mirror ai_toolset::ToolAnnotations.
	ReadOnly    bool
	Destructive *bool
	Idempotent  bool
	OpenWorld   *bool
}

// ToolCatalog lists the tools the MCP server advertises (replaces
// AsyncToolCollection::tools).
type ToolCatalog interface {
	Tools(ctx context.Context) []ToolSpec
}

// ToolCaller executes a tool call for an authenticated user (replaces
// AsyncToolCollection::try_tool_call).
type ToolCaller interface {
	CallTool(ctx context.Context, userID, toolName string, arguments json.RawMessage) (*mcp.CallToolResult, error)
}

// ---------------------------------------------------------------------------
// Static catalog — the documented MCP surface.
//
// TODO(tools-parity): the real AiHost::Mcp toolset aggregates the
// subagent/notification/reminders/email_mcp/calendar_mcp/import toolsets plus
// the DCS tools; port those tool definitions (name/title/description/schema/
// annotations) from crates/ai_tools + the toolset crates. The entries below
// cover the tools named in the server's own instructions so list_tools is
// usable while execution is stubbed.
// ---------------------------------------------------------------------------

func objectSchema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

var (
	trueVal  = true
	falseVal = false
)

// StaticCatalog is the placeholder ToolCatalog listing the documented tools.
type StaticCatalog struct{}

// Tools implements ToolCatalog.
func (StaticCatalog) Tools(context.Context) []ToolSpec {
	query := map[string]any{"type": "string"}
	return []ToolSpec{
		{
			Name:        "ContentSearch",
			Title:       "Content Search",
			Description: "Search the full text of documents, emails, and messages in the user's Macro workspace.",
			InputSchema: objectSchema(map[string]any{"query": query}, "query"),
			ReadOnly:    true, OpenWorld: &falseVal,
		},
		{
			Name:        "NameSearch",
			Title:       "Name Search",
			Description: "Find Macro entities (documents, channels, chats, projects, threads) by name.",
			InputSchema: objectSchema(map[string]any{"query": query}, "query"),
			ReadOnly:    true, OpenWorld: &falseVal,
		},
		{
			Name:        "ReadContent",
			Title:       "Read Content",
			Description: "Read the full content of a Macro item.",
			InputSchema: objectSchema(nil),
			ReadOnly:    true, OpenWorld: &falseVal,
		},
		{
			Name:        "ReadMetadata",
			Title:       "Read Metadata",
			Description: "Read metadata (owner, timestamps, type) for a Macro item.",
			InputSchema: objectSchema(nil),
			ReadOnly:    true, OpenWorld: &falseVal,
		},
		{
			Name:        "ReadThread",
			Title:       "Read Thread",
			Description: "Read an email thread's messages.",
			InputSchema: objectSchema(nil),
			ReadOnly:    true, OpenWorld: &falseVal,
		},
		{
			Name:        "ReadChannelMessages",
			Title:       "Read Channel Messages",
			Description: "Read messages and attachments from a Macro channel.",
			InputSchema: objectSchema(nil),
			ReadOnly:    true, OpenWorld: &falseVal,
		},
		{
			Name:        "ReadChannelThread",
			Title:       "Read Channel Thread",
			Description: "Read a channel message thread.",
			InputSchema: objectSchema(nil),
			ReadOnly:    true, OpenWorld: &falseVal,
		},
		{
			Name:        "ReadChannelMessageContext",
			Title:       "Read Channel Message Context",
			Description: "Read the surrounding context of a channel message.",
			InputSchema: objectSchema(nil),
			ReadOnly:    true, OpenWorld: &falseVal,
		},
		{
			Name:        "ListEntities",
			Title:       "List Entities",
			Description: "Browse the user's recent Macro items.",
			InputSchema: objectSchema(nil),
			ReadOnly:    true, OpenWorld: &falseVal,
		},
		{
			Name:        "CreateDocument",
			Title:       "Create Document",
			Description: "Create a new Macro document.",
			InputSchema: objectSchema(nil),
			Destructive: &falseVal, OpenWorld: &falseVal,
		},
		{
			Name:        "EditDocument",
			Title:       "Edit Document",
			Description: "Edit an existing Macro document.",
			InputSchema: objectSchema(nil),
			Destructive: &trueVal, OpenWorld: &falseVal,
		},
	}
}

// NotImplementedCaller is the placeholder ToolCaller: tools respond with an
// isError result until the Go toolset port lands.
//
// TODO(tools-parity): implement CallTool against the Go port of
// crates/ai_toolset (see AGENTS guidance) plus the tool_response media
// handling (markdown_images attachment resolution via the static file URL).
type NotImplementedCaller struct{}

// CallTool implements ToolCaller.
func (NotImplementedCaller) CallTool(_ context.Context, _ string, toolName string, _ json.RawMessage) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{
			Text: "tool " + toolName + " is not yet implemented in the Go MCP service",
		}},
		IsError: true,
	}, nil
}
