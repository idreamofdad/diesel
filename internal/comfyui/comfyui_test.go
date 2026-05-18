package comfyui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// node is a small constructor that mirrors workflowNode and keeps the
// test tables readable. Inputs is variadic key/value so each case can
// stick to the inputs it cares about.
func node(class string, inputs map[string]any) workflowNode {
	return workflowNode{ClassType: class, Inputs: inputs}
}

// titled mirrors `node` but also stamps a `_meta.title`, matching how
// ComfyUI's editor exports the API-format graph.
func titled(class, title string, inputs map[string]any) workflowNode {
	return workflowNode{
		ClassType: class,
		Inputs:    inputs,
		Meta:      map[string]any{"title": title},
	}
}

// connection mirrors ComfyUI's input wiring shape ["<node-id>", <slot>],
// which is `[]any` after a json.Unmarshal round-trip.
func connection(id string, slot int) []any {
	return []any{id, float64(slot)}
}

func TestFindSampler(t *testing.T) {
	cases := []struct {
		name    string
		graph   map[string]workflowNode
		wantID  string
		wantErr string
	}{
		{
			name: "single KSamplerAdvanced is found",
			graph: map[string]workflowNode{
				"10": node("KSamplerAdvanced", map[string]any{
					"positive":   connection("6", 0),
					"negative":   connection("7", 0),
					"noise_seed": float64(42),
				}),
			},
			wantID: "10",
		},
		{
			name: "single KSampler with plain seed is found",
			graph: map[string]workflowNode{
				"3": node("KSampler", map[string]any{
					"positive": connection("1", 0),
					"negative": connection("2", 0),
					"seed":     float64(7),
				}),
			},
			wantID: "3",
		},
		{
			name: "non-sampler nodes are ignored",
			graph: map[string]workflowNode{
				"1": node("CheckpointLoaderSimple", map[string]any{"ckpt_name": "x"}),
				"2": node("EmptyLatentImage", map[string]any{"width": float64(512)}),
				"3": node("CLIPTextEncode", map[string]any{"text": "hi"}),
			},
			wantErr: "no KSampler-family node",
		},
		{
			name: "missing seed/noise_seed is not a sampler",
			graph: map[string]workflowNode{
				"5": node("Sampler", map[string]any{
					"positive": connection("1", 0),
					"negative": connection("2", 0),
				}),
			},
			wantErr: "no KSampler-family node",
		},
		{
			name: "missing positive is not a sampler",
			graph: map[string]workflowNode{
				"5": node("Sampler", map[string]any{
					"negative":   connection("2", 0),
					"noise_seed": float64(1),
				}),
			},
			wantErr: "no KSampler-family node",
		},
		{
			name: "lowest-id sampler wins when multiple match",
			graph: map[string]workflowNode{
				"30": node("KSampler", map[string]any{
					"positive": connection("1", 0),
					"negative": connection("2", 0),
					"seed":     float64(1),
				}),
				"10": node("KSamplerAdvanced", map[string]any{
					"positive":   connection("3", 0),
					"negative":   connection("4", 0),
					"noise_seed": float64(2),
				}),
				"20": node("KSampler", map[string]any{
					"positive": connection("5", 0),
					"negative": connection("6", 0),
					"seed":     float64(3),
				}),
			},
			wantID: "10",
		},
		{
			name:    "empty graph errors",
			graph:   map[string]workflowNode{},
			wantErr: "no KSampler-family node",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, _, err := findSampler(tc.graph)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantID, id)
		})
	}
}

