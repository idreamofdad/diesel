package chat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"diesel/internal/settings"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeKB is a minimal KnowledgeBase for exercising the tool loop without the
// real MCP server.
type fakeKB struct {
	graph string
	mu    sync.Mutex
	calls []string
}

func (f *fakeKB) GraphJSON(context.Context) (string, error) { return f.graph, nil }
func (f *fakeKB) Tools(context.Context) ([]ToolDef, error) {
	return []ToolDef{{
		Name:        "create_entities",
		Description: "create entities",
		Schema:      json.RawMessage(`{"type":"object","properties":{}}`),
	}}, nil
}
func (f *fakeKB) Call(_ context.Context, name string, _ json.RawMessage) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
	return `{"ok":true}`, nil
}

// scriptedServer returns responses from a list, one per request, capturing
// each decoded request body for assertions. It returns the server URL and a
// pointer to the captured bodies slice.
func scriptedServer(t *testing.T, responses []func(w http.ResponseWriter)) (string, *[]map[string]any) {
	t.Helper()
	var (
		mu     sync.Mutex
		bodies []map[string]any
		i      int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var b map[string]any
		_ = json.Unmarshal(raw, &b)
		mu.Lock()
		bodies = append(bodies, b)
		idx := i
		i++
		mu.Unlock()
		if idx < len(responses) {
			responses[idx](w)
			return
		}
		w.WriteHeader(500)
		_, _ = io.WriteString(w, "no scripted response")
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &bodies
}

func toolCallResponse(w http.ResponseWriter) {
	w.WriteHeader(200)
	_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"","tool_calls":[`+
		`{"id":"c1","type":"function","function":{"name":"create_entities","arguments":"{}"}}`+
		`]}}],"usage":{"prompt_tokens":5,"completion_tokens":5,"total_tokens":10}}`)
}

func finalReplyResponse(text string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":`+
			jsonString(`{"text":"`+text+`","emotion":"happy","naked":false,"background":"pub","pose":"sitting"}`)+
			`}}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`)
	}
}

// jsonString JSON-encodes s so it can be embedded as a string value.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// noToolCalls is a 200 response with empty content and no tool_calls — the
// model signalling "nothing (more) to do".
func noToolCalls(w http.ResponseWriter) {
	w.WriteHeader(200)
	_, _ = io.WriteString(w, `{"choices":[{"message":{"content":""}}],"usage":{}}`)
}

// TestCompletion_ReplyCarriesGraphButNoTools: the reply path injects the graph
// for reading but never advertises tools — tools are reserved for MemoryPass.
func TestCompletion_ReplyCarriesGraphButNoTools(t *testing.T) {
	url, bodies := scriptedServer(t, []func(http.ResponseWriter){
		finalReplyResponse("hello"),
	})
	kb := &fakeKB{graph: `{"entities":[{"name":"Tyr","entityType":"person"}],"relations":[]}`}

	reply, _, err := Completion(context.Background(),
		settings.AppSettings{APIEndpoint: url, Model: "m"},
		[]Message{{Role: RoleUser, Content: "hi"}},
		kb,
	)
	require.NoError(t, err)
	assert.Equal(t, "hello", reply.Text)
	require.Len(t, *bodies, 1, "reply path is a single call")
	first := (*bodies)[0]
	_, hasTools := first["tools"]
	assert.False(t, hasTools, "reply request must NOT advertise tools")
	_, hasSchema := first["response_format"]
	assert.True(t, hasSchema, "reply request carries the strict schema")
	assert.Contains(t, systemText(first), "Tyr", "graph injected into the system prompt for reading")
	assert.Empty(t, kb.calls, "Completion never calls tools")
}

// TestMemoryPass_RecordsViaTools: the second pass advertises tools, no schema,
// executes the model's tool calls, and stops when the model is done.
func TestMemoryPass_RecordsViaTools(t *testing.T) {
	url, bodies := scriptedServer(t, []func(http.ResponseWriter){
		toolCallResponse, // round 0: model calls create_entities
		noToolCalls,      // round 1: nothing more to record
	})
	kb := &fakeKB{graph: `{"entities":[],"relations":[]}`}

	err := MemoryPass(context.Background(),
		settings.AppSettings{APIEndpoint: url, Model: "m"},
		[]Message{{Role: RoleUser, Content: "I'm Tyr"}, {Role: RoleAssistant, Content: "hey"}},
		kb,
	)
	require.NoError(t, err)
	assert.Equal(t, []string{"create_entities"}, kb.calls, "the requested write tool ran once")
	require.GreaterOrEqual(t, len(*bodies), 1)
	first := (*bodies)[0]
	_, hasTools := first["tools"]
	assert.True(t, hasTools, "memory pass advertises tools")
	_, hasSchema := first["response_format"]
	assert.False(t, hasSchema, "memory pass NEVER sends a response_format — no tools+schema clash")
}

