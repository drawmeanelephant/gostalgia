package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"gostalgia/sdk"
)

type mockConfigService struct {
	mu            sync.Mutex
	effective     map[string]any
	previewActive bool
	previewTheme  string
	calls         []string
	userThemes    []map[string]any // served by theme/list alongside builtins
}

func newMockConfigService() *mockConfigService {
	return &mockConfigService{
		effective: map[string]any{
			"theme": "nostalgia",
			"accessibility": map[string]any{
				"color_mode":     "ansi256",
				"reduced_motion": false,
				"high_contrast":  false,
			},
			"startup": map[string]any{
				"view":            "home",
				"restore_session": false,
			},
			"shortcuts": map[string]any{
				"home":          "f1",
				"launcher":      "f2",
				"stop":          "f3",
				"view":          "f4",
				"tasks":         "f5",
				"notifications": "f6",
				"settings":      "f7",
				"palette":       "ctrl+p",
			},
		},
	}
}

func (m *mockConfigService) call(ctx context.Context, method string, params, out any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, method)

	rawParams, err := json.Marshal(params)
	if err != nil {
		return err
	}

	switch method {
	case "config/list":
		res := map[string]any{
			"effective": m.effective,
			"layers":    map[string]any{},
		}
		raw, _ := json.Marshal(res)
		if out != nil {
			return json.Unmarshal(raw, out)
		}
		return nil

	case "config/get":
		var p struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(rawParams, &p)
		val := m.effective[p.Path]
		res := map[string]any{
			"path":  p.Path,
			"value": val,
			"layer": "user",
		}
		raw, _ := json.Marshal(res)
		if out != nil {
			return json.Unmarshal(raw, out)
		}
		return nil

	case "config/set":
		var p struct {
			Path  string `json:"path"`
			Value any    `json:"value"`
		}
		_ = json.Unmarshal(rawParams, &p)
		if strings.HasPrefix(p.Path, "shortcuts.") {
			action := strings.TrimPrefix(p.Path, "shortcuts.")
			sc := m.effective["shortcuts"].(map[string]any)
			valStr := p.Value.(string)
			// Mock conflict check
			for a, k := range sc {
				if a != action && k == valStr {
					return errors.New("keybinding conflict: key already assigned")
				}
			}
			sc[action] = valStr
		} else if strings.HasPrefix(p.Path, "accessibility.") {
			prop := strings.TrimPrefix(p.Path, "accessibility.")
			acc := m.effective["accessibility"].(map[string]any)
			acc[prop] = p.Value
		} else if strings.HasPrefix(p.Path, "startup.") {
			prop := strings.TrimPrefix(p.Path, "startup.")
			st := m.effective["startup"].(map[string]any)
			st[prop] = p.Value
		} else {
			m.effective[p.Path] = p.Value
		}
		return nil

	case "config/preview":
		var p struct {
			Path  string `json:"path"`
			Value string `json:"value"`
		}
		_ = json.Unmarshal(rawParams, &p)
		m.previewActive = true
		m.previewTheme = p.Value
		return nil

	case "config/commit_preview":
		if m.previewActive {
			m.effective["theme"] = m.previewTheme
			m.previewActive = false
		}
		return nil

	case "config/cancel_preview":
		m.previewActive = false
		m.previewTheme = ""
		return nil

	case "config/reset":
		m.effective["theme"] = "nostalgia"
		return nil

	case "theme/list":
		themes := []map[string]any{
			{"name": "high-contrast", "source": "builtin"},
			{"name": "high-contrast-light", "source": "builtin"},
			{"name": "midnight", "source": "builtin"},
			{"name": "monochrome", "source": "builtin"},
			{"name": "nostalgia", "source": "builtin"},
		}
		themes = append(themes, m.userThemes...)
		raw, _ := json.Marshal(map[string]any{"themes": themes})
		if out != nil {
			return json.Unmarshal(raw, out)
		}
		return nil

	default:
		return fmt.Errorf("mockConfigService: unhandled method %s", method)
	}
}

type settingsTestHarness struct {
	settings *Settings
	cfg      *mockConfigService
	ctx      *sdk.Context
	handlers map[string]sdk.Handler
	reqSeq   uint64
}