func TestRewritePromptAndSeed(t *testing.T) {
	cases := []struct {
		name       string
		graph      map[string]workflowNode
		positive   string
		negative   string
		seed       int64
		wantErrSub string
		// assertGraph runs on success and checks mutations.
		assertGraph func(t *testing.T, g map[string]workflowNode)
	}{
		{
			name: "noise_seed sampler path",
			graph: map[string]workflowNode{
				"10": node("KSamplerAdvanced", map[string]any{
					"positive":   connection("6", 0),
					"negative":   connection("7", 0),
					"noise_seed": float64(0),
				}),
				"6": node("CLIPTextEncode", map[string]any{"text": "OLD POS"}),
				"7": node("CLIPTextEncode", map[string]any{"text": "OLD NEG"}),
			},
			positive: "a portrait",
			negative: "blurry",
			seed:     12345,
			assertGraph: func(t *testing.T, g map[string]workflowNode) {
				assert.Equal(t, int64(12345), g["10"].Inputs["noise_seed"])
				assert.Equal(t, "a portrait", g["6"].Inputs["text"])
				assert.Equal(t, "blurry", g["7"].Inputs["text"])
			},
		},
		{
			name: "plain seed sampler path",
			graph: map[string]workflowNode{
				"3": node("KSampler", map[string]any{
					"positive": connection("1", 0),
					"negative": connection("2", 0),
					"seed":     float64(0),
				}),
				"1": node("CLIPTextEncode", map[string]any{"text": ""}),
				"2": node("CLIPTextEncode", map[string]any{"text": ""}),
			},
			positive: "p",
			negative: "n",
			seed:     7,
			assertGraph: func(t *testing.T, g map[string]workflowNode) {
				assert.Equal(t, int64(7), g["3"].Inputs["seed"])
				assert.Nil(t, g["3"].Inputs["noise_seed"], "should not introduce noise_seed key")
				assert.Equal(t, "p", g["1"].Inputs["text"])
				assert.Equal(t, "n", g["2"].Inputs["text"])
			},
		},
		{
			name: "positive points at node without text input",
			graph: map[string]workflowNode{
				"10": node("KSamplerAdvanced", map[string]any{
					"positive":   connection("99", 0),
					"negative":   connection("7", 0),
					"noise_seed": float64(0),
				}),
				"99": node("ConditioningCombine", map[string]any{"conditioning_1": connection("6", 0)}),
				"7":  node("CLIPTextEncode", map[string]any{"text": ""}),
			},
			positive:   "p",
			negative:   "n",
			seed:       1,
			wantErrSub: "no text input",
		},
		{
			name: "positive connection points at missing node",
			graph: map[string]workflowNode{
				"10": node("KSamplerAdvanced", map[string]any{
					"positive":   connection("404", 0),
					"negative":   connection("7", 0),
					"noise_seed": float64(0),
				}),
				"7": node("CLIPTextEncode", map[string]any{"text": ""}),
			},
			positive:   "p",
			negative:   "n",
			seed:       1,
			wantErrSub: "missing node 404",
		},
		{
			name: "no sampler at all",
			graph: map[string]workflowNode{
				"1": node("CheckpointLoaderSimple", map[string]any{"ckpt_name": "x"}),
			},
			positive:   "p",
			negative:   "n",
			seed:       1,
			wantErrSub: "no KSampler-family node",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := rewritePromptAndSeed(tc.graph, tc.positive, tc.negative, tc.seed)
			if tc.wantErrSub != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErrSub)
				return
			}
			require.NoError(t, err)
			if tc.assertGraph != nil {
				tc.assertGraph(t, tc.graph)
			}
		})
	}
}

