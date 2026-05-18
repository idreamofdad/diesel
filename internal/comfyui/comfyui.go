package comfyui

import (
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"diesel/internal/settings"
	"diesel/internal/tracing"
	"diesel/internal/util"

	"github.com/coder/websocket"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// defaultWorkflow is the ComfyUI graph in "API format" (the flat
// {id: {class_type, inputs}} shape the /prompt endpoint accepts, not the
// editor's UI-graph export). It's compiled into the binary at build time
// so there are no extra files to ship. The checkpoint — and every other
// model — is baked into the JSON; Generate parses a fresh copy per call
// and rewrites only the prompt text and seed.
//
//go:embed default_workflow.json
var defaultWorkflow string

// workflowNode is one entry of a ComfyUI API-format graph. Inputs is left
// as a free-form map because node schemas vary wildly and we only ever
// touch a handful of known keys — but it's a reference type, so mutating
// it through a map-value copy still updates the graph we re-marshal.
// Meta carries the optional `_meta` block ComfyUI's editor stamps on
// exported nodes — we use `_meta.title` to identify the nudity toggle
// without hard-coding a node ID.
type workflowNode struct {
	ClassType string         `json:"class_type"`
	Inputs    map[string]any `json:"inputs"`
	Meta      map[string]any `json:"_meta,omitempty"`
}

// rewritePromptAndSeed locates the sampler inside `graph` by its
// structural signature (a node with positive/negative inputs and a seed
// field), then follows its positive/negative connections to the prompt
// nodes and overwrites their `text` fields. The seed field is whichever
// of `noise_seed` (KSamplerAdvanced) or `seed` (plain KSampler) the
// sampler exposes.
//
// Discovery is structural rather than ID-based so the workflow can be
// re-exported with different node numbering without breaking the
// rewriter. We fail loudly rather than silently leaving the baked-in
// prompt in place — a render that ignores the user's prompt is a bug,
// not a fallback.
func rewritePromptAndSeed(graph map[string]workflowNode, positive, negative string, seed int64) (string, error) {
	samplerID, sampler, err := findSampler(graph)
	if err != nil {
		return "", err
	}
	switch {
	case hasInput(sampler, "noise_seed"):
		sampler.Inputs["noise_seed"] = seed
	case hasInput(sampler, "seed"):
		sampler.Inputs["seed"] = seed
	default:
		return samplerID, fmt.Errorf("sampler %s has no seed/noise_seed input", samplerID)
	}
	if err := setConnectedText(graph, sampler, "positive", positive); err != nil {
		return samplerID, fmt.Errorf("positive prompt: %w", err)
	}
	if err := setConnectedText(graph, sampler, "negative", negative); err != nil {
		return samplerID, fmt.Errorf("negative prompt: %w", err)
	}
	return samplerID, nil
}

// findSampler returns the lowest-ID node whose inputs match a sampler
// signature: positive + negative connections plus a seed field. That
// combination only matches KSampler-family nodes in practice, and works
// across KSampler, KSamplerAdvanced, SamplerCustom, and similar without a
// hand-maintained class-type whitelist. Sorted iteration keeps the choice
// deterministic when more than one match exists.
func findSampler(graph map[string]workflowNode) (string, workflowNode, error) {
	ids := make([]string, 0, len(graph))
	for id := range graph {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		n := graph[id]
		if hasInput(n, "positive") && hasInput(n, "negative") &&
			(hasInput(n, "seed") || hasInput(n, "noise_seed")) {
			return id, n, nil
		}
	}
	return "", workflowNode{}, errors.New("workflow has no KSampler-family node")
}

func hasInput(n workflowNode, key string) bool {
	_, ok := n.Inputs[key]
	return ok
}

// setNudity flips the per-turn nudity toggle inside `graph` to match the
// chat reply's `naked` flag. The target is a PrimitiveBoolean whose
// `_meta.title` is "Nudity" — typically wired to a ComfySwitchNode that
// gates the nudity LoRA. Workflows without a nudity toggle silently
// no-op: not every graph supports the switch, and the prompt-fragment
// splice upstream still pulls the renderer in the right direction.
// Returns the matched node ID (empty string when no toggle exists) so
// the caller can stamp it on a trace attribute.
func setNudity(graph map[string]workflowNode, naked bool) string {
	ids := make([]string, 0, len(graph))
	for id := range graph {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		n := graph[id]
		if n.ClassType != "PrimitiveBoolean" {
			continue
		}
		title, _ := n.Meta["title"].(string)
		if title != "Nudity" {
			continue
		}
		if _, ok := n.Inputs["value"]; ok {
			n.Inputs["value"] = naked
			return id
		}
	}
	return ""
}

// setSteps overrides the sampler step count by writing to the
// PrimitiveInt node titled "Steps" in `graph`. The bundled workflow
// wires that node into both the sampler's `steps` and `end_at_step`
// inputs so a single value drives both, the same way the nudity toggle
// drives the ComfySwitchNode. Workflows without a "Steps" node — or
// callers passing a non-positive value — silently no-op; the workflow's
// hard-coded step count then wins. Returns the matched node ID (empty
// when no override happened) for tracing.
func setSteps(graph map[string]workflowNode, steps int) string {
	if steps <= 0 {
		return ""
	}
	ids := make([]string, 0, len(graph))
	for id := range graph {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		n := graph[id]
		if n.ClassType != "PrimitiveInt" {
			continue
		}
		title, _ := n.Meta["title"].(string)
		if title != "Steps" {
			continue
		}
		if _, ok := n.Inputs["value"]; ok {
			n.Inputs["value"] = steps
			return id
		}
	}
	return ""
}

// setConnectedText follows sampler.<key> — a ComfyUI input connection of
// the form ["<node-id>", <slot>] — to its source node and rewrites that
// node's `text` field. Used to redirect the sampler's positive/negative
// inputs without caring whether they're called "6" and "7" or anything
// else, and without scanning every CLIPTextEncode in the graph.
func setConnectedText(graph map[string]workflowNode, sampler workflowNode, key, text string) error {
	conn, ok := sampler.Inputs[key].([]any)
	if !ok || len(conn) < 1 {
		return fmt.Errorf("%q input is not a connection", key)
	}
	targetID, ok := conn[0].(string)
	if !ok {
		return fmt.Errorf("%q connection has no source node id", key)
	}
	target, ok := graph[targetID]
	if !ok {
		return fmt.Errorf("%q points at missing node %s", key, targetID)
	}
	if _, ok := target.Inputs["text"]; !ok {
		return fmt.Errorf("%q source node %s (%s) has no text input", key, targetID, target.ClassType)
	}
	target.Inputs["text"] = text
	return nil
}

// Progress is one update from a streaming render. Either Total > 0 (a
// step counter) or Preview is non-empty (an intermediate frame). The
// caller decides what to do with each — typically: step → progress bar,
// preview → portrait panel.
type Progress struct {
	Step, Total int
	Preview     []byte
}

// Generate runs the bundled workflow on the configured ComfyUI server
// and returns the rendered PNG bytes. It blocks until the render finishes
// or the deadline passes — callers should run it off the UI thread, using
// the same goroutine + QTimer-poll pattern the chat and TTS paths use.
//
// onProgress (when non-nil) receives a stream of updates over the
// ComfyUI WebSocket: step counters from the "progress" event and
// intermediate preview frames as binary messages. It runs on the
// goroutine driving Generate, so a Qt-thread caller should funnel
// updates through a channel.
func Generate(ctx context.Context, s settings.AppSettings, positive, negative string, naked bool, onProgress func(Progress)) ([]byte, error) {
	ctx, span := tracing.StartSpan(ctx, "image.generate",
		attribute.Int("image.prompt.length", len(positive)),
		attribute.Int("image.negative_prompt.length", len(negative)),
		attribute.Bool("image.naked", naked),
	)
	defer span.End()

	endpoint := util.NormalizeEndpoint(s.ComfyUIEndpoint)
	if endpoint == "" {
		err := errors.New("no ComfyUI endpoint configured")
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	if strings.TrimSpace(positive) == "" {
		err := errors.New("no image prompt configured")
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	// Multi-line user settings (Settings has QTextEdits for the base
	// prompt, clothing, nudity, and negative fragments) come through with
	// embedded newlines once spliced together. CLIPTextEncode tokenizes
	// each line independently, which drops cross-line concept blending
	// and tends to leave the trailing fragments under-weighted, so flatten
	// to a single line before handing the prompts to the rewriter.
	positive = strings.ReplaceAll(positive, "\n", " ")
	negative = strings.ReplaceAll(negative, "\n", " ")

	// Parse a fresh graph per call — concurrent generations must never
	// share the mutable node maps. Wrapped in its own span so the parse +
	// structural rewrite cost is visible separately from the GPU wait;
	// when the workflow JSON grows large or the rewriter fails to find a
	// sampler, the timing/error lands here instead of getting hidden
	// inside image.generate.
	rewriteCtx, rewriteSpan := tracing.StartSpan(ctx, "image.workflow.rewrite",
		attribute.Int("workflow.json.bytes", len(defaultWorkflow)),
	)
	var graph map[string]workflowNode
	if err := json.Unmarshal([]byte(defaultWorkflow), &graph); err != nil {
		rewriteSpan.RecordError(err)
		rewriteSpan.SetStatus(codes.Error, err.Error())
		rewriteSpan.End()
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("embedded workflow is invalid: %w", err)
	}
	rewriteSpan.SetAttributes(attribute.Int("workflow.nodes", len(graph)))
	seed := randomSeed()
	samplerID, err := rewritePromptAndSeed(graph, positive, negative, seed)
	if err != nil {
		rewriteSpan.RecordError(err)
		rewriteSpan.SetStatus(codes.Error, err.Error())
		rewriteSpan.End()
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("workflow: %w", err)
	}
	rewriteSpan.SetAttributes(attribute.String("workflow.sampler_id", samplerID))
	nudityID := setNudity(graph, naked)
	if nudityID != "" {
		rewriteSpan.SetAttributes(attribute.String("workflow.nudity_id", nudityID))
	}
	stepsID := setSteps(graph, s.ImageSteps)
	if stepsID != "" {
		rewriteSpan.SetAttributes(
			attribute.String("workflow.steps_id", stepsID),
			attribute.Int("workflow.steps_value", s.ImageSteps),
		)
	}
	rewriteSpan.End()
	_ = rewriteCtx
	span.SetAttributes(
		attribute.Int64("image.seed", seed),
		attribute.String("workflow.sampler_id", samplerID),
	)
	if nudityID != "" {
		span.SetAttributes(attribute.String("workflow.nudity_id", nudityID))
	}
	if stepsID != "" {
		span.SetAttributes(attribute.String("workflow.steps_id", stepsID))
	}
	// Stderr trace of what we're actually sending. Visible when Diesel is
	// launched from a terminal, invisible in the packaged .app — useful
	// for verifying the emotion splice without adding UI noise.
	log.Printf("[comfyui] seed=%d positive=%q negative=%q", seed, positive, negative)

	// Connect the WebSocket *before* submitting the job so we don't miss
	// early "executing" / "progress" events. The same client_id ties the
	// socket to the queued prompt. The 5-minute ceiling hangs off the
	// incoming ctx so a caller cancelling its parent span (window closed,
	// new request) tears the render down immediately.
	clientID := randomHex(16)
	wsCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	conn, _, err := websocket.Dial(wsCtx, wsURLFor(endpoint, clientID), nil)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("websocket dial: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	// Preview frames can easily exceed the 32 KB default read limit.
	conn.SetReadLimit(8 << 20)

	// Queue the job.
	httpClient := tracing.HTTPClient(30 * time.Second)
	reqBody, err := json.Marshal(map[string]any{
		"prompt":    graph,
		"client_id": clientID,
	})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	postReq, err := http.NewRequestWithContext(ctx, "POST", endpoint+"/prompt", bytes.NewReader(reqBody))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	postReq.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(postReq)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	promptID, err := decodePromptID(resp)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.AddEvent("queued", trace.WithAttributes(attribute.String("comfyui.prompt_id", promptID)))

	// Drain WS until we either see SaveImage's executed event (carries the
	// final filename), the prompt finishes (no more nodes to execute), or
	// the server reports an error. Messages for other clients are filtered
	// out by prompt_id — the WS broadcasts everything to all listeners.
	var finalRef imageRef
	var previewCount, stepCount int
	finishRender := func(ref imageRef) ([]byte, error) {
		span.SetAttributes(
			attribute.Int("image.preview.frames", previewCount),
			attribute.Int("image.steps", stepCount),
			attribute.String("image.filename", ref.Filename),
		)
		span.AddEvent("render.finished")
		png, err := fetchImage(ctx, httpClient, endpoint, ref)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return nil, err
		}
		span.SetAttributes(attribute.Int("image.png.bytes", len(png)))
		return png, nil
	}
	for {
		msgType, data, err := conn.Read(wsCtx)
		if err != nil {
			err = fmt.Errorf("websocket read: %w", err)
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return nil, err
		}
		switch msgType {
		case websocket.MessageText:
			var msg struct {
				Type string          `json:"type"`
				Data json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}
			switch msg.Type {
			case "progress":
				var p struct {
					Value    int    `json:"value"`
					Max      int    `json:"max"`
					PromptID string `json:"prompt_id"`
				}
				if json.Unmarshal(msg.Data, &p) == nil && p.PromptID == promptID {
					stepCount = p.Value
					span.AddEvent("progress", trace.WithAttributes(
						attribute.Int("step", p.Value),
						attribute.Int("total", p.Max),
					))
					if onProgress != nil {
						onProgress(Progress{Step: p.Value, Total: p.Max})
					}
				}
			case "executed":
				// Sent once per node that produced outputs. Latch the
				// last image-bearing one so we have a filename ready
				// when the prompt finishes.
				var ev struct {
					PromptID string `json:"prompt_id"`
					Node     string `json:"node"`
					Output   struct {
						Images []imageRef `json:"images"`
					} `json:"output"`
				}
				if json.Unmarshal(msg.Data, &ev) == nil && ev.PromptID == promptID {
					for _, img := range ev.Output.Images {
						if img.Filename != "" {
							finalRef = img
							span.AddEvent("executed", trace.WithAttributes(
								attribute.String("node", ev.Node),
								attribute.String("filename", img.Filename),
							))
						}
					}
				}
			case "executing":
				// `executing` with a null node = the prompt is done.
				// Some ComfyUI versions also emit `execution_success`
				// — handled below.
				var ev struct {
					PromptID string  `json:"prompt_id"`
					Node     *string `json:"node"`
				}
				if json.Unmarshal(msg.Data, &ev) == nil && ev.PromptID == promptID && ev.Node == nil {
					if finalRef.Filename == "" {
						err := errors.New("render finished but no image was produced")
						span.SetStatus(codes.Error, err.Error())
						return nil, err
					}
					return finishRender(finalRef)
				}
			case "execution_success":
				var ev struct {
					PromptID string `json:"prompt_id"`
				}
				if json.Unmarshal(msg.Data, &ev) == nil && ev.PromptID == promptID {
					if finalRef.Filename == "" {
						err := errors.New("render finished but no image was produced")
						span.SetStatus(codes.Error, err.Error())
						return nil, err
					}
					return finishRender(finalRef)
				}
			case "execution_error":
				var ev struct {
					PromptID  string `json:"prompt_id"`
					Exception string `json:"exception_message"`
				}
				_ = json.Unmarshal(msg.Data, &ev)
				m := strings.TrimSpace(ev.Exception)
				if m == "" {
					m = "ComfyUI reported a render error"
				}
				err := errors.New(m)
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
				return nil, err
			}
		case websocket.MessageBinary:
			// ComfyUI preview frame format: [u32 event_type][u32 image_format][bytes...]
			// event_type 1 = preview, image_format 1 = JPEG, 2 = PNG.
			// Both are decodable by QPixmap.LoadFromDataWithData, so we
			// just forward the payload.
			if len(data) >= 8 && onProgress != nil {
				if binary.BigEndian.Uint32(data[0:4]) == 1 {
					previewCount++
					onProgress(Progress{Preview: data[8:]})
				}
			}
		}
	}
}

// decodePromptID reads the /prompt response. ComfyUI answers a rejected
// graph with 400 and a node_errors blob — surfaced here so a bad checkpoint
// name or missing node fails loudly instead of hanging the poll loop.
func decodePromptID(resp *http.Response) (string, error) {
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", util.HTTPStatusError(resp, 8192)
	}
	var payload struct {
		PromptID string `json:"prompt_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}
	if payload.PromptID == "" {
		return "", errors.New("ComfyUI accepted the job but returned no prompt_id")
	}
	return payload.PromptID, nil
}

// imageRef points at one rendered image in ComfyUI's output store.
type imageRef struct {
	Filename  string `json:"filename"`
	Subfolder string `json:"subfolder"`
	Type      string `json:"type"`
}

// wsURLFor builds the WebSocket URL ComfyUI uses for live render events,
// translating http→ws / https→wss against the configured REST endpoint.
func wsURLFor(endpoint, clientID string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return endpoint
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	default:
		u.Scheme = "ws"
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/ws"
	q := u.Query()
	q.Set("clientId", clientID)
	u.RawQuery = q.Encode()
	return u.String()
}

// fetchImage downloads a rendered image from ComfyUI's /view endpoint.
func fetchImage(ctx context.Context, client *http.Client, endpoint string, ref imageRef) ([]byte, error) {
	ctx, span := tracing.StartSpan(ctx, "image.fetch",
		attribute.String("image.filename", ref.Filename),
		attribute.String("image.subfolder", ref.Subfolder),
		attribute.String("image.type", ref.Type),
	)
	defer span.End()

	q := url.Values{}
	q.Set("filename", ref.Filename)
	q.Set("subfolder", ref.Subfolder)
	q.Set("type", ref.Type)
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint+"/view?"+q.Encode(), nil)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("view HTTP %d", resp.StatusCode)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetAttributes(attribute.Int("image.bytes", len(body)))
	return body, nil
}

// FetchCheckpoints lists the checkpoint files the server can load, for
// the settings dialog's model dropdown. ComfyUI describes every node's
// inputs via /object_info; CheckpointLoaderSimple's ckpt_name input is a
// tuple whose first element is the list of available checkpoints.
func FetchCheckpoints(endpoint string) ([]string, error) {
	endpoint = util.NormalizeEndpoint(endpoint)
	if endpoint == "" {
		return nil, errors.New("no endpoint configured")
	}
	client := tracing.HTTPClient(6 * time.Second)
	resp, err := client.Get(endpoint + "/object_info/CheckpointLoaderSimple")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, util.HTTPStatusError(resp, 512)
	}
	var payload map[string]struct {
		Input struct {
			Required map[string]json.RawMessage `json:"required"`
		} `json:"input"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	raw, ok := payload["CheckpointLoaderSimple"].Input.Required["ckpt_name"]
	if !ok {
		return nil, errors.New("server did not describe CheckpointLoaderSimple")
	}
	// ckpt_name is ["[name, name, ...]", {options...}] — only the first
	// tuple element is the name list.
	var tuple []json.RawMessage
	if err := json.Unmarshal(raw, &tuple); err != nil || len(tuple) == 0 {
		return nil, errors.New("unexpected ckpt_name schema")
	}
	var names []string
	if err := json.Unmarshal(tuple[0], &names); err != nil {
		return nil, errors.New("unexpected ckpt_name schema")
	}
	return names, nil
}

// TestConnection probes the endpoint for the settings dialog. It hits
// /system_stats (cheap, always present) to confirm reachability, then
// reports how many checkpoints are installed so the user knows the model
// dropdown will have something in it.
func TestConnection(endpoint string) string {
	endpoint = util.NormalizeEndpoint(endpoint)
	if endpoint == "" {
		return "✗ No endpoint configured."
	}
	client := tracing.HTTPClient(6 * time.Second)
	resp, err := client.Get(endpoint + "/system_stats")
	if err != nil {
		return "✗ " + err.Error()
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Sprintf("✗ HTTP %d", resp.StatusCode)
	}
	ckpts, err := FetchCheckpoints(endpoint)
	if err != nil {
		return "✓ Connected, but couldn't list checkpoints: " + err.Error()
	}
	if len(ckpts) == 0 {
		return "✓ Connected, but no checkpoints are installed."
	}
	return fmt.Sprintf("✓ Connected — %d checkpoint(s) available.", len(ckpts))
}

// randomSeed returns a non-negative seed for KSampler. A fresh seed per
// call is what makes "regenerate after every reply" produce a new image
// rather than the same one each time.
func randomSeed() int64 {
	// 2^53 keeps the value exactly representable if it ever round-trips
	// through a float (JSON number) on the ComfyUI side.
	n, err := rand.Int(rand.Reader, big.NewInt(1<<53))
	if err != nil {
		return time.Now().UnixNano() & (1<<53 - 1)
	}
	return n.Int64()
}

// randomHex returns n random bytes hex-encoded, used for the per-request
// ComfyUI client_id.
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// CharacterImagePath is where the most recent rendered portrait is cached,
// a sibling of settings.json and conversation.json. Restored on launch so
// the window opens with Diesel's face already in place.
func CharacterImagePath() (string, error) {
	return util.ConfigFilePath("character.png")
}

// SaveCharacterImage caches `png` to CharacterImagePath.
func SaveCharacterImage(png []byte) error {
	path, err := CharacterImagePath()
	if err != nil {
		return err
	}
	return util.AtomicWriteFile(path, png, 0o644)
}
