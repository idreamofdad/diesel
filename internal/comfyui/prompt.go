package comfyui

// This file collects every hardcoded prompt fragment the portrait
// pipeline relies on. Splitting them out of comfyui.go keeps the
// rendering logic and the prompt text on separate pages — tweaking how
// Diesel looks doesn't require scrolling past the ComfyUI graph
// rewriter, and vice versa.

// ImageQualityPrefix is prepended to every composed prompt. Illustrious
// / NoobAI-XL checkpoints respond well to a short quality block at the
// front of the tag list — the renderer treats the leading tokens as
// the strongest steering signal.
const ImageQualityPrefix = `masterpiece, best quality, very aesthetic, absurdres, newest`

// ImagePrompt is the base positive prompt describing how Diesel should
// look. Tuned for the checkpoint baked into default_workflow.json. The
// hub splices ImageClothing or ImageNudity onto the end of this before
// handing it to Generate, then appends an emotion fragment from
// EmotionPrompts. The background tag is sourced from ImageBackgrounds
// per turn — kept out of here so the scene can swap with the chat.
const ImagePrompt = `solo, dubusi, ochman, fat man, hairy, braided beard, mustache, short green hair, green eyes`

// ImageClothing is appended to ImagePrompt when the structured reply's
// Naked flag is false. Kept separate so the splice can swap it for
// ImageNudity — a hard-coded outfit in the base prompt would fight the
// nudity splice and confuse the renderer.
const ImageClothing = `blue t-shirt, blue jeans`

// ImageNudity is appended to ImagePrompt in place of ImageClothing when
// the reply's Naked flag is true.
const ImageNudity = `naked, small penis, flaccid, uncut, uncircumcised, foreskin, green pubic hair, green chest hair, small nipples, small testicles`

// ImageNegativePrompt steers the renderer away from the usual diffusion
// failure modes. Combines Diesel-specific avoidances (he's male, so
// reject feminine tags) with the Illustrious quality-failure baseline
// (lo-res, deformed anatomy, watermarks, etc.). Read directly by
// Generate.
const ImageNegativePrompt = `woman, girl, shirt logo, feminine, wide hips, worst quality, low quality, lowres, bad anatomy, bad hands, extra fingers, deformed, blurry, watermark, signature, text, jpeg artifacts, ponytail`

// SceneSpec pairs a human-readable label with the tag list that ends up
// in the prompt. Label is used for continuity reminders fed back to the
// model ("you were last shown in: the pub"); Tags is the comma-separated
// fragment spliced into the composed prompt.
type SceneSpec struct {
	Label string
	Tags  string
}

// ImageBackgrounds enumerates the scene options Diesel can be rendered
// in. The slug keys are the values the chat schema constrains the model
// to choose from; any addition here must show up in chat.Backgrounds and
// gain a row in ImagePoseAddons for every pose. The `scenery` tag at the
// tail of each Tags string is Illustrious-specific shorthand that asks
// the renderer to treat the block as environment rather than as floating
// props near the subject.
var ImageBackgrounds = map[string]SceneSpec{
	"living_room": {
		Label: "the living room",
		Tags:  "indoors, living room, couch, sofa, coffee table, window, curtains, table lamp, rug, indoor plant, picture frame, wooden floor, throw pillow, bookshelf, evening, warm lighting, scenery",
	},
	"mechanics_shop": {
		Label: "the car mechanics shop",
		Tags:  "indoors, garage, auto repair shop, car, tools, tool box, wrench, pegboard, oil stain, concrete floor, workbench, tire, ladder, fluorescent lighting, hydraulic lift, industrial, scenery",
	},
	"forest_park": {
		Label: "the forest park",
		Tags:  "outdoors, forest, tree, path, grass, wildflower, dappled sunlight, sunbeam, moss, leaf, fern, day, nature, depth of field, scenery",
	},
	"pub": {
		Label: "the pub",
		Tags:  "indoors, bar (place), bar stool, counter, bottle, alcohol, beer tap, dim lighting, wooden wall, pendant light, warm light, pub, shelf, scenery",
	},
}

