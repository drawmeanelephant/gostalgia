package services

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"gostalgia/internal/config"
	"gostalgia/internal/ipc"
	"gostalgia/internal/security"
	"gostalgia/internal/service"
	"gostalgia/internal/themedef"
)

// ThemeService exposes the user-loadable theme registry over IPC. Users
// drop theme definitions into /config/themes/*.json; this service lists
// them alongside the built-in themes and serves validated definitions to
// the shell and Settings. It owns "theme/*".
type ThemeService struct {
	ctx *service.Context

	mu        sync.Mutex
	seenSkips map[string]string // file -> last logged reason; dedupes skip logging across rescans
}

func NewTheme() *ThemeService { return &ThemeService{} }

func (s *ThemeService) Name() string      { return "theme" }
func (s *ThemeService) Depends() []string { return nil }

func (s *ThemeService) Init(ctx *service.Context) error {
	s.ctx = ctx
	s.seenSkips = map[string]string{}
	// Let config/set and config/preview persist the names of valid user
	// themes, not just the built-ins. The validator rescans the themes
	// directory, so a theme dropped moments ago is already persistable.
	config.SetThemeValidator(s.isUserTheme)
	return nil
}

func (s *ThemeService) Start(ctx context.Context) error {
	for method, h := range map[string]ipc.Handler{
		"theme/list": s.list,
		"theme/get":  s.get,
	} {
		if err := s.ctx.Router.Handle(method, h); err != nil {
			return err
		}
	}
	return nil
}

func (s *ThemeService) Stop(ctx context.Context) error {
	config.SetThemeValidator(nil)
	s.ctx.Router.UnhandlePrefix("theme/")
	return nil
}

// reservedThemeNames are the names user files may not claim: the built-in
// themes plus the aliases the shell's `theme` command accepts, which would
// otherwise be shadowed.
var reservedThemeNames = map[string]bool{
	"nostalgia":           true,
	"midnight":            true,
	"monochrome":          true,
	"high-contrast":       true,
	"high-contrast-light": true,
	"default":             true,
	"dark":                true,
	"mono":                true,
	"plain":               true,
	"highcontrast":        true,
	"hc":                  true,
	"highcontrastlight":   true,
	"hcl":                 true,
}

func (s *ThemeService) log() *slog.Logger {
	if s.ctx.Log != nil {
		return s.ctx.Log
	}
	return slog.Default()
}

// loadUserThemes scans /config/themes, logs each newly seen skip reason
// once, and returns the valid definitions plus this scan's skip records.
// A scan failure is logged and reported as an empty registry — it is
// never a service error.
func (s *ThemeService) loadUserThemes() ([]themedef.Definition, []themedef.Skip) {
	if s.ctx.VFS == nil {
		return nil, nil
	}
	defs, skips, err := themedef.LoadDir(s.ctx.VFS, themedef.Dir, reservedThemeNames)
	if err != nil {
		s.log().Warn("user theme scan failed", "dir", themedef.Dir, "err", err)
		return nil, nil
	}
	s.mu.Lock()
	for _, sk := range skips {
		if prev, seen := s.seenSkips[sk.File]; !seen || prev != sk.Reason {
			s.log().Warn("user theme file skipped", "file", sk.File, "reason", sk.Reason)
			s.seenSkips[sk.File] = sk.Reason
		}
	}
	s.mu.Unlock()
	return defs, skips
}

// isUserTheme reports whether name resolves to a valid user theme. It is
// registered with the config layer so theme selections persist like the
// built-in names do.
func (s *ThemeService) isUserTheme(name string) bool {
	defs, _ := s.loadUserThemes()
	for _, d := range defs {
		if d.Name == name {
			return true
		}
	}
	return false
}

type themeInfo struct {
	Name        string `json:"name"`
	Source      string `json:"source"` // "builtin" or "user"
	Description string `json:"description,omitempty"`
	File        string `json:"file,omitempty"`
}

type themeListResp struct {
	Themes  []themeInfo     `json:"themes"`
	Skipped []themedef.Skip `json:"skipped,omitempty"`
}

func (s *ThemeService) list(ctx context.Context, req ipc.Request) (any, error) {
	if err := ipc.RequireCap(ctx, security.CapFileRead); err != nil {
		return nil, err
	}
	defs, skips := s.loadUserThemes()
	names := config.BuiltinThemeNames()
	themes := make([]themeInfo, 0, len(names)+len(defs))
	for _, name := range names {
		themes = append(themes, themeInfo{Name: name, Source: "builtin"})
	}
	for _, d := range defs {
		themes = append(themes, themeInfo{
			Name:        d.Name,
			Source:      "user",
			Description: d.Description,
			File:        d.Source,
		})
	}
	return themeListResp{Themes: themes, Skipped: skips}, nil
}

type themeGetReq struct {
	Name string `json:"name"`
}

type themeGetResp struct {
	Name       string              `json:"name"`
	Source     string              `json:"source"`
	File       string              `json:"file"`
	Definition themedef.Definition `json:"definition"`
}

func (s *ThemeService) get(ctx context.Context, req ipc.Request) (any, error) {
	if err := ipc.RequireCap(ctx, security.CapFileRead); err != nil {
		return nil, err
	}
	var p themeGetReq
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	name := strings.ToLower(strings.TrimSpace(p.Name))
	if name == "" {
		return nil, errors.New("theme/get: name is required")
	}
	defs, _ := s.loadUserThemes()
	for _, d := range defs {
		if d.Name == name {
			return themeGetResp{
				Name:       d.Name,
				Source:     "user",
				File:       d.Source,
				Definition: d,
			}, nil
		}
	}
	if config.ValidThemeName(name) {
		return nil, fmt.Errorf("theme/get: %q is a built-in theme, not a user theme", name)
	}
	return nil, fmt.Errorf("theme/get: unknown theme %q", name)
}
