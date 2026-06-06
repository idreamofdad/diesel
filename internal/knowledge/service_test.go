package knowledge

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"diesel/internal/settings"
	"diesel/internal/storage"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// openTestService spins up the full service (MCP server + in-memory client
// session) over a temp database, exercising the real JSON-RPC path the
// companion model uses.
func openTestService(t *testing.T) *Service {
	t.Helper()
	st, err := storage.Open(filepath.Join(t.TempDir(), "diesel.db"))
	require.NoError(t, err)
	svc, err := New(st, settings.AppSettings{})
	require.NoError(t, err)
	t.Cleanup(func() {
		svc.Stop()
		_ = st.Close()
	})
	return svc
}

func TestBridge_AdvertisesWriteToolsOnly(t *testing.T) {
	svc := openTestService(t)
	defs, err := svc.Bridge().Tools(context.Background())
	require.NoError(t, err)

	got := map[string]bool{}
	for _, d := range defs {
		got[d.Name] = true
		assert.NotEmpty(t, d.Schema, "tool %q should advertise a parameter schema", d.Name)
	}
	// The six write tools are offered to the model...
	for _, want := range []string{
		"create_entities", "delete_entities", "add_observations", "delete_observations",
		"create_relations", "delete_relations",
	} {
		assert.True(t, got[want], "missing write tool %q", want)
	}
	// ...but the read-only tools are hidden — the model reads the graph from
	// its injected system prompt instead.
	for _, hidden := range []string{"read_graph", "search_nodes", "open_nodes"} {
		assert.False(t, got[hidden], "read-only tool %q should not be advertised to the model", hidden)
	}
}

func TestBridge_CreateThenReadOverMCP(t *testing.T) {
	svc := openTestService(t)
	b := svc.Bridge()
	ctx := context.Background()

	args, _ := json.Marshal(map[string]any{
		"entities": []map[string]any{
			{"name": "Tyr Mactire", "entityType": "person", "observations": []string{"works at McDonalds"}},
		},
	})
	out, err := b.Call(ctx, "create_entities", args)
	require.NoError(t, err)
	assert.NotContains(t, out, "Error:")

	// read_graph over the session reflects the new entity...
	read, err := b.Call(ctx, "read_graph", json.RawMessage(`{}`))
	require.NoError(t, err)
	assert.Contains(t, read, "Tyr Mactire")
	assert.Contains(t, read, "works at McDonalds")

	// ...and so does the direct GraphJSON used for prompt injection.
	graph, err := b.GraphJSON(ctx)
	require.NoError(t, err)
	assert.Contains(t, graph, "Tyr Mactire")
}

func TestBridge_DanglingRelationSurfacesAsToolError(t *testing.T) {
	svc := openTestService(t)
	b := svc.Bridge()
	ctx := context.Background()

	// Tyr exists; Beckett was never created, so the relation must be rejected
	// naming the missing endpoint.
	mk, _ := json.Marshal(map[string]any{
		"entities": []map[string]any{{"name": "Tyr Mactire", "entityType": "person", "observations": []string{}}},
	})
	_, err := b.Call(ctx, "create_entities", mk)
	require.NoError(t, err)

	args, _ := json.Marshal(map[string]any{
		"relations": []map[string]any{
			{"from": "Tyr Mactire", "to": "Beckett", "relationType": "owns"},
		},
	})
	out, err := b.Call(ctx, "create_relations", args)
	require.NoError(t, err, "a domain failure is a tool error, not a transport error")
	assert.True(t, strings.HasPrefix(out, "Error:"), "expected a tool error, got %q", out)
	assert.Contains(t, out, "Beckett")
}
