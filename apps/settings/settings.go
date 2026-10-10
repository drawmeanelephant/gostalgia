// Package settings is Gostalgia's interactive system preferences application.
// It provides a keyboard-first configuration browser adhering to the SDK
// presentation contract (sdk.View and sdk.ActionRequest), allowing users to
// inspect and customize themes, accessibility modes, shortcuts with conflict
// detection, startup behavior, and layered storage.
package settings

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"gostalgia/sdk"
)

const (
	// ID is the reverse-DNS identifier for Settings.
	ID = "com.gostalgia.settings"
)

//go:embed manifest.json
var manifestJSON []byte

// Manifest returns the validated manifest declaration.
func Manifest() sdk.Manifest {
	m, err := sdk.ParseManifest(manifestJSON)
	if err != nil {
		panic(err)
	}
	return m
}

// ManifestJSON returns a copy of the raw embedded JSON.
func ManifestJSON() []byte {
	return append([]byte(nil), manifestJSON...)
}

// Category represents a settings screen category.
type Category string

const (
	CategoryAppearance    Category = "appearance"
	CategoryAccessibility Category = "accessibility"
	CategoryShortcuts     Category = "shortcuts"
	CategoryStartup       Category = "startup"
	CategoryStorage       Category = "storage"
)

// userTheme is a user-loadable theme discovered through the theme service
// (files under /config/themes).
type userTheme struct {
	name        string
	description string
}

// Settings implements the preferences presentation application.
type Settings struct {
	app *sdk.Context
	mu  sync.Mutex

	category     Category
	selectedItem string

	theme         string
	previewTheme  string
	userThemes    []userTheme
	colorMode     string
	reducedMotion bool
	highContrast  bool
	screenReader  bool

	startupView    string
	startupApp     string
	restoreSession bool
	dnd            bool

	shortcuts map[string]string

	statusMessage string
	errorMessage  string
}

// Factory allocates a new Settings application instance.
func Factory() (sdk.Instance, error) {
	return &Settings{
		category:     CategoryAppearance,
		selectedItem: "theme_nostalgia",
		theme:        "nostalgia",
		colorMode:    "ansi256",
		startupView:  "home",
		shortcuts:    make(map[string]string),
	}, nil
}

// Init runs before routes become visible.
func (s *Settings) Init(appCtx *sdk.Context) error {
	s.app = appCtx

	// Register programmatic routes
	if err := appCtx.Handle("preferences", s.handlePreferences); err != nil {
		return err
	}
	if err := appCtx.Handle("preview", s.handlePreview); err != nil {
		return err
	}
	if err := appCtx.Handle("save", s.handleSave); err != nil {
		return err
	}
	if err := appCtx.Handle("revert", s.handleCancel); err != nil {
		return err
	}
	if err := appCtx.Handle("reset", s.handleReset); err != nil {
		return err
	}

	// Load initial preferences from environment config service
	s.loadFromConfig(context.Background())

	// Register presentation contract
	return appCtx.Present(s.snapshot, s.handleAction)
}

// Run blocks until app context is cancelled.
func (s *Settings) Run(ctx context.Context) error {
	s.app.Log.Info("settings running")
	<-ctx.Done()
	return nil
}

// Stop releases any resources held by Settings.
func (s *Settings) Stop(ctx context.Context) error {
	return nil
}

func (s *Settings) loadFromConfig(ctx context.Context) {
	var resp struct {
		Effective map[string]any `json:"effective"`
		Layers    map[string]any `json:"layers"`
	}
	if err := s.app.Call(ctx, "config/list", nil, &resp); err == nil && resp.Effective != nil {
		eff := resp.Effective
		if t, ok := eff["theme"].(string); ok {
			s.theme = t
		}
		if acc, ok := eff["accessibility"].(map[string]any); ok {
			if rm, ok := acc["reduced_motion"].(bool); ok {
				s.reducedMotion = rm
			}
			if hc, ok := acc["high_contrast"].(bool); ok {
				s.highContrast = hc
			}
			if cm, ok := acc["color_mode"].(string); ok {
				s.colorMode = cm
			}
			if sr, ok := acc["screen_reader"].(bool); ok {
				s.screenReader = sr
			}
		}
		if st, ok := eff["startup"].(map[string]any); ok {
			if v, ok := st["view"].(string); ok {
				s.startupView = v
			}
			if a, ok := st["app"].(string); ok {
				s.startupApp = a
			}
			if rs, ok := st["restore_session"].(bool); ok {
				s.restoreSession = rs
			}
		}
		if sc, ok := eff["shortcuts"].(map[string]any); ok {
			for k, v := range sc {
				if str, ok := v.(string); ok {
					s.shortcuts[k] = str
				}
			}
		}
		if notif, ok := eff["notifications"].(map[string]any); ok {
			if d, ok := notif["dnd"].(bool); ok {
				s.dnd = d
			}
		}
	}
}

