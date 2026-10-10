package config

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
)

var (
	pathPattern   = regexp.MustCompile(`^[a-zA-Z0-9_-]+(\.[a-zA-Z0-9_-]+)*$`)
	appIDPattern  = regexp.MustCompile(`^[a-z][a-z0-9]*(\.[a-z0-9-]+)+$`)
	allowedThemes = map[string]bool{
		"nostalgia":           true,
		"midnight":            true,
		"monochrome":          true,
		"high-contrast":       true,
		"high-contrast-light": true,
	}
	allowedColorModes = map[string]bool{
		"plain":     true,
		"ansi16":    true,
		"ansi256":   true,
		"truecolor": true,
	}
	allowedStartupViews = map[string]bool{
		"home":     true,
		"launcher": true,
		"prompt":   true,
	}
)

// themeValidator extends theme validation with names resolved at runtime
// (user themes under /config/themes). It is a process-wide hook because
// Validate is a pure function with no access to the environment
// filesystem; the theme service registers it at boot.
var themeValidator = struct {
	sync.RWMutex
	fn func(name string) bool
}{}

// SetThemeValidator registers the lookup Validate uses for theme names
// beyond the built-in set. Passing nil restores built-in-only validation.
func SetThemeValidator(fn func(name string) bool) {
	themeValidator.Lock()
	defer themeValidator.Unlock()
	themeValidator.fn = fn
}

// BuiltinThemeNames returns the sorted built-in theme identifiers.
func BuiltinThemeNames() []string {
	names := make([]string, 0, len(allowedThemes))
	for name := range allowedThemes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ValidThemeName reports whether name selects a theme: a built-in or a
// name the registered validator recognizes.
func ValidThemeName(name string) bool {
	if allowedThemes[name] {
		return true
	}
	themeValidator.RLock()
	fn := themeValidator.fn
	themeValidator.RUnlock()
	return fn != nil && fn(name)
}

// KeybindingConflict describes two or more actions bound to the same key chord.
type KeybindingConflict struct {
	Key     string   `json:"key"`
	Actions []string `json:"actions"`
}

// KeybindingConflictError reports conflicting shortcut bindings.
type KeybindingConflictError struct {
	Conflicts []KeybindingConflict `json:"conflicts"`
}

func (e *KeybindingConflictError) Error() string {
	var parts []string
	for _, c := range e.Conflicts {
		parts = append(parts, fmt.Sprintf("key %q is assigned to multiple actions: %s", c.Key, strings.Join(c.Actions, ", ")))
	}
	return "config: keybinding conflicts: " + strings.Join(parts, "; ")
}

// DetectKeybindingConflicts scans a shortcuts map (action -> key chord) and returns any collisions.
func DetectKeybindingConflicts(shortcuts map[string]string) []KeybindingConflict {
	byKey := make(map[string][]string)
	for action, key := range shortcuts {
		normKey := strings.ToLower(strings.TrimSpace(key))
		if normKey == "" {
			continue
		}
		byKey[normKey] = append(byKey[normKey], action)
	}

	var conflicts []KeybindingConflict
	for key, actions := range byKey {
		if len(actions) > 1 {
			sort.Strings(actions)
			conflicts = append(conflicts, KeybindingConflict{
				Key:     key,
				Actions: actions,
			})
		}
	}
	sort.Slice(conflicts, func(i, j int) bool {
		return conflicts[i].Key < conflicts[j].Key
	})
	return conflicts
}

// Validate validates a single path and value according to system configuration schema.
func Validate(path string, val any) error {
	if !pathPattern.MatchString(path) {
		return fmt.Errorf("config: invalid path format %q", path)
	}

	switch path {
	case "theme":
		str, ok := val.(string)
		if !ok || !ValidThemeName(strings.ToLower(str)) {
			return fmt.Errorf("config: invalid theme %v (must be a built-in theme or a theme defined in /config/themes)", val)
		}
	case "accessibility.reduced_motion", "accessibility.high_contrast", "accessibility.screen_reader":
		if _, ok := val.(bool); !ok {
			return fmt.Errorf("config: %s must be boolean", path)
		}
	case "accessibility.color_mode":
		str, ok := val.(string)
		if !ok || !allowedColorModes[strings.ToLower(str)] {
			return fmt.Errorf("config: invalid color_mode %v (must be one of: plain, ansi16, ansi256, truecolor)", val)
		}
	case "startup.view":
		str, ok := val.(string)
		if !ok || !allowedStartupViews[strings.ToLower(str)] {
			return fmt.Errorf("config: invalid startup.view %v (must be one of: home, launcher, prompt)", val)
		}
	case "startup.app":
		str, ok := val.(string)
		if !ok {
			return fmt.Errorf("config: startup.app must be string")
		}
		if str != "" && !appIDPattern.MatchString(str) {
			return fmt.Errorf("config: invalid startup.app %q (must be reverse-DNS ID)", str)
		}
	case "startup.restore_session", "notifications.dnd":
		if _, ok := val.(bool); !ok {
			return fmt.Errorf("config: %s must be boolean", path)
		}
	case "notifications.toast_limit":
		switch n := val.(type) {
		case int:
			if n < 0 {
				return fmt.Errorf("config: notifications.toast_limit must be >= 0")
			}
		case float64:
			if n < 0 || n != float64(int(n)) {
				return fmt.Errorf("config: notifications.toast_limit must be a non-negative integer")
			}
		default:
			return fmt.Errorf("config: notifications.toast_limit must be an integer")
		}
	default:
		if strings.HasPrefix(path, "shortcuts.") {
			str, ok := val.(string)
			if !ok || strings.TrimSpace(str) == "" {
				return fmt.Errorf("config: shortcut %s must be a non-empty key chord string", path)
			}
		}
	}

	return nil
}

// FlattenMap flattens a nested map into dotted paths ("a.b.c" -> value).
func FlattenMap(prefix string, m map[string]any, out map[string]any) {
	for k, v := range m {
		fullPath := k
		if prefix != "" {
			fullPath = prefix + "." + k
		}
		if childMap, ok := v.(map[string]any); ok {
			FlattenMap(fullPath, childMap, out)
		} else {
			out[fullPath] = v
		}
	}
}

// ValidateBatch validates multiple settings and checks for shortcut conflicts.
func ValidateBatch(settings map[string]any) error {
	flat := make(map[string]any)
	FlattenMap("", settings, flat)

	shortcuts := make(map[string]string)
	for p, v := range flat {
		if err := Validate(p, v); err != nil {
			return err
		}
		if strings.HasPrefix(p, "shortcuts.") {
			action := strings.TrimPrefix(p, "shortcuts.")
			if s, ok := v.(string); ok {
				shortcuts[action] = s
			}
		}
	}

	if len(shortcuts) > 1 {
		conflicts := DetectKeybindingConflicts(shortcuts)
		if len(conflicts) > 0 {
			return &KeybindingConflictError{Conflicts: conflicts}
		}
	}

	return nil
}