func TestSetNudity(t *testing.T) {
	cases := []struct {
		name      string
		graph     map[string]workflowNode
		naked     bool
		wantID    string
		wantValue any // nil means: assert the toggle was untouched
	}{
		{
			name: "titled PrimitiveBoolean is flipped to true",
			graph: map[string]workflowNode{
				"56": titled("PrimitiveBoolean", "Nudity", map[string]any{"value": false}),
			},
			naked:     true,
			wantID:    "56",
			wantValue: true,
		},
		{
			name: "titled PrimitiveBoolean is flipped to false",
			graph: map[string]workflowNode{
				"56": titled("PrimitiveBoolean", "Nudity", map[string]any{"value": true}),
			},
			naked:     false,
			wantID:    "56",
			wantValue: false,
		},
		{
			name: "other PrimitiveBoolean nodes are ignored",
			graph: map[string]workflowNode{
				"40": titled("PrimitiveBoolean", "Aroused", map[string]any{"value": false}),
				"56": titled("PrimitiveBoolean", "Nudity", map[string]any{"value": false}),
			},
			naked:     true,
			wantID:    "56",
			wantValue: true,
		},
		{
			name: "lowest-id Nudity wins on duplicates",
			graph: map[string]workflowNode{
				"99": titled("PrimitiveBoolean", "Nudity", map[string]any{"value": false}),
				"10": titled("PrimitiveBoolean", "Nudity", map[string]any{"value": false}),
			},
			naked:     true,
			wantID:    "10",
			wantValue: true,
		},
		{
			name: "no toggle in graph is a silent no-op",
			graph: map[string]workflowNode{
				"4": node("CheckpointLoaderSimple", map[string]any{"ckpt_name": "x"}),
			},
			naked:  true,
			wantID: "",
		},
		{
			name: "Nudity title on a non-boolean node is ignored",
			graph: map[string]workflowNode{
				"56": titled("CLIPTextEncode", "Nudity", map[string]any{"text": "hi"}),
			},
			naked:  true,
			wantID: "",
		},
		{
			name: "missing value input means no flip",
			graph: map[string]workflowNode{
				"56": titled("PrimitiveBoolean", "Nudity", map[string]any{}),
			},
			naked:  true,
			wantID: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotID := setNudity(tc.graph, tc.naked)
			assert.Equal(t, tc.wantID, gotID)
			if tc.wantID != "" && tc.wantValue != nil {
				assert.Equal(t, tc.wantValue, tc.graph[tc.wantID].Inputs["value"])
			}
		})
	}
}

func TestSetSteps(t *testing.T) {
	cases := []struct {
		name      string
		graph     map[string]workflowNode
		steps     int
		wantID    string
		wantValue any // nil means: assert the node was untouched
	}{
		{
			name: "titled PrimitiveInt is overwritten",
			graph: map[string]workflowNode{
				"57": titled("PrimitiveInt", "Steps", map[string]any{"value": float64(10)}),
			},
			steps:     25,
			wantID:    "57",
			wantValue: 25,
		},
		{
			name: "other PrimitiveInt nodes are ignored",
			graph: map[string]workflowNode{
				"40": titled("PrimitiveInt", "CFG", map[string]any{"value": float64(4)}),
				"57": titled("PrimitiveInt", "Steps", map[string]any{"value": float64(10)}),
			},
			steps:     30,
			wantID:    "57",
			wantValue: 30,
		},
		{
			name: "lowest-id Steps wins on duplicates",
			graph: map[string]workflowNode{
				"99": titled("PrimitiveInt", "Steps", map[string]any{"value": float64(10)}),
				"10": titled("PrimitiveInt", "Steps", map[string]any{"value": float64(10)}),
			},
			steps:     12,
			wantID:    "10",
			wantValue: 12,
		},
		{
			name: "no node in graph is a silent no-op",
			graph: map[string]workflowNode{
				"4": node("CheckpointLoaderSimple", map[string]any{"ckpt_name": "x"}),
			},
			steps:  20,
			wantID: "",
		},
		{
			name: "Steps title on a non-int node is ignored",
			graph: map[string]workflowNode{
				"57": titled("PrimitiveBoolean", "Steps", map[string]any{"value": false}),
			},
			steps:  20,
			wantID: "",
		},
		{
			name: "missing value input means no write",
			graph: map[string]workflowNode{
				"57": titled("PrimitiveInt", "Steps", map[string]any{}),
			},
			steps:  20,
			wantID: "",
		},
		{
			name: "zero steps is a silent no-op so the workflow default wins",
			graph: map[string]workflowNode{
				"57": titled("PrimitiveInt", "Steps", map[string]any{"value": float64(10)}),
			},
			steps:  0,
			wantID: "",
		},
		{
			name: "negative steps is a silent no-op",
			graph: map[string]workflowNode{
				"57": titled("PrimitiveInt", "Steps", map[string]any{"value": float64(10)}),
			},
			steps:  -3,
			wantID: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotID := setSteps(tc.graph, tc.steps)
			assert.Equal(t, tc.wantID, gotID)
			if tc.wantID != "" && tc.wantValue != nil {
				assert.Equal(t, tc.wantValue, tc.graph[tc.wantID].Inputs["value"])
			}
		})
	}
}