// TestMemoryPass_SendsUserMessage: the memory pass must include a user turn.
// A request of system-only messages makes strict chat templates 400 with
// "No user query found in messages" — which is what a separately configured
// tools model with a stricter template does.
func TestMemoryPass_SendsUserMessage(t *testing.T) {
	url, bodies := scriptedServer(t, []func(http.ResponseWriter){noToolCalls})
	kb := &fakeKB{graph: `{"entities":[],"relations":[]}`}
	err := MemoryPass(context.Background(),
		settings.AppSettings{APIEndpoint: url, Model: "m"},
		[]Message{{Role: RoleUser, Content: "I'm Tyr"}, {Role: RoleAssistant, Content: "hey"}},
		kb,
	)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(*bodies), 1)
	assert.True(t, hasRole((*bodies)[0], "user"),
		"memory pass must send at least one user message or strict templates reject it")
}

// TestMemoryPass_NoOpWhenNothingNew: when the model makes no tool calls, the
// pass records nothing and returns cleanly.
func TestMemoryPass_NoOpWhenNothingNew(t *testing.T) {
	url, _ := scriptedServer(t, []func(http.ResponseWriter){noToolCalls})
	kb := &fakeKB{graph: `{"entities":[],"relations":[]}`}
	err := MemoryPass(context.Background(),
		settings.AppSettings{APIEndpoint: url, Model: "m"},
		[]Message{{Role: RoleUser, Content: "nice weather"}},
		kb,
	)
	require.NoError(t, err)
	assert.Empty(t, kb.calls)
}

// TestMemoryPass_NilKBIsNoOp: no knowledge base means no work and no error.
func TestMemoryPass_NilKBIsNoOp(t *testing.T) {
	require.NoError(t, MemoryPass(context.Background(),
		settings.AppSettings{APIEndpoint: "http://127.0.0.1:1", Model: "m"}, nil, nil))
}

// TestToolsToWire_BackfillsProperties: a no-arg tool whose schema is the bare
// {"type":"object"} (what the SDK infers for an empty input struct, e.g.
// read_graph) must come out with an explicit empty "properties" map — some
// backends (LM Studio) 400 the whole request otherwise.
func TestToolsToWire_BackfillsProperties(t *testing.T) {
	wire := toolsToWire([]ToolDef{
		{Name: "read_graph", Description: "no args", Schema: json.RawMessage(`{"type":"object"}`)},
		{Name: "search_nodes", Description: "one arg", Schema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`)},
	})
	require.Len(t, wire, 2)
	for _, w := range wire {
		fn := w["function"].(map[string]any)
		params := fn["parameters"].(map[string]any)
		assert.Equal(t, "object", params["type"])
		_, ok := params["properties"]
		assert.True(t, ok, "tool %v must carry a properties map", fn["name"])
	}
	// The one-arg tool kept its real properties untouched.
	props := wire[1]["function"].(map[string]any)["parameters"].(map[string]any)["properties"].(map[string]any)
	assert.Contains(t, props, "query")
}

// hasRole reports whether any message in a request body carries the given role.
func hasRole(body map[string]any, role string) bool {
	msgs, _ := body["messages"].([]any)
	for _, m := range msgs {
		if mm, ok := m.(map[string]any); ok && mm["role"] == role {
			return true
		}
	}
	return false
}

// systemText concatenates the content of all system messages in a request body.
func systemText(body map[string]any) string {
	msgs, _ := body["messages"].([]any)
	var sb strings.Builder
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		if mm["role"] == "system" {
			if c, ok := mm["content"].(string); ok {
				sb.WriteString(c)
				sb.WriteByte('\n')
			}
		}
	}
	return sb.String()
}
