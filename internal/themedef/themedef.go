// Package themedef defines the JSON schema and loader for user-loadable
// themes dropped into /config/themes/*.json. It is deliberately
// stdlib-only: services outside internal/experience validate and serve
// theme definitions without importing Charm. internal/experience/theme
// converts a validated Definition into renderable tokens.
package themedef

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// Dir is the environment directory scanned for user theme files.
const Dir = "/config/themes"

// Bounds keep user themes cheap to scan and small enough to hand-edit.
const (
	// MaxFiles bounds how many theme files a single scan loads; files
	// beyond the limit are skipped with a reason.
	MaxFiles = 64
	// MaxFileSize bounds one theme file (64 KiB).
	MaxFileSize = 64 << 10
	// maxSkips bounds the skip records a scan reports.
	maxSkips        = 32
	maxNameLen      = 48
	maxSpacing      = 8
	maxGlyphRunes   = 8
	maxLabelRunes   = 32
	maxMessageRunes = 200
	maxDescRunes    = 160
)

var (
	namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	hexPattern  = regexp.MustCompile(`^#[0-9a-fA-F]{3}$|^#[0-9a-fA-F]{4}$|^#[0-9a-fA-F]{6}$|^#[0-9a-fA-F]{8}$`)
	ansiPattern = regexp.MustCompile(`^[0-9]{1,3}$`)
)

// Border presets a definition may name instead of spelling out glyphs.
var borderPresets = map[string]bool{
	"normal": true, "rounded": true, "thick": true,
	"double": true, "block": true, "ascii": true,
}

// GlyphSet is an explicit set of border glyphs.
type GlyphSet struct {
	Top         string `json:"top"`
	Bottom      string `json:"bottom"`
	Left        string `json:"left"`
	Right       string `json:"right"`
	TopLeft     string `json:"top_left"`
	TopRight    string `json:"top_right"`
	BottomLeft  string `json:"bottom_left"`
	BottomRight string `json:"bottom_right"`
}

// Border is either a named preset ("rounded") or an explicit glyph set.
type Border struct {
	Preset string
	Glyphs *GlyphSet
}

// UnmarshalJSON accepts "preset" or a glyph object.
func (b *Border) UnmarshalJSON(data []byte) error {
	var preset string
	if err := json.Unmarshal(data, &preset); err == nil {
		b.Preset = preset
		b.Glyphs = nil
		return nil
	}
	var g GlyphSet
	if err := json.Unmarshal(data, &g); err != nil {
		return fmt.Errorf("border must be a preset name or a glyph object")
	}
	b.Preset = ""
	b.Glyphs = &g
	return nil
}

// MarshalJSON mirrors the input schema: presets as strings, glyph sets as
// objects.
func (b Border) MarshalJSON() ([]byte, error) {
	if b.Glyphs != nil {
		return json.Marshal(b.Glyphs)
	}
	return json.Marshal(b.Preset)
}

// Palette carries the eight named colors.
type Palette struct {
	Canvas     string `json:"canvas"`
	Surface    string `json:"surface"`
	Text       string `json:"text"`
	Muted      string `json:"muted"`
	Accent     string `json:"accent"`
	Border     string `json:"border"`
	Header     string `json:"header"`
	HeaderText string `json:"header_text"`
}

// Spacing values are terminal cells, not bytes or runes.
type Spacing struct {
	PanelX int `json:"panel_x"`
	Gap    int `json:"gap"`
}

// Borders are the panel and dialog frames.
type Borders struct {
	Panel  Border `json:"panel"`
	Dialog Border `json:"dialog"`
}

// Typography controls bold weights.
type Typography struct {
	TitleBold   bool `json:"title_bold"`
	HeadingBold bool `json:"heading_bold"`
	KeyBold     bool `json:"key_bold"`
}

// Focus carries the focus colors and marker glyph.
type Focus struct {
	Text   string `json:"text"`
	Fill   string `json:"fill"`
	Border string `json:"border"`
	Marker string `json:"marker"`
}