// refreshThemesLocked re-reads the theme registry so the picker lists
// user themes dropped into /config/themes alongside the built-ins. A
// missing theme service or a failed call keeps the last known list.
func (s *Settings) refreshThemesLocked(ctx context.Context) {
	var resp struct {
		Themes []struct {
			Name        string `json:"name"`
			Source      string `json:"source"`
			Description string `json:"description"`
		} `json:"themes"`
	}
	if err := s.app.Call(ctx, "theme/list", nil, &resp); err != nil {
		return
	}
	var users []userTheme
	for _, t := range resp.Themes {
		if t.Source == "user" {
			users = append(users, userTheme{name: t.Name, description: t.Description})
		}
	}
	s.userThemes = users
}

func (s *Settings) snapshot(ctx context.Context) (sdk.View, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.refreshThemesLocked(ctx)

	state := sdk.ViewReady
	if s.errorMessage != "" {
		state = sdk.ViewError
	}

	title := fmt.Sprintf("Settings - %s", s.categoryTitle())
	status := s.buildStatus()

	items := s.buildItems()
	fields := s.buildFields()
	actions := s.buildActions()

	return sdk.View{
		Title:   title,
		State:   state,
		Status:  status,
		Error:   s.errorMessage,
		Items:   items,
		Fields:  fields,
		Actions: actions,
	}, nil
}

func (s *Settings) categoryTitle() string {
	switch s.category {
	case CategoryAppearance:
		return "Appearance & Theme"
	case CategoryAccessibility:
		return "Accessibility & Motion"
	case CategoryShortcuts:
		return "Shortcuts & Keybindings"
	case CategoryStartup:
		return "Startup & Views"
	case CategoryStorage:
		return "Preferences Storage"
	default:
		return "General"
	}
}

func (s *Settings) buildStatus() string {
	if s.statusMessage != "" {
		return s.statusMessage
	}
	currentTheme := s.theme
	if s.previewTheme != "" {
		currentTheme = s.previewTheme + " (PREVIEWING - Unsaved)"
	}
	rm := "off"
	if s.reducedMotion {
		rm = "on"
	}
	return fmt.Sprintf("Theme: %s · Color: %s · Reduced Motion: %s", currentTheme, s.colorMode, rm)
}

