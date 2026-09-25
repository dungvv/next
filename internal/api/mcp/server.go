package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/macro-inc/macro/internal/api/mcpauth"
)

// instructions mirrors the mcp server's ServerInfo.instructions: the tool-use
// preamble plus the item-linking rules rendered against the public app base
// URL (crates/prompt::mcp_instructions).
//
// TODO(prompt-parity): MCP_STATIC_INSTRUCTIONS composes prompt::citations,
// do_not, about_macro and document_content_links; port those sections
// verbatim so MCP clients get identical instructions.
func instructions(baseURL string) string {
	base := strings.TrimRight(baseURL, "/")
	linkForm := base + "/app/<type>/<id>"
	return "This server provides tools for interacting with a user's Macro workspace. " +
		"Use ContentSearch and NameSearch to find entities. " +
		"Use ReadContent, ReadMetadata, and ReadThread to read them. " +
		"Use ReadChannelMessages, ReadChannelThread, and ReadChannelMessageContext " +
		"to read channel messages and their attachments. Channel image attachments " +
		"include inline images when available; image and video attachments include " +
		"downloadable URLs. Use a video-capable tool to inspect video URLs. " +
		"Use CreateDocument to create new documents. " +
		"Use EditDocument to edit existing documents. " +
		"Use ListEntities to browse recent items.\n\n" +
		"When referring the user to a Macro item (document, channel, chat, project, " +
		"task, or email thread) in your responses, write a plain URL of the form " +
		"`" + linkForm + "`, where `<type>` is the item's type — `md` for a " +
		"document, `channel`, `chat`, `project`, `task`, or `email` for an email " +
		"thread — and `<id>` is the item id. Render it as a normal Markdown link, " +
		"e.g. `[Name](" + base + "/app/md/<id>)`.\n\n" +
		"Do NOT emit `<m-document-mention>` XML tags or any other Macro internal " +
		"mention/markup format in these responses. Those only render inside the " +
		"Macro app and appear as raw text to MCP clients.\n\n" +
		"When you list multiple Macro items, present them as a Markdown table with " +
		"the columns `number`, `name`, and `link`, where `link` is the `" +
		linkForm + "` URL for each item.\n"
}

// serverVersion mirrors env!("CARGO_PKG_VERSION") on the Rust service.
const serverVersion = "0.0.0-go"

// newServer builds a per-request MCP server bound to the authenticated
// caller (mirrors AuthenticatedToolService::new inside the
// StreamableHttpService factory).
func newServer(ctx context.Context, catalog ToolCatalog, caller ToolCaller, userID, itemBaseURL string) *mcp.Server {
	impl := &mcp.Implementation{
		Name:        "macro-tools",
		Title:       "Macro",
		Description: "Search, read, and create content across documents, emails, and messages in Macro.",
		Version:     serverVersion,
	}
	if base := strings.TrimRight(itemBaseURL, "/"); base != "" {
		impl.Icons = []mcp.Icon{{
			Source:   base + "/app/macro-favicon.svg",
			MIMEType: "image/svg+xml",
			Sizes:    []string{"any"},
		}}
	}
	s := mcp.NewServer(impl, &mcp.ServerOptions{
		Instructions: instructions(itemBaseURL),
	})
	for _, spec := range catalog.Tools(ctx) {
		spec := spec
		s.AddTool(&mcp.Tool{
			Name:        spec.Name,
			Description: spec.Description,
			InputSchema: spec.InputSchema,
			Annotations: &mcp.ToolAnnotations{
				Title:           spec.Title,
				ReadOnlyHint:    spec.ReadOnly,
				DestructiveHint: spec.Destructive,
				IdempotentHint:  spec.Idempotent,
				OpenWorldHint:   spec.OpenWorld,
			},
		}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			// Mirrors AuthenticatedToolService::authenticated_user_id: the
			// bearer middleware has already run, so a missing identity means
			// auth is misconfigured.
			uid := userID
			if uid == "" {
				if c, ok := mcpauth.CallerFromContext(ctx); ok {
					uid = c
				}
			}
			if uid == "" {
				return nil, fmt.Errorf("missing user identity — is auth configured?")
			}
			return caller.CallTool(ctx, uid, req.Params.Name, req.Params.Arguments)
		})
	}
	return s
}