// ImagePoseBases enumerates the body postures Diesel can be rendered in.
// Each Base is the pose-only tag block; per-scene flavor (props, eye
// lines, prop interactions) lives in ImagePoseAddons. The original
// matrix in sd_prompts_backgrounds_poses.md leads with `1girl, solo` —
// stripped here because ImagePrompt already establishes the subject and
// ImageNegativePrompt rejects feminine cues; reintroducing `1girl` would
// fight both.
var ImagePoseBases = map[string]SceneSpec{
	"standing": {
		Label: "standing",
		Tags:  "standing, full body, looking at viewer, contrapposto",
	},
	"sitting": {
		Label: "sitting",
		Tags:  "sitting, full body, looking at viewer",
	},
	"bent_over": {
		Label: "bent over, viewed from behind",
		Tags:  "from behind, bent over, leaning forward, back view, full body",
	},
}

// ImagePoseAddons holds the per-scene flavor tags spliced onto a pose
// base. Keyed pose → background → tags. Every (pose, background) pair
// must have an entry; chat_test guards this so a missing cell fails
// loudly rather than silently rendering a generic pose without scene
// interaction.
var ImagePoseAddons = map[string]map[string]string{
	"standing": {
		"living_room":    "hand on hip, relaxed, smile",
		"mechanics_shop": "holding rag, hand on hip, confident",
		"forest_park":    "hand in own hair, smile, head tilt",
		"pub":            "leaning on counter, holding cup, looking back, looking at viewer",
	},
	"sitting": {
		"living_room":    "sitting on couch, legs to the side, holding cup, leaning on arm",
		"mechanics_shop": "sitting on tool box, legs apart, holding wrench, dangling legs",
		"forest_park":    "sitting on log, hands on lap, crossed legs, closed eyes, smile",
		"pub":            "sitting on bar stool, elbow rest, head rest, holding cup",
	},
	"bent_over": {
		"living_room":    "picking up, book, coffee table, hair down",
		"mechanics_shop": "holding wrench, car hood, leaning into engine",
		"forest_park":    "reaching out, flower, picking flower, fern",
		"pub":            "reaching, counter, bar (place), arm support",
	},
}

// DefaultImageBackground is the scene used when no background can be
// inherited from prior history — i.e. the very first turn renders here
// if the model fails to return a structured reply. Subsequent fallbacks
// inherit from lastBackground.
const DefaultImageBackground = "living_room"

// DefaultImagePose is the pose counterpart to DefaultImageBackground —
// same first-turn-only fallback semantics.
const DefaultImagePose = "standing"

// EmotionPrompts maps each chat-reply emotion to the prompt fragment
// spliced onto the end of the image prompt to steer the portrait's
// expression. Values are tuned as SD-style comma-separated tag lists
// rather than bare adjectives so the renderer has concrete features to
// latch onto (mouth shape, eye state, brow position). An empty value
// (neutral) skips the splice and renders the base prompt unchanged.
// Keys must match chat.Emotions one-for-one; chat_test.go guards that.
var EmotionPrompts = map[string]string{
	"happy":             "warm smile, bright eyes, cheerful expression",
	"sad":               "downturned mouth, sorrowful eyes, slight tear, melancholy expression",
	"angry":             "furrowed brow, scowl, gritted teeth, angry expression",
	"surprised happy":   "wide delighted eyes, open smiling mouth, raised eyebrows, pleasantly surprised expression",
	"surprised shocked": "wide shocked eyes, mouth agape, raised eyebrows, alarmed expression",
	"laughing":          "head tilted back, mouth wide open laughing, squinted eyes, joyful laughter",
	"neutral":           "",
	"amused":            "subtle smirk, raised eyebrow, glint in the eyes, amused expression",
	"annoyed":           "narrowed eyes, slight frown, pursed lips, annoyed expression",
	"thoughtful":        "hand on chin, distant gaze, slightly furrowed brow, contemplative expression",
	"flirtatious":       "half-lidded eyes, playful smirk, raised eyebrow, flirtatious expression",
	"horny":             "flushed cheeks, half-lidded eyes, parted lips, biting lower lip, aroused expression, smirk",
}
