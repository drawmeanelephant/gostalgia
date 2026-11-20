package config

// DefaultSettings returns a copy of Gostalgia's built-in default configuration.
func DefaultSettings() map[string]any {
	return map[string]any{
		"theme": "nostalgia",
		"accessibility": map[string]any{
			"reduced_motion": false,
			"high_contrast":  false,
			"color_mode":     "ansi256",
			"screen_reader":  false,
		},
		"startup": map[string]any{
			"view":            "home",
			"app":             "",
			"restore_session": false,
		},
		"notifications": map[string]any{
			"dnd":         false,
			"toast_limit": 3,
		},
		"shortcuts": map[string]any{
			"home":          "f1",
			"launcher":      "f2",
			"stop":          "f3",
			"view":          "f4",
			"tasks":         "f5",
			"notifications": "f6",
			"settings":      "f7",
			"window_next":   "f8",
			"palette":       "ctrl+p",
		},
	}
}