func TestSetSteps_OnEmbeddedWorkflow(t *testing.T) {
	// Round-trip against the actual graph shipped in the binary, so a
	// re-export of default_workflow.json that drops the Steps title or
	// the PrimitiveInt class fails here instead of silently leaving the
	// step count stuck at whatever the JSON ships with.
	parse := func(t *testing.T) map[string]workflowNode {
		t.Helper()
		var g map[string]workflowNode
		require.NoError(t, json.Unmarshal([]byte(defaultWorkflow), &g))
		return g
	}

	g := parse(t)
	id := setSteps(g, 25)
	require.NotEmpty(t, id, "embedded workflow should expose a Steps override")
	assert.Equal(t, 25, g[id].Inputs["value"])

	g = parse(t)
	id2 := setSteps(g, 1)
	assert.Equal(t, id, id2, "steps id is stable across calls")
	assert.Equal(t, 1, g[id].Inputs["value"])
}

func TestSetNudity_OnEmbeddedWorkflow(t *testing.T) {
	// Round-trip against the actual graph shipped in the binary, so a
	// re-export of default_workflow.json that drops the Nudity title or
	// the PrimitiveBoolean class fails here instead of silently leaving
	// the toggle stuck.
	parse := func(t *testing.T) map[string]workflowNode {
		t.Helper()
		var g map[string]workflowNode
		require.NoError(t, json.Unmarshal([]byte(defaultWorkflow), &g))
		return g
	}

	g := parse(t)
	id := setNudity(g, true)
	require.NotEmpty(t, id, "embedded workflow should expose a Nudity toggle")
	assert.Equal(t, true, g[id].Inputs["value"])

	g = parse(t)
	id2 := setNudity(g, false)
	assert.Equal(t, id, id2, "toggle id is stable across calls")
	assert.Equal(t, false, g[id].Inputs["value"])
}

func TestRewritePromptAndSeed_OnEmbeddedWorkflow(t *testing.T) {
	// Round-trip through the actual workflow shipped in the binary —
	// guards against drift between default_workflow.json and the
	// discovery code without hardcoding any node IDs in the assertions.
	var graph map[string]workflowNode
	require.NoError(t, json.Unmarshal([]byte(defaultWorkflow), &graph))

	_, err := rewritePromptAndSeed(graph, "hello", "blurry", 42)
	require.NoError(t, err)

	samplerID, sampler, err := findSampler(graph)
	require.NoError(t, err)
	assert.NotEmpty(t, samplerID)

	// Seed landed on whichever field the sampler exposes.
	if v, ok := sampler.Inputs["noise_seed"]; ok {
		assert.EqualValues(t, 42, v)
	} else {
		assert.EqualValues(t, 42, sampler.Inputs["seed"])
	}

	// Prompts landed on the connected text nodes.
	posID, _ := sampler.Inputs["positive"].([]any)[0].(string)
	negID, _ := sampler.Inputs["negative"].([]any)[0].(string)
	assert.Equal(t, "hello", graph[posID].Inputs["text"])
	assert.Equal(t, "blurry", graph[negID].Inputs["text"])
}

func TestSetConnectedText_MalformedInputs(t *testing.T) {
	cases := []struct {
		name       string
		sampler    workflowNode
		wantErrSub string
	}{
		{
			name:       "missing key",
			sampler:    node("KSampler", map[string]any{}),
			wantErrSub: "is not a connection",
		},
		{
			name:       "connection is not a list",
			sampler:    node("KSampler", map[string]any{"positive": "wat"}),
			wantErrSub: "is not a connection",
		},
		{
			name:       "connection has no node id",
			sampler:    node("KSampler", map[string]any{"positive": []any{}}),
			wantErrSub: "is not a connection",
		},
		{
			name:       "node id is not a string",
			sampler:    node("KSampler", map[string]any{"positive": []any{float64(6), float64(0)}}),
			wantErrSub: "no source node id",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := setConnectedText(map[string]workflowNode{}, tc.sampler, "positive", "x")
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErrSub)
		})
	}
}