func (s *Settings) buildItems() []sdk.Item {
	var items []sdk.Item
	switch s.category {
	case CategoryAppearance:
		themes := []struct {
			name, label, desc string
		}{
			{"nostalgia", "Nostalgia (Default)", "Warm cream/sand surface, slate text, amber accents"},
			{"midnight", "Midnight", "Cool slate canvas, rich blue headers, subdued dark styling"},
			{"monochrome", "Monochrome", "High-compatibility pure ASCII borders, zero ANSI color escapes"},
			{"high-contrast", "High Contrast Dark", "Pure black background, bright white text, vivid yellow focus"},
			{"high-contrast-light", "High Contrast Light", "Pure white canvas, pure black text, deep navy headers"},
		}
		for _, t := range themes {
			tag := ""
			if s.previewTheme != "" && s.previewTheme == t.name {
				tag = " [PREVIEWING]"
			} else if s.previewTheme == "" && s.theme == t.name {
				tag = " [ACTIVE]"
			}
			items = append(items, sdk.Item{
				ID:     "theme_" + t.name,
				Label:  t.label + tag,
				Detail: t.desc,
			})
		}
		for _, t := range s.userThemes {
			tag := ""
			if s.previewTheme != "" && s.previewTheme == t.name {
				tag = " [PREVIEWING]"
			} else if s.previewTheme == "" && s.theme == t.name {
				tag = " [ACTIVE]"
			}
			detail := t.description
			if detail == "" {
				detail = "User theme loaded from /config/themes"
			}
			items = append(items, sdk.Item{
				ID:     "theme_" + t.name,
				Label:  t.name + tag,
				Detail: detail,
			})
		}

	case CategoryAccessibility:
		rmState := "[OFF] Standard fluid transitions"
		if s.reducedMotion {
			rmState = "[ON] Reduced motion enabled"
		}
		items = append(items, sdk.Item{
			ID:     "toggle_motion",
			Label:  "Reduced Motion",
			Detail: rmState,
		})

		modes := []struct {
			id, label, desc string
		}{
			{"mode_ansi256", "Color Mode: ANSI-256", "Standard terminal color palette (default)"},
			{"mode_truecolor", "Color Mode: TrueColor", "24-bit direct RGB color support"},
			{"mode_ansi16", "Color Mode: ANSI-16", "Basic 16 terminal ANSI colors"},
			{"mode_plain", "Color Mode: Plain", "Pure text rendering with ASCII borders, no escape sequences"},
		}
		for _, m := range modes {
			tag := ""
			modeKey := strings.TrimPrefix(m.id, "mode_")
			if s.colorMode == modeKey {
				tag = " [ACTIVE]"
			}
			items = append(items, sdk.Item{
				ID:     m.id,
				Label:  m.label + tag,
				Detail: m.desc,
			})
		}

	case CategoryShortcuts:
		actions := []struct {
			action, label, defKey string
		}{
			{"home", "Home Dashboard", "f1"},
			{"launcher", "App Shelf Launcher", "f2"},
			{"stop", "Stop Active App", "f3"},
			{"view", "App Presentation View", "f4"},
			{"tasks", "Task Manager", "f5"},
			{"notifications", "Notifications & Alerts", "f6"},
			{"settings", "Preferences & Settings", "f7"},
			{"palette", "Command Palette", "ctrl+p"},
		}
		for _, a := range actions {
			currentKey := s.shortcuts[a.action]
			if currentKey == "" {
				currentKey = a.defKey
			}
			items = append(items, sdk.Item{
				ID:     "sc_" + a.action,
				Label:  fmt.Sprintf("%s [%s]", a.label, strings.ToUpper(currentKey)),
				Detail: fmt.Sprintf("Bound key: %s (default: %s)", currentKey, a.defKey),
			})
		}

	case CategoryStartup:
		views := []struct {
			id, label, desc string
		}{
			{"start_home", "Startup View: Home", "Open the Home screen with dashboard and document shortcuts"},
			{"start_launcher", "Startup View: App Shelf", "Open the App Shelf launcher directly on startup"},
			{"start_prompt", "Startup View: Prompt", "Start at the interactive command center prompt"},
		}
		for _, v := range views {
			tag := ""
			viewKey := strings.TrimPrefix(v.id, "start_")
			if s.startupView == viewKey {
				tag = " [ACTIVE]"
			}
			items = append(items, sdk.Item{
				ID:     v.id,
				Label:  v.label + tag,
				Detail: v.desc,
			})
		}

		restoreState := "[OFF] Fresh session on boot"
		if s.restoreSession {
			restoreState = "[ON] Resume running applications"
		}
		items = append(items, sdk.Item{
			ID:     "toggle_restore_session",
			Label:  "Restore Previous Session",
			Detail: restoreState,
		})

	case CategoryStorage:
		items = append(items, sdk.Item{
			ID:     "store_precedence",
			Label:  "Preference Precedence Hierarchy",
			Detail: "1. Preview (in-memory) -> 2. App override -> 3. User settings -> 4. System config -> 5. Defaults",
		})
		items = append(items, sdk.Item{
			ID:     "store_user_path",
			Label:  "User Preferences File",
			Detail: "Stored at /users/guest/config/settings.json (writable)",
		})
		items = append(items, sdk.Item{
			ID:     "store_system_path",
			Label:  "System Configuration File",
			Detail: "Stored at config/system.json (host file, admin only)",
		})
	}
	return items
}

