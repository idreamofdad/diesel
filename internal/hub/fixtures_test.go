package hub

import "diesel/internal/settings"

// settingsFixture returns an AppSettings with image-prompt parts set to
// recognizable sentinels so the splice tests can assert which fragment
// landed where without depending on the real prompts.
func settingsFixture() settings.AppSettings {
	return settings.AppSettings{
		ImagePrompt:   "BASE",
		ImageClothing: "CLOTHING",
		ImageNudity:   "NUDE",
	}
}
