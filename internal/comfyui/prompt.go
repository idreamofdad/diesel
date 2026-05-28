package comfyui

// This file collects every hardcoded prompt fragment the portrait
// pipeline relies on. Splitting them out of comfyui.go keeps the
// rendering logic and the prompt text on separate pages — tweaking how
// Diesel looks doesn't require scrolling past the ComfyUI graph
// rewriter, and vice versa.

// ImagePrompt is the base positive prompt describing how Diesel should
// look. Tuned for the checkpoint baked into default_workflow.json. The
// hub splices ImageClothing or ImageNudity onto the end of this before
// handing it to Generate, then appends an emotion fragment from
// EmotionPrompts.
const ImagePrompt = `solo, dubusi, ochman, fat man, hairy, braided beard, short green hair, green eyes, living room`

// ImageClothing is appended to ImagePrompt when the structured reply's
// Naked flag is false. Kept separate so the splice can swap it for
// ImageNudity — a hard-coded outfit in the base prompt would fight the
// nudity splice and confuse the renderer.
const ImageClothing = `blue t-shirt, blue jeans,`

// ImageNudity is appended to ImagePrompt in place of ImageClothing when
// the reply's Naked flag is true.
const ImageNudity = `naked, small penis, flaccid, uncut, uncircumcised, foreskin, green pubic hair, green chest hair, small nipples,`

// ImageNegativePrompt steers the renderer away from the usual diffusion
// failure modes. Read directly by Generate.
const ImageNegativePrompt = `woman, girl, shirt logo, feminine, wide hips,`

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