func (s *Settings) buildFields() []sdk.Field {
	var fields []sdk.Field
	if s.category == CategoryShortcuts {
		fields = append(fields, sdk.Field{
			ID:    "key_chord",
			Label: "New key chord for selected action (e.g. f8, ctrl+g)",
			Value: "",
		})
	}
	return fields
}

func (s *Settings) buildActions() []sdk.Action {
	return []sdk.Action{
		{ID: "select", Label: "Select / Toggle"},
		{ID: "preview_theme", Label: "Preview Theme"},
		{ID: "save", Label: "Save Changes"},
		{ID: "cancel", Label: "Cancel / Revert"},
		{ID: "reset_defaults", Label: "Reset Defaults"},
		{ID: "cat_appearance", Label: "Appearance"},
		{ID: "cat_accessibility", Label: "Accessibility"},
		{ID: "cat_shortcuts", Label: "Shortcuts"},
		{ID: "cat_startup", Label: "Startup"},
		{ID: "cat_storage", Label: "Storage"},
	}
}

func (s *Settings) handleAction(ctx context.Context, req sdk.ActionRequest) (sdk.View, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.errorMessage = ""
	s.statusMessage = ""

	switch req.Action {
	case "cat_appearance":
		s.category = CategoryAppearance
		s.selectedItem = "theme_nostalgia"
	case "cat_accessibility":
		s.category = CategoryAccessibility
		s.selectedItem = "toggle_motion"
	case "cat_shortcuts":
		s.category = CategoryShortcuts
		s.selectedItem = "sc_home"
	case "cat_startup":
		s.category = CategoryStartup
		s.selectedItem = "start_home"
	case "cat_storage":
		s.category = CategoryStorage
		s.selectedItem = "store_precedence"

	case "select":
		s.handleSelectLocked(ctx, req)

	case "preview_theme":
		s.handlePreviewThemeLocked(ctx, req)

	case "save":
		s.handleSaveLocked(ctx)

	case "cancel":
		s.handleCancelLocked(ctx)

	case "reset_defaults":
		s.handleResetLocked(ctx)
	}

	s.refreshThemesLocked(ctx)

	state := sdk.ViewReady
	if s.errorMessage != "" {
		state = sdk.ViewError
	}

	return sdk.View{
		Title:   fmt.Sprintf("Settings - %s", s.categoryTitle()),
		State:   state,
		Status:  s.buildStatus(),
		Error:   s.errorMessage,
		Items:   s.buildItems(),
		Fields:  s.buildFields(),
		Actions: s.buildActions(),
	}, nil
}

func (s *Settings) handleSelectLocked(ctx context.Context, req sdk.ActionRequest) {
	itemID := req.ItemID
	if itemID == "" {
		itemID = s.selectedItem
	}
	s.selectedItem = itemID

	switch {
	case strings.HasPrefix(itemID, "theme_"):
		themeName := strings.TrimPrefix(itemID, "theme_")
		s.previewTheme = themeName
		_ = s.app.Call(ctx, "config/preview", map[string]any{"path": "theme", "value": themeName}, nil)
		s.statusMessage = fmt.Sprintf("Previewing theme %q. Press 'Save Changes' to keep or 'Cancel' to revert.", themeName)

	case itemID == "toggle_motion":
		s.reducedMotion = !s.reducedMotion
		_ = s.app.Call(ctx, "config/set", map[string]any{"path": "accessibility.reduced_motion", "value": s.reducedMotion}, nil)
		s.statusMessage = fmt.Sprintf("Reduced motion set to %v.", s.reducedMotion)

	case strings.HasPrefix(itemID, "mode_"):
		modeName := strings.TrimPrefix(itemID, "mode_")
		s.colorMode = modeName
		_ = s.app.Call(ctx, "config/set", map[string]any{"path": "accessibility.color_mode", "value": modeName}, nil)
		s.statusMessage = fmt.Sprintf("Color mode set to %s.", modeName)

	case strings.HasPrefix(itemID, "start_"):
		viewName := strings.TrimPrefix(itemID, "start_")
		s.startupView = viewName
		_ = s.app.Call(ctx, "config/set", map[string]any{"path": "startup.view", "value": viewName}, nil)
		s.statusMessage = fmt.Sprintf("Startup view set to %s.", viewName)

	case itemID == "toggle_restore_session":
		s.restoreSession = !s.restoreSession
		_ = s.app.Call(ctx, "config/set", map[string]any{"path": "startup.restore_session", "value": s.restoreSession}, nil)
		s.statusMessage = fmt.Sprintf("Restore session set to %v.", s.restoreSession)

	case strings.HasPrefix(itemID, "sc_"):
		actionName := strings.TrimPrefix(itemID, "sc_")
		newKey := strings.TrimSpace(req.Values["key_chord"])
		if newKey != "" {
			var resp struct {
				OK bool `json:"ok"`
			}
			err := s.app.Call(ctx, "config/set", map[string]any{
				"path":  "shortcuts." + actionName,
				"value": strings.ToLower(newKey),
			}, &resp)
			if err != nil {
				s.errorMessage = fmt.Sprintf("Failed to update shortcut: %v", err)
			} else {
				s.shortcuts[actionName] = strings.ToLower(newKey)
				s.statusMessage = fmt.Sprintf("Shortcut for %s updated to %s.", actionName, newKey)
			}
		} else {
			s.statusMessage = fmt.Sprintf("Enter new key chord in field below to rebind %s.", actionName)
		}
	}
}