func TestWsURLFor(t *testing.T) {
	cases := []struct {
		name, endpoint, want string
	}{
		{"http upgrades to ws", "http://127.0.0.1:8188", "ws://127.0.0.1:8188/ws?clientId=abc"},
		{"https upgrades to wss", "https://comfy.example.com", "wss://comfy.example.com/ws?clientId=abc"},
		{"trailing slash on path is normalized", "http://x:8188/", "ws://x:8188/ws?clientId=abc"},
		{"sub-path is preserved", "http://x/api/comfy/", "ws://x/api/comfy/ws?clientId=abc"},
		{"unknown scheme falls back to ws", "tcp://x:1234", "ws://x:1234/ws?clientId=abc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := wsURLFor(tc.endpoint, "abc")
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestRandomSeed_Range(t *testing.T) {
	// Non-negative and within the 2^53 ceiling the function documents.
	const ceiling = int64(1) << 53
	for i := 0; i < 50; i++ {
		s := randomSeed()
		assert.GreaterOrEqual(t, s, int64(0))
		assert.Less(t, s, ceiling)
	}
}

func TestRandomHex_LengthAndCharset(t *testing.T) {
	cases := []int{0, 1, 8, 16, 32}
	for _, n := range cases {
		t.Run("len "+strconv.Itoa(n), func(t *testing.T) {
			got := randomHex(n)
			assert.Len(t, got, n*2, "hex output is 2x the byte count")
			for _, c := range got {
				assert.True(t,
					(c >= '0' && c <= '9') || (c >= 'a' && c <= 'f'),
					"non-hex character in output: %q", c)
			}
		})
	}
}

func TestHasInput(t *testing.T) {
	n := node("X", map[string]any{"a": 1, "b": nil})
	assert.True(t, hasInput(n, "a"))
	assert.True(t, hasInput(n, "b"), "nil-valued key still counts as present")
	assert.False(t, hasInput(n, "c"))
	assert.False(t, hasInput(node("X", nil), "a"), "nil inputs map handles cleanly")
}

func TestDecodePromptID(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantID     string
		wantErrSub string
	}{
		{
			name:   "happy path",
			status: 200,
			body:   `{"prompt_id":"abc-123"}`,
			wantID: "abc-123",
		},
		{
			name:       "400 surfaces body",
			status:     400,
			body:       `{"node_errors":{"6":"missing checkpoint"}}`,
			wantErrSub: "HTTP 400",
		},
		{
			name:       "200 but malformed JSON",
			status:     200,
			body:       `{not json`,
			wantErrSub: "invalid character",
		},
		{
			name:       "200 but no prompt_id",
			status:     200,
			body:       `{"queue_position":1}`,
			wantErrSub: "no prompt_id",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)

			resp, err := http.Get(srv.URL)
			require.NoError(t, err)

			id, err := decodePromptID(resp)
			if tc.wantErrSub != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErrSub)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantID, id)
		})
	}
}

func TestFetchImage(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		assert.Equal(t, "/view", r.URL.Path)
		_, _ = w.Write([]byte{0x89, 'P', 'N', 'G'})
	}))
	t.Cleanup(srv.Close)

	got, err := fetchImage(context.Background(), srv.Client(), srv.URL, imageRef{Filename: "out.png", Subfolder: "sub", Type: "output"})
	require.NoError(t, err)
	assert.Equal(t, []byte{0x89, 'P', 'N', 'G'}, got)
	assert.Equal(t, "out.png", gotQuery.Get("filename"))
	assert.Equal(t, "sub", gotQuery.Get("subfolder"))
	assert.Equal(t, "output", gotQuery.Get("type"))
}

