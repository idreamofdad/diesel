package knowledge

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// serverInstructions is advertised to connected MCP clients (and is a handy
// reminder for the companion model about what the graph is for).
const serverInstructions = `A persistent knowledge graph for remembering people, animals, places, and how they relate.
Entities are typed, uniquely-named nodes (e.g. "Tyr Mactire" of type "person").
Observations are short free-text facts attached to an entity (e.g. "works at McDonalds").
Relations are directed, active-voice edges between entities (e.g. Tyr —owns→ Beckett).
Create entities before relating them; deleting an entity also removes its observations and relations.`

// tool input shapes. Field json tags and the order here mirror the reference
// MCP "memory" server so external clients recognize the surface; the
// jsonschema tags become the per-field descriptions the model sees. All
// fields are required by the inferred schema — pass an empty array when a
// list doesn't apply (e.g. an entity with no observations yet).
type (
	createEntitiesIn struct {
		Entities []Entity `json:"entities" jsonschema:"the entities to create; an existing name merges new observations in and keeps its original type"`
	}
	deleteEntitiesIn struct {
		EntityNames []string `json:"entityNames" jsonschema:"names of entities to delete; their observations and relations are removed too"`
	}
	addObservationsIn struct {
		Observations []ObservationMutation `json:"observations" jsonschema:"facts to add, grouped by the entity they describe; the entity must already exist"`
	}
	deleteObservationsIn struct {
		Deletions []ObservationMutation `json:"deletions" jsonschema:"facts to remove, grouped by entity; missing facts are ignored"`
	}
	createRelationsIn struct {
		Relations []Relation `json:"relations" jsonschema:"directed edges to add; both endpoint entities must already exist"`
	}
	deleteRelationsIn struct {
		Relations []Relation `json:"relations" jsonschema:"exact edges to remove; missing edges are ignored"`
	}
	readGraphIn   struct{}
	searchNodesIn struct {
		Query string `json:"query" jsonschema:"case-insensitive text matched against entity names, types, and observations"`
	}
	openNodesIn struct {
		Names []string `json:"names" jsonschema:"entity names to fetch, with the relations among them"`
	}
)

// NewServer builds the in-process MCP server exposing the nine knowledge-graph
// tools (the reference memory-server surface) backed by store. The same
// *mcp.Server instance serves both the internal in-memory session the chat
// loop uses and any external clients that connect over the HTTP listener.
func NewServer(store *Store) *mcp.Server {
	s := mcp.NewServer(
		&mcp.Implementation{Name: "diesel-knowledge", Title: "Diesel Knowledge Graph", Version: "1.0.0"},
		&mcp.ServerOptions{Instructions: serverInstructions},
	)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "create_entities",
		Description: "Create one or more entities (typed, named nodes). Re-creating an existing name merges in any new observations and keeps the original type.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in createEntitiesIn) (*mcp.CallToolResult, any, error) {
		created, err := store.CreateEntities(ctx, in.Entities)
		if err != nil {
			return domainError(err), nil, nil
		}
		return jsonResult(map[string]any{"entities": created}), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "delete_entities",
		Description: "Delete entities by name. Cascades to their observations and any relation touching them.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in deleteEntitiesIn) (*mcp.CallToolResult, any, error) {
		if err := store.DeleteEntities(ctx, in.EntityNames); err != nil {
			return domainError(err), nil, nil
		}
		return jsonResult(map[string]any{"ok": true, "deleted": in.EntityNames}), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "add_observations",
		Description: "Add free-text facts to existing entities. The target entity must already exist.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in addObservationsIn) (*mcp.CallToolResult, any, error) {
		if err := store.AddObservations(ctx, in.Observations); err != nil {
			return domainError(err), nil, nil
		}
		return jsonResult(map[string]any{"ok": true}), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "delete_observations",
		Description: "Remove specific facts from entities. Missing facts are ignored.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in deleteObservationsIn) (*mcp.CallToolResult, any, error) {
		if err := store.DeleteObservations(ctx, in.Deletions); err != nil {
			return domainError(err), nil, nil
		}
		return jsonResult(map[string]any{"ok": true}), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "create_relations",
		Description: "Create directed, active-voice relations between existing entities (e.g. Tyr owns Beckett). Both endpoints must already exist.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in createRelationsIn) (*mcp.CallToolResult, any, error) {
		created, err := store.CreateRelations(ctx, in.Relations)
		if err != nil {
			return domainError(err), nil, nil
		}
		return jsonResult(map[string]any{"relations": created}), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "delete_relations",
		Description: "Remove specific relations by exact (from, relationType, to) triple. Missing edges are ignored.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in deleteRelationsIn) (*mcp.CallToolResult, any, error) {
		if err := store.DeleteRelations(ctx, in.Relations); err != nil {
			return domainError(err), nil, nil
		}
		return jsonResult(map[string]any{"ok": true}), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "read_graph",
		Description: "Return the entire knowledge graph: all entities with their observations, and all relations.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ readGraphIn) (*mcp.CallToolResult, any, error) {
		g, err := store.ReadGraph(ctx)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(g), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "search_nodes",
		Description: "Return the subgraph whose entities match a text query (by name, type, or observation), with the relations among them.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in searchNodesIn) (*mcp.CallToolResult, any, error) {
		g, err := store.SearchNodes(ctx, in.Query)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(g), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "open_nodes",
		Description: "Return a specific set of entities by name, with the relations among them.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in openNodesIn) (*mcp.CallToolResult, any, error) {
		g, err := store.OpenNodes(ctx, in.Names)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(g), nil, nil
	})

	return s
}

// jsonResult wraps a value as a tool result whose single text content is the
// indented JSON encoding of v — what both the companion model and external
// clients read back. Out is left as nil (the typed handlers use Out=any, so
// the SDK adds no output schema and no structured content of its own).
func jsonResult(v any) *mcp.CallToolResult {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return domainError(fmt.Errorf("encode result: %w", err))
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}
}

// domainError reports an expected, user-facing failure (entity doesn't exist,
// empty name, …) as a tool error the model can read and react to — IsError
// plus the message as text — rather than a protocol-level Go error, which the
// model would never see.
func domainError(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
	}
}