func (s *Settings) handlePreviewThemeLocked(ctx context.Context, req sdk.ActionRequest) {
	itemID := req.ItemID
	if itemID == "" {
		itemID = s.selectedItem
	}
	if strings.HasPrefix(itemID, "theme_") {
		themeName := strings.TrimPrefix(itemID, "theme_")
		s.previewTheme = themeName
		_ = s.app.Call(ctx, "config/preview", map[string]any{"path": "theme", "value": themeName}, nil)
		s.statusMessage = fmt.Sprintf("Previewing theme %q. Press 'Save Changes' to keep or 'Cancel' to revert.", themeName)
	}
}

func (s *Settings) handleSaveLocked(ctx context.Context) {
	if s.previewTheme != "" {
		_ = s.app.Call(ctx, "config/commit_preview", map[string]any{"layer": "user"}, nil)
		s.theme = s.previewTheme
		s.previewTheme = ""
	}
	s.statusMessage = "All preferences saved to user configuration."
}

func (s *Settings) handleCancelLocked(ctx context.Context) {
	if s.previewTheme != "" {
		_ = s.app.Call(ctx, "config/cancel_preview", nil, nil)
		s.previewTheme = ""
	}
	s.loadFromConfig(ctx)
	s.statusMessage = "Unsaved changes discarded and preview cancelled."
}

func (s *Settings) handleResetLocked(ctx context.Context) {
	if s.previewTheme != "" {
		_ = s.app.Call(ctx, "config/cancel_preview", nil, nil)
		s.previewTheme = ""
	}
	_ = s.app.Call(ctx, "config/reset", map[string]any{"layer": "user"}, nil)
	s.loadFromConfig(ctx)
	s.statusMessage = "User preferences reset to factory defaults."
}

// Programmatic IPC handlers
func (s *Settings) handlePreferences(ctx context.Context, raw json.RawMessage) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{
		"theme":          s.theme,
		"preview_theme":  s.previewTheme,
		"color_mode":     s.colorMode,
		"reduced_motion": s.reducedMotion,
		"high_contrast":  s.highContrast,
		"startup_view":   s.startupView,
		"shortcuts":      s.shortcuts,
	}, nil
}

func (s *Settings) handlePreview(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		Theme string `json:"theme"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if p.Theme == "" {
		return nil, fmt.Errorf("theme is required")
	}

	s.mu.Lock()
	s.previewTheme = p.Theme
	s.mu.Unlock()

	return map[string]any{"ok": true, "previewing": p.Theme}, nil
}

func (s *Settings) handleSave(ctx context.Context, raw json.RawMessage) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handleSaveLocked(ctx)
	return map[string]any{"ok": true}, nil
}

func (s *Settings) handleCancel(ctx context.Context, raw json.RawMessage) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handleCancelLocked(ctx)
	return map[string]any{"ok": true}, nil
}

func (s *Settings) handleReset(ctx context.Context, raw json.RawMessage) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handleResetLocked(ctx)
	return map[string]any{"ok": true}, nil
}
