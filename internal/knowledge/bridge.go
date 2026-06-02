package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"diesel/internal/chat"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Bridge adapts the knowledge graph to chat.KnowledgeBase. The graph read for
// prompt injection goes straight to the store (fast, no round trip), while
// tool listing and calls go through a real in-memory MCP client session — the
// same JSON-RPC protocol an external client would speak — so the companion
// model exercises the actual MCP surface rather than a shortcut.
type Bridge struct {
	store   *Store
	session *mcp.ClientSession
}

var _ chat.KnowledgeBase = (*Bridge)(nil)

// modelHiddenTools are the read-only tools we do NOT advertise to Diesel's own
// model: the entire graph is injected into its system prompt every turn, so it
// reads from there and never needs to query — and exposing fewer, write-only
// tools keeps a small local model focused on recording memory. The MCP server
// still exposes these to external clients over the HTTP listener, which have no
// such injection and genuinely need to read the graph.
var modelHiddenTools = map[string]bool{
	"read_graph":   true,
	"search_nodes": true,
	"open_nodes":   true,
}

// GraphJSON returns the entire graph as compact-but-readable JSON for the
// system prompt.
func (b *Bridge) GraphJSON(ctx context.Context) (string, error) {
	g, err := b.store.ReadGraph(ctx)
	if err != nil {
		return "", err
	}
	out, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return "", fmt.Errorf("knowledge: encode graph: %w", err)
	}
	return string(out), nil
}

// Tools lists the server's tools over the MCP session and converts each into
// the chat layer's ToolDef (name, description, raw JSON-Schema params).
func (b *Bridge) Tools(ctx context.Context) ([]chat.ToolDef, error) {
	res, err := b.session.ListTools(ctx, nil)
	if err != nil {
		logger.Debug().Err(err).Msg("ListTools failed")
		return nil, fmt.Errorf("knowledge: list tools: %w", err)
	}
	logger.Debug().Msgf("ListTools returned %d tools", len(res.Tools))
	defs := make([]chat.ToolDef, 0, len(res.Tools))
	for _, t := range res.Tools {
		if modelHiddenTools[t.Name] {
			continue // read-only tool — the model reads from the injected graph
		}
		// From the client side InputSchema is the server's schema decoded as a
		// map[string]any; re-marshal it to raw JSON for the OpenAI tools array.
		schema, err := json.Marshal(t.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("knowledge: encode schema for %q: %w", t.Name, err)
		}
		defs = append(defs, chat.ToolDef{
			Name:        t.Name,
			Description: t.Description,
			Schema:      schema,
		})
	}
	logger.Debug().Msgf("advertising %d write tools to the model (read-only tools hidden)", len(defs))
	return defs, nil
}

// Call invokes a tool and flattens its result to text. A tool-level error
// (IsError) is returned as readable text prefixed with "Error:" so the model
// can recover; only transport failures surface as a Go error.
func (b *Bridge) Call(ctx context.Context, name string, args json.RawMessage) (string, error) {
	logger.Debug().Msgf("CallTool %q args=%s", name, string(args))
	res, err := b.session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		logger.Debug().Err(err).Msgf("CallTool %q transport error", name)
		return "", fmt.Errorf("knowledge: call %q: %w", name, err)
	}
	text := textOf(res)
	logger.Debug().Msgf("CallTool %q isError=%v resultLen=%d", name, res.IsError, len(text))
	if res.IsError {
		return "Error: " + text, nil
	}
	if text == "" {
		return "ok", nil
	}
	return text, nil
}

// textOf concatenates the text content blocks of a tool result.
func textOf(res *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}