// Status carries one named state's colors and text cues.
type Status struct {
	Text    string `json:"text"`
	Fill    string `json:"fill"`
	Symbol  string `json:"symbol"`
	Label   string `json:"label"`
	Message string `json:"message"`
}

// States carries the six named states.
type States struct {
	Normal   Status `json:"normal"`
	Disabled Status `json:"disabled"`
	Busy     Status `json:"busy"`
	Success  Status `json:"success"`
	Error    Status `json:"error"`
	Empty    Status `json:"empty"`
}

// Progress carries the filled and track glyphs.
type Progress struct {
	Filled string `json:"filled"`
	Track  string `json:"track"`
}

// Definition is a validated, fully defaulted user theme. It mirrors the
// token set of internal/experience/theme.Theme; Source records the file a
// definition was loaded from and never crosses the wire.
type Definition struct {
	Name          string     `json:"name"`
	Description   string     `json:"description,omitempty"`
	Palette       Palette    `json:"palette"`
	Spacing       Spacing    `json:"spacing"`
	Borders       Borders    `json:"borders"`
	Typography    Typography `json:"typography"`
	Focus         Focus      `json:"focus"`
	States        States     `json:"states"`
	Progress      Progress   `json:"progress"`
	ReducedMotion bool       `json:"reduced_motion"`

	Source string `json:"-"`
}

// Skip records a file the loader rejected, with a human-readable reason.
type Skip struct {
	File   string `json:"file"`
	Reason string `json:"reason"`
}

// fileTheme mirrors the on-disk schema. Pointer fields detect omitted
// sections so defaults can be supplied the same way the built-in themes
// derive tokens from their palette.
type fileTheme struct {
	Name          string          `json:"name"`
	Description   string          `json:"description"`
	Palette       *filePalette    `json:"palette"`
	Spacing       *fileSpacing    `json:"spacing"`
	Borders       *fileBorders    `json:"borders"`
	Typography    *fileTypography `json:"typography"`
	Focus         *fileFocus      `json:"focus"`
	States        *fileStates     `json:"states"`
	Progress      *fileProgress   `json:"progress"`
	ReducedMotion bool            `json:"reduced_motion"`
}

type filePalette struct {
	Canvas     string `json:"canvas"`
	Surface    string `json:"surface"`
	Text       string `json:"text"`
	Muted      string `json:"muted"`
	Accent     string `json:"accent"`
	Border     string `json:"border"`
	Header     string `json:"header"`
	HeaderText string `json:"header_text"`
}

type fileSpacing struct {
	PanelX *int `json:"panel_x"`
	Gap    *int `json:"gap"`
}

type fileBorders struct {
	Panel  *Border `json:"panel"`
	Dialog *Border `json:"dialog"`
}

type fileTypography struct {
	TitleBold   *bool `json:"title_bold"`
	HeadingBold *bool `json:"heading_bold"`
	KeyBold     *bool `json:"key_bold"`
}

type fileFocus struct {
	Text   string `json:"text"`
	Fill   string `json:"fill"`
	Border string `json:"border"`
	Marker string `json:"marker"`
}

type fileStatus struct {
	Text    string `json:"text"`
	Fill    string `json:"fill"`
	Symbol  string `json:"symbol"`
	Label   string `json:"label"`
	Message string `json:"message"`
}

type fileStates struct {
	Normal   *fileStatus `json:"normal"`
	Disabled *fileStatus `json:"disabled"`
	Busy     *fileStatus `json:"busy"`
	Success  *fileStatus `json:"success"`
	Error    *fileStatus `json:"error"`
	Empty    *fileStatus `json:"empty"`
}

type fileProgress struct {
	Filled string `json:"filled"`
	Track  string `json:"track"`
}