func setupSettingsTest(t *testing.T) *settingsTestHarness {
	t.Helper()
	cfg := newMockConfigService()
	handlers := make(map[string]sdk.Handler)

	inst, err := Factory()
	if err != nil {
		t.Fatal(err)
	}
	s := inst.(*Settings)

	call := func(ctx context.Context, method string, params, out any) error {
		return cfg.call(ctx, method, params, out)
	}
	handle := func(name string, h sdk.Handler) error {
		handlers[name] = h
		return nil
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := sdk.NewContext(Manifest(), logger, call, handle)

	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}

	return &settingsTestHarness{
		settings: s,
		cfg:      cfg,
		ctx:      ctx,
		handlers: handlers,
	}
}

func (h *settingsTestHarness) view(ctx context.Context) (sdk.View, error) {
	vHandler, ok := h.handlers["view"]
	if !ok {
		return sdk.View{}, errors.New("view handler not registered")
	}
	res, err := vHandler(ctx, json.RawMessage(`{"version":1}`))
	if err != nil {
		return sdk.View{}, err
	}
	return res.(sdk.View), nil
}

func (h *settingsTestHarness) act(ctx context.Context, action string, itemID string, values map[string]string) (sdk.View, error) {
	currView, err := h.view(ctx)
	if err != nil {
		return sdk.View{}, err
	}
	aHandler, ok := h.handlers["action"]
	if !ok {
		return sdk.View{}, errors.New("action handler not registered")
	}
	seq := atomic.AddUint64(&h.reqSeq, 1)
	req := sdk.ActionRequest{
		Version:   sdk.PresentationVersion,
		Instance:  currView.Instance,
		RequestID: fmt.Sprintf("req_%d", seq),
		Action:    action,
		ItemID:    itemID,
		Values:    values,
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return sdk.View{}, err
	}
	res, err := aHandler(ctx, raw)
	if err != nil {
		return sdk.View{}, err
	}
	return res.(sdk.View), nil
}

func TestSettingsManifest(t *testing.T) {
	m := Manifest()
	if m.ID != ID {
		t.Fatalf("unexpected ID %q", m.ID)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("manifest validation failed: %v", err)
	}
	hasRead := false
	hasWrite := false
	for _, p := range m.Permissions {
		if p == "config.read" {
			hasRead = true
		}
		if p == "config.write" {
			hasWrite = true
		}
	}
	if !hasRead || !hasWrite {
		t.Fatal("manifest must declare config.read and config.write capabilities")
	}
}

func TestSettingsPresentationInitialView(t *testing.T) {
	h := setupSettingsTest(t)
	view, err := h.view(context.Background())
	if err != nil {
		t.Fatalf("view failed: %v", err)
	}
	if view.State != sdk.ViewReady {
		t.Fatalf("expected ViewReady, got %s", view.State)
	}
	if !strings.Contains(view.Title, "Appearance") {
		t.Fatalf("expected title to mention Appearance, got %q", view.Title)
	}
	if len(view.Items) < 3 {
		t.Fatalf("expected theme items, got %d", len(view.Items))
	}
}