func TestFetchImage_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}))
	t.Cleanup(srv.Close)

	_, err := fetchImage(context.Background(), srv.Client(), srv.URL, imageRef{Filename: "x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "view HTTP 404")
}

func TestFetchCheckpoints(t *testing.T) {
	cases := []struct {
		name       string
		endpoint   string
		status     int
		body       string
		want       []string
		wantErrSub string
	}{
		{
			name:   "happy path returns names",
			status: 200,
			body:   `{"CheckpointLoaderSimple":{"input":{"required":{"ckpt_name":[["model_a.safetensors","model_b.safetensors"],{}]}}}}`,
			want:   []string{"model_a.safetensors", "model_b.safetensors"},
		},
		{
			name:   "empty list",
			status: 200,
			body:   `{"CheckpointLoaderSimple":{"input":{"required":{"ckpt_name":[[],{}]}}}}`,
			want:   []string{},
		},
		{
			name:       "missing CheckpointLoaderSimple node",
			status:     200,
			body:       `{}`,
			wantErrSub: "did not describe CheckpointLoaderSimple",
		},
		{
			name:       "tuple shape unexpected",
			status:     200,
			body:       `{"CheckpointLoaderSimple":{"input":{"required":{"ckpt_name":"not a tuple"}}}}`,
			wantErrSub: "unexpected ckpt_name schema",
		},
		{
			name:       "names not a string slice",
			status:     200,
			body:       `{"CheckpointLoaderSimple":{"input":{"required":{"ckpt_name":[42,{}]}}}}`,
			wantErrSub: "unexpected ckpt_name schema",
		},
		{
			name:       "http error",
			status:     500,
			body:       "down",
			wantErrSub: "HTTP 500",
		},
		{
			name:       "no endpoint",
			endpoint:   " ",
			wantErrSub: "no endpoint configured",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := tc.endpoint
			if endpoint == "" {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					assert.Equal(t, "/object_info/CheckpointLoaderSimple", r.URL.Path)
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
				}))
				t.Cleanup(srv.Close)
				endpoint = srv.URL
			}
			got, err := FetchCheckpoints(endpoint)
			if tc.wantErrSub != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErrSub)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// comfyMux serves /system_stats and /object_info/CheckpointLoaderSimple
// independently so TestConnection's two-step probe can be driven
// case-by-case.
type comfyMux struct {
	statsStatus int
	ckptStatus  int
	ckptBody    string
}

func (m *comfyMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/system_stats":
		status := m.statsStatus
		if status == 0 {
			status = 200
		}
		w.WriteHeader(status)
	case "/object_info/CheckpointLoaderSimple":
		status := m.ckptStatus
		if status == 0 {
			status = 200
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(m.ckptBody))
	}
}

func TestTestConnection(t *testing.T) {
	cases := []struct {
		name     string
		setup    func(*comfyMux)
		endpoint string
		wantSub  string
	}{
		{
			name:     "no endpoint",
			endpoint: " ",
			wantSub:  "✗ No endpoint configured.",
		},
		{
			name:    "system_stats 500",
			setup:   func(m *comfyMux) { m.statsStatus = 500 },
			wantSub: "✗ HTTP 500",
		},
		{
			name: "stats ok but checkpoints query fails",
			setup: func(m *comfyMux) {
				m.ckptStatus = 500
			},
			wantSub: "✓ Connected, but couldn't list checkpoints",
		},
		{
			name: "stats ok but no checkpoints installed",
			setup: func(m *comfyMux) {
				m.ckptBody = `{"CheckpointLoaderSimple":{"input":{"required":{"ckpt_name":[[],{}]}}}}`
			},
			wantSub: "✓ Connected, but no checkpoints are installed.",
		},
		{
			name: "happy path",
			setup: func(m *comfyMux) {
				m.ckptBody = `{"CheckpointLoaderSimple":{"input":{"required":{"ckpt_name":[["a","b","c"],{}]}}}}`
			},
			wantSub: "✓ Connected — 3 checkpoint(s) available.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := tc.endpoint
			if endpoint == "" {
				mux := &comfyMux{}
				if tc.setup != nil {
					tc.setup(mux)
				}
				srv := httptest.NewServer(mux)
				t.Cleanup(srv.Close)
				endpoint = srv.URL
			}
			got := TestConnection(endpoint)
			assert.Contains(t, got, tc.wantSub)
		})
	}
}

func TestSaveCharacterImage_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("APPDATA", dir)

	payload := []byte{0x89, 'P', 'N', 'G', 0, 1, 2, 3}
	require.NoError(t, SaveCharacterImage(payload))

	path, err := CharacterImagePath()
	require.NoError(t, err)
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, payload, got)
}