// Parse decodes, defaults, and validates one theme definition document.
// Unknown fields, malformed values, and missing required tokens are
// errors — never panics, never partial output.
func Parse(data []byte) (Definition, error) {
	var def Definition
	if len(data) > MaxFileSize {
		return def, fmt.Errorf("theme file exceeds %d bytes", MaxFileSize)
	}
	if !utf8.Valid(data) {
		return def, fmt.Errorf("theme file is not valid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var ft fileTheme
	if err := dec.Decode(&ft); err != nil {
		return def, fmt.Errorf("invalid theme JSON: %w", err)
	}
	return resolve(ft)
}

func resolve(ft fileTheme) (Definition, error) {
	var def Definition

	if !namePattern.MatchString(ft.Name) || len(ft.Name) > maxNameLen {
		return def, fmt.Errorf("name %q must match [a-z0-9][a-z0-9_-]* and be at most %d characters", ft.Name, maxNameLen)
	}
	def.Name = ft.Name
	if utf8.RuneCountInString(ft.Description) > maxDescRunes {
		return def, fmt.Errorf("description exceeds %d characters", maxDescRunes)
	}
	def.Description = ft.Description

	if ft.Palette == nil {
		return def, fmt.Errorf("palette is required")
	}
	p := Palette(*ft.Palette)
	for name, c := range map[string]string{
		"palette.canvas": p.Canvas, "palette.surface": p.Surface,
		"palette.text": p.Text, "palette.muted": p.Muted,
		"palette.accent": p.Accent, "palette.border": p.Border,
		"palette.header": p.Header, "palette.header_text": p.HeaderText,
	} {
		if err := checkColor(name, c); err != nil {
			return def, err
		}
	}
	def.Palette = p

	def.Spacing = Spacing{PanelX: 1, Gap: 1}
	if ft.Spacing != nil {
		if ft.Spacing.PanelX != nil {
			def.Spacing.PanelX = *ft.Spacing.PanelX
		}
		if ft.Spacing.Gap != nil {
			def.Spacing.Gap = *ft.Spacing.Gap
		}
	}
	if def.Spacing.PanelX < 0 || def.Spacing.PanelX > maxSpacing ||
		def.Spacing.Gap < 0 || def.Spacing.Gap > maxSpacing {
		return def, fmt.Errorf("spacing values must be between 0 and %d cells", maxSpacing)
	}

	def.Borders = Borders{Panel: Border{Preset: "thick"}, Dialog: Border{Preset: "double"}}
	if ft.Borders != nil {
		if ft.Borders.Panel != nil {
			def.Borders.Panel = *ft.Borders.Panel
		}
		if ft.Borders.Dialog != nil {
			def.Borders.Dialog = *ft.Borders.Dialog
		}
	}
	for name, b := range map[string]Border{"borders.panel": def.Borders.Panel, "borders.dialog": def.Borders.Dialog} {
		if err := checkBorder(name, b); err != nil {
			return def, err
		}
	}

	def.Typography = Typography{TitleBold: true, HeadingBold: true, KeyBold: true}
	if ft.Typography != nil {
		if ft.Typography.TitleBold != nil {
			def.Typography.TitleBold = *ft.Typography.TitleBold
		}
		if ft.Typography.HeadingBold != nil {
			def.Typography.HeadingBold = *ft.Typography.HeadingBold
		}
		if ft.Typography.KeyBold != nil {
			def.Typography.KeyBold = *ft.Typography.KeyBold
		}
	}

	def.Focus = Focus{Text: p.Surface, Fill: p.Text, Border: p.Accent, Marker: "›"}
	if ft.Focus != nil {
		if ft.Focus.Text != "" {
			def.Focus.Text = ft.Focus.Text
		}
		if ft.Focus.Fill != "" {
			def.Focus.Fill = ft.Focus.Fill
		}
		if ft.Focus.Border != "" {
			def.Focus.Border = ft.Focus.Border
		}
		if ft.Focus.Marker != "" {
			def.Focus.Marker = ft.Focus.Marker
		}
	}
	for name, c := range map[string]string{
		"focus.text": def.Focus.Text, "focus.fill": def.Focus.Fill, "focus.border": def.Focus.Border,
	} {
		if err := checkColor(name, c); err != nil {
			return def, err
		}
	}
	if err := checkGlyph("focus.marker", def.Focus.Marker); err != nil {
		return def, err
	}

	def.States = defaultStates(p)
	if ft.States != nil {
		merge := func(dst *Status, src *fileStatus) {
			if src == nil {
				return
			}
			if src.Text != "" {
				dst.Text = src.Text
			}
			if src.Fill != "" {
				dst.Fill = src.Fill
			}
			if src.Symbol != "" {
				dst.Symbol = src.Symbol
			}
			if src.Label != "" {
				dst.Label = src.Label
			}
			if src.Message != "" {
				dst.Message = src.Message
			}
		}
		merge(&def.States.Normal, ft.States.Normal)
		merge(&def.States.Disabled, ft.States.Disabled)
		merge(&def.States.Busy, ft.States.Busy)
		merge(&def.States.Success, ft.States.Success)
		merge(&def.States.Error, ft.States.Error)
		merge(&def.States.Empty, ft.States.Empty)
	}
	for name, s := range map[string]Status{
		"states.normal": def.States.Normal, "states.disabled": def.States.Disabled,
		"states.busy": def.States.Busy, "states.success": def.States.Success,
		"states.error": def.States.Error, "states.empty": def.States.Empty,
	} {
		if err := checkStatus(name, s); err != nil {
			return def, err
		}
	}

	def.Progress = Progress{Filled: "━", Track: "·"}
	if ft.Progress != nil {
		if ft.Progress.Filled != "" {
			def.Progress.Filled = ft.Progress.Filled
		}
		if ft.Progress.Track != "" {
			def.Progress.Track = ft.Progress.Track
		}
	}
	if err := checkGlyph("progress.filled", def.Progress.Filled); err != nil {
		return def, err
	}
	if err := checkGlyph("progress.track", def.Progress.Track); err != nil {
		return def, err
	}

	def.ReducedMotion = ft.ReducedMotion
	return def, nil
}

// defaultStates derives the six named states from the palette exactly the
// way the built-in colored themes do.
func defaultStates(p Palette) States {
	return States{
		Normal:   Status{Text: p.Text, Fill: p.Surface, Symbol: "○", Label: "READY", Message: "Ready when you are."},
		Disabled: Status{Text: p.Muted, Fill: p.Canvas, Symbol: "–", Label: "DISABLED", Message: "This action is unavailable."},
		Busy:     Status{Text: p.Accent, Fill: p.Surface, Symbol: "…", Label: "WORKING", Message: "Working on your request…"},
		Success:  Status{Text: "#426640", Fill: p.Surface, Symbol: "✓", Label: "DONE", Message: "All set."},
		Error:    Status{Text: "#A33632", Fill: p.Surface, Symbol: "!", Label: "ERROR", Message: "Something went wrong. Try again."},
		Empty:    Status{Text: p.Muted, Fill: p.Surface, Symbol: "◇", Label: "EMPTY", Message: "Nothing here yet."},
	}
}

// checkColor accepts the same color forms the built-in themes use: #RGB,
// #RGBA, #RRGGBB, #RRGGBBAA hex, or an ANSI-256 index "0"–"255".
func checkColor(path, c string) error {
	if hexPattern.MatchString(c) {
		return nil
	}
	if ansiPattern.MatchString(c) {
		var n int
		for _, r := range c {
			n = n*10 + int(r-'0')
		}
		if n <= 255 {
			return nil
		}
	}
	return fmt.Errorf("%s: invalid color %q (want #RGB, #RRGGBB, or an ANSI index 0-255)", path, c)
}

func checkBorder(path string, b Border) error {
	if b.Glyphs != nil {
		for name, g := range map[string]string{
			"top": b.Glyphs.Top, "bottom": b.Glyphs.Bottom,
			"left": b.Glyphs.Left, "right": b.Glyphs.Right,
			"top_left": b.Glyphs.TopLeft, "top_right": b.Glyphs.TopRight,
			"bottom_left": b.Glyphs.BottomLeft, "bottom_right": b.Glyphs.BottomRight,
		} {
			if err := checkGlyph(path+"."+name, g); err != nil {
				return err
			}
		}
		return nil
	}
	if !borderPresets[b.Preset] {
		return fmt.Errorf("%s: unknown border preset %q (want normal, rounded, thick, double, block, or ascii)", path, b.Preset)
	}
	return nil
}

func checkGlyph(path, g string) error {
	if g == "" {
		return fmt.Errorf("%s must not be empty", path)
	}
	if n := utf8.RuneCountInString(g); n > maxGlyphRunes {
		return fmt.Errorf("%s %q exceeds %d runes", path, g, maxGlyphRunes)
	}
	return nil
}

func checkStatus(path string, s Status) error {
	if err := checkColor(path+".text", s.Text); err != nil {
		return err
	}
	if err := checkColor(path+".fill", s.Fill); err != nil {
		return err
	}
	if err := checkGlyph(path+".symbol", s.Symbol); err != nil {
		return err
	}
	if s.Label == "" {
		return fmt.Errorf("%s.label must not be empty", path)
	}
	if utf8.RuneCountInString(s.Label) > maxLabelRunes {
		return fmt.Errorf("%s.label exceeds %d runes", path, maxLabelRunes)
	}
	if s.Message == "" {
		return fmt.Errorf("%s.message must not be empty", path)
	}
	if utf8.RuneCountInString(s.Message) > maxMessageRunes {
		return fmt.Errorf("%s.message exceeds %d runes", path, maxMessageRunes)
	}
	return nil
}

// LoadDir scans dir for *.json theme files and returns the valid,
// deduplicated definitions sorted by name plus one Skip record per
// rejected file. A missing directory yields empty results, not an error;
// only failures listing the directory itself are errors. reserved names
// (the built-in themes) cause a file to be skipped with a reason — user
// themes may not shadow built-ins. Count and size are bounded by MaxFiles
// and MaxFileSize.
func LoadDir(fsys fs.FS, dir string, reserved map[string]bool) ([]Definition, []Skip, error) {
	entries, err := fs.ReadDir(fsys, strings.TrimPrefix(dir, "/"))
	if err != nil {
		if isNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("themedef: read %s: %w", dir, err)
	}

	var defs []Definition
	var skips []Skip
	seen := make(map[string]string, len(entries))
	skip := func(file, reason string) {
		if len(skips) < maxSkips {
			skips = append(skips, Skip{File: file, Reason: reason})
		}
	}

	loaded := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		if loaded >= MaxFiles {
			skip(name, fmt.Sprintf("theme file limit of %d reached", MaxFiles))
			continue
		}
		loaded++
		if info, err := e.Info(); err == nil && info.Size() > MaxFileSize {
			skip(name, fmt.Sprintf("file exceeds %d bytes", MaxFileSize))
			continue
		}
		data, err := fs.ReadFile(fsys, strings.TrimPrefix(dir, "/")+"/"+name)
		if err != nil {
			skip(name, fmt.Sprintf("read: %v", err))
			continue
		}
		def, err := Parse(data)
		if err != nil {
			skip(name, err.Error())
			continue
		}
		if reserved[def.Name] {
			skip(name, fmt.Sprintf("theme name %q is reserved by a built-in theme", def.Name))
			continue
		}
		if prev, dup := seen[def.Name]; dup {
			skip(name, fmt.Sprintf("theme name %q already defined by %s", def.Name, prev))
			continue
		}
		seen[def.Name] = name
		def.Source = name
		defs = append(defs, def)
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	return defs, skips, nil
}

func isNotExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist)
}
