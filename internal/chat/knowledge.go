package chat

import (
	"context"
	"encoding/json"
)

// KnowledgeBase is the chat layer's view of Diesel's persistent memory. It is
// deliberately narrow and SDK-agnostic: chat knows nothing about MCP, the
// knowledge package, or SQLite — only that something can hand it the graph to
// inject, advertise a set of callable tools, and run a tool call. The
// knowledge package provides the concrete implementation; Completion treats a
// nil KnowledgeBase as "memory disabled" and behaves exactly as it did before
// tools existed.
type KnowledgeBase interface {
	// GraphJSON returns the whole knowledge graph as a JSON string for
	// injection into the system prompt. An empty graph still returns valid
	// JSON (e.g. {"entities":[],"relations":[]}).
	GraphJSON(ctx context.Context) (string, error)
	// Tools lists the callable tools (name, description, JSON-Schema params)
	// to advertise to the model as OpenAI-style function tools.
	Tools(ctx context.Context) ([]ToolDef, error)
	// Call invokes a tool by name with raw JSON arguments and returns the
	// tool's textual result. A tool-level failure (e.g. "entity doesn't
	// exist") is returned as result text the model can read, not as an error;
	// err is reserved for transport/unexpected failures.
	Call(ctx context.Context, name string, args json.RawMessage) (string, error)
}

// ToolDef is a single advertisable tool. Schema is the raw JSON-Schema object
// for the tool's parameters, dropped verbatim into the OpenAI tools array.
type ToolDef struct {
	Name        string
	Description string
	Schema      json.RawMessage
}
