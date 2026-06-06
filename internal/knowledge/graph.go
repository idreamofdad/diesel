// Package knowledge is Diesel's persistent memory: a small knowledge graph
// of entities, observations, and relations that the companion model edits
// through MCP tools and reads back as a JSON blob injected into its system
// prompt.
//
// The data model mirrors the well-known MCP "memory" server so external MCP
// clients (Claude Desktop, …) feel at home when they connect to the HTTP
// listener: entities are typed, named nodes; observations are free-text
// facts hung off an entity; relations are directed, typed edges named in
// active voice ("{first_name}" —owns→ "{pet_name}").
//
// The graph lives in the same SQLite database as the rest of Diesel's state
// (see the kg_* tables in storage's migrations). This package owns its own
// SQL via storage.Store.SQLDB() — the same shared-pool pattern the Matrix
// bridge uses — so the storage package stays unaware of the graph.
package knowledge

// Entity is one node in the graph: a uniquely-named, typed thing the model
// wants to remember, with the free-text facts it has accumulated about it.
// Name is the natural key — relations refer to entities by name — so it is
// case-sensitive and unique across the whole graph.
type Entity struct {
	Name         string   `json:"name"`
	Type         string   `json:"entityType"`
	Observations []string `json:"observations"`
}

// Relation is a directed, typed edge between two entities, named in active
// voice so the triple reads as a sentence: from ("{first_name}") + relationType
// ("owns") + to ("{pet_name}").
type Relation struct {
	From         string `json:"from"`
	To           string `json:"to"`
	RelationType string `json:"relationType"`
}

// Graph is the whole knowledge base in the shape the read_graph tool returns
// and the chat layer serializes into the system prompt. Both slices are
// always non-nil after a load so the JSON blob renders as "entities": []
// rather than "entities": null on an empty graph — cleaner for the model to
// read and for external clients to parse.
type Graph struct {
	Entities  []Entity   `json:"entities"`
	Relations []Relation `json:"relations"`
}