func TestSettingsCategoryNavigation(t *testing.T) {
	h := setupSettingsTest(t)
	ctx := context.Background()

	// Switch to Accessibility
	view, err := h.act(ctx, "cat_accessibility", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(view.Title, "Accessibility") {
		t.Fatalf("expected Accessibility title, got %q", view.Title)
	}

	// Switch to Shortcuts
	view, err = h.act(ctx, "cat_shortcuts", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(view.Title, "Shortcuts") {
		t.Fatalf("expected Shortcuts title, got %q", view.Title)
	}
	if len(view.Fields) == 0 {
		t.Fatal("expected key chord field in shortcuts category")
	}

	// Switch to Startup
	view, err = h.act(ctx, "cat_startup", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(view.Title, "Startup") {
		t.Fatalf("expected Startup title, got %q", view.Title)
	}

	// Switch to Storage
	view, err = h.act(ctx, "cat_storage", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(view.Title, "Storage") {
		t.Fatalf("expected Storage title, got %q", view.Title)
	}
}

func TestSettingsThemePreviewAndCommit(t *testing.T) {
	h := setupSettingsTest(t)
	ctx := context.Background()

	// Preview midnight theme
	view, err := h.act(ctx, "select", "theme_midnight", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !h.cfg.previewActive || h.cfg.previewTheme != "midnight" {
		t.Fatalf("expected mock preview to be active for midnight, got %v / %s", h.cfg.previewActive, h.cfg.previewTheme)
	}
	if !strings.Contains(strings.ToLower(view.Status), "preview") {
		t.Fatalf("expected status to mention previewing, got %q", view.Status)
	}

	// Save changes
	view, err = h.act(ctx, "save", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if h.cfg.previewActive {
		t.Fatal("expected preview to be cleared after save")
	}
	if h.cfg.effective["theme"] != "midnight" {
		t.Fatalf("expected committed theme midnight, got %v", h.cfg.effective["theme"])
	}

	// Preview monochrome then cancel
	_, _ = h.act(ctx, "select", "theme_monochrome", nil)
	if !h.cfg.previewActive {
		t.Fatal("expected preview active")
	}
	view, err = h.act(ctx, "cancel", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if h.cfg.previewActive {
		t.Fatal("expected preview cancelled")
	}
	if h.settings.theme != "midnight" {
		t.Fatalf("expected theme to remain midnight after cancel, got %s", h.settings.theme)
	}
}

// User themes dropped in /config/themes are surfaced by theme/list and
// appear in the picker with the same select-preview-commit flow as the
// built-ins.
func TestSettingsUserThemeInPicker(t *testing.T) {
	h := setupSettingsTest(t)
	ctx := context.Background()
	h.cfg.userThemes = []map[string]any{
		{"name": "ocean", "source": "user", "description": "Deep sea palette"},
	}

	view, err := h.view(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, it := range view.Items {
		if it.ID == "theme_ocean" {
			found = true
			if !strings.Contains(it.Detail, "Deep sea") {
				t.Fatalf("user theme detail = %q", it.Detail)
			}
		}
	}
	if !found {
		t.Fatalf("user theme missing from picker: %+v", view.Items)
	}

	// Preview and commit flow works for user names too.
	view, err = h.act(ctx, "select", "theme_ocean", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !h.cfg.previewActive || h.cfg.previewTheme != "ocean" {
		t.Fatalf("preview = %v / %q, want ocean", h.cfg.previewActive, h.cfg.previewTheme)
	}
	if _, err := h.act(ctx, "save", "", nil); err != nil {
		t.Fatal(err)
	}
	if h.cfg.effective["theme"] != "ocean" {
		t.Fatalf("committed theme = %v, want ocean", h.cfg.effective["theme"])
	}
}

func TestSettingsShortcutConflictHandling(t *testing.T) {
	h := setupSettingsTest(t)
	ctx := context.Background()

	_, _ = h.act(ctx, "cat_shortcuts", "", nil)

	// Attempt to rebind home to "f2" which is already bound to launcher
	view, err := h.act(ctx, "select", "sc_home", map[string]string{"key_chord": "f2"})
	if err != nil {
		t.Fatal(err)
	}
	if view.State != sdk.ViewError || !strings.Contains(view.Error, "conflict") {
		t.Fatalf("expected conflict error, got state=%s error=%q", view.State, view.Error)
	}

	// Valid rebind to "f9"
	view, err = h.act(ctx, "select", "sc_home", map[string]string{"key_chord": "f9"})
	if err != nil {
		t.Fatal(err)
	}
	if view.State != sdk.ViewReady || view.Error != "" {
		t.Fatalf("expected success, got error=%q", view.Error)
	}
	if h.settings.shortcuts["home"] != "f9" {
		t.Fatalf("expected home shortcut f9, got %s", h.settings.shortcuts["home"])
	}
}

func TestSettingsProgrammaticRoutes(t *testing.T) {
	h := setupSettingsTest(t)
	ctx := context.Background()

	prefHandler := h.handlers["preferences"]
	res, err := prefHandler(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	prefMap := res.(map[string]any)
	if prefMap["theme"] != "nostalgia" {
		t.Fatalf("expected nostalgia, got %v", prefMap["theme"])
	}

	previewHandler := h.handlers["preview"]
	_, err = previewHandler(ctx, json.RawMessage(`{"theme":"midnight"}`))
	if err != nil {
		t.Fatal(err)
	}
	if h.settings.previewTheme != "midnight" {
		t.Fatalf("expected preview theme midnight, got %s", h.settings.previewTheme)
	}

	revertHandler := h.handlers["revert"]
	_, err = revertHandler(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if h.settings.previewTheme != "" {
		t.Fatalf("expected preview theme cleared, got %s", h.settings.previewTheme)
	}
}
