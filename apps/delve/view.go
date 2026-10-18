// view.go composes Delve's presentation snapshot. The full layout wants
// the version-3 contract: the map in a fixed-width Block, HP/satiety/XP
// as Meters, a 3x3 movement Grid, and the message log in the scroll
// Region — the help action swaps the region's content for a controls
// list. Older negotiated versions degrade honestly: version 2 keeps
// blocks/meters/grid and renders the log as a second block; version 1
// carries map and log lines as items.
package delve

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"gostalgia/apps/delve/engine"
	"gostalgia/sdk"
)

// v1LogItems bounds the log lines a version-1 snapshot carries as items
// so map + inventory + log stay under the 100-item cap. v2LogLines is the
// contract's block-line budget, applied to the v2 log block.
const (
	v1LogItems = 20
	v2LogLines = 32
)

// helpText is the controls summary the help action toggles into the
// region (or a block/item on older contracts).
const helpText = `Controls

  move pad — step in any of eight directions; · waits a turn
  pickup — take what lies underfoot
  descend / ascend — use the stairs you stand on (> and <)
  rest — wait until interrupted or 20 turns pass
  use / equip / drop — act on the selected pack item
  new — abandon this run and delve again
  help — toggle this panel

The Underhall is eight levels deep. The Brass Idol waits in the
Vault at the bottom; carry it up the level-1 stairs to win. Death
is permanent — the ledger remembers every delver.`

// introItems welcome a player who has not begun a run yet.
var introItems = []sdk.Item{
	{ID: "intro0", Label: "Below the Brass Elephant's cellar the Underhall waits:", Detail: "eight levels of dark, monsters, and loot"},
	{ID: "intro1", Label: "Bring back the Brass Idol from the Vault on the deepest floor", Detail: "carry it up the level-1 stairs to win"},
	{ID: "intro2", Label: "Death is permanent. The ledger by the door remembers.", Detail: "top ten runs are engraved on scores.json"},
}

// viewLocked builds the snapshot at the negotiated presentation version.
// Callers hold mu.
func (d *Delve) viewLocked(version int) sdk.View {
	v := sdk.View{
		Title:  d.titleLocked(),
		State:  sdk.ViewReady,
		Error:  d.lastError,
		Status: d.statusLocked(),
	}
	switch {
	case d.g == nil:
		d.preRunViewLocked(&v, version)
	case d.g.Outcome() != engine.OutcomeOngoing:
		d.endedViewLocked(&v, version)
	default:
		d.runViewLocked(&v, version)
	}
	return v
}

// preRunViewLocked is the tavern-door screen: no run exists yet, so the
// view offers a name field, the begin action, and the ledger.
func (d *Delve) preRunViewLocked(v *sdk.View, version int) {
	v.Items = append(append([]sdk.Item{}, introItems...), d.scoreItemsLocked()...)
	v.Fields = []sdk.Field{{ID: "name", Label: "Delver name (optional)", Value: d.name}}
	v.Actions = []sdk.Action{
		{ID: "new", Label: "Begin the delve"},
		{ID: "help", Label: "Help"},
	}
	if d.showHelp {
		d.helpSurfaceLocked(v, version)
	}
}

// endedViewLocked is the post-run screen: tombstone for the slain,
// victory for the escaped, the ledger alongside, and a fresh-start path.
func (d *Delve) endedViewLocked(v *sdk.View, version int) {
	sc := d.g.FinalScore()
	name := sc.Name
	if name == "" {
		name = d.name
	}
	endLines := tombstoneLines(name, sc)
	if d.g.Outcome() == engine.OutcomeWon {
		endLines = victoryLines(name, sc)
	}
	if version >= 2 {
		v.Blocks = []sdk.Block{{ID: "end", Text: strings.Join(endLines, "\n")}}
	} else {
		for i, line := range endLines {
			v.Items = append(v.Items, textItem(fmt.Sprintf("end%02d", i), line))
		}
	}
	v.Items = append(v.Items, d.scoreItemsLocked()...)
	v.Fields = []sdk.Field{{ID: "name", Label: "Delver name (optional)", Value: d.name}}
	v.Actions = []sdk.Action{
		{ID: "new", Label: "Delve again"},
		{ID: "help", Label: "Help"},
	}
	d.logSurfaceLocked(v, version)
}

// runViewLocked is the living game: map, meters, pad, log, inventory,
// and the actions the current position affords. A pending save shows the
// same frozen world with everything dimmed behind continue-or-new.
func (d *Delve) runViewLocked(v *sdk.View, version int) {
	st := d.g.Stats()
	fl := d.g.Flags()
	inv := d.g.Inventory()

	v.Items = d.inventoryItemsLocked(inv)
	v.Actions = d.runActionsLocked(inv, fl)
	if d.pending {
		v.Fields = []sdk.Field{{ID: "name", Label: "Delver name (optional)", Value: d.name}}
	}
	if version == 1 {
		// v1: the world reads as items — map lines, the log tail, then
		// the pack. IDs stay lettered so the entry pattern holds.
		log := tailLines(d.g.LogTail(logTailLines), v1LogItems)
		items := make([]sdk.Item, 0, engine.MapHeight+len(log)+len(inv))
		for i, line := range d.g.MapView() {
			items = append(items, textItem(fmt.Sprintf("map%02d", i), line))
		}
		for i, line := range log {
			items = append(items, textItem(fmt.Sprintf("log%02d", i), line))
		}
		v.Items = append(items, v.Items...)
		if d.showHelp {
			d.helpSurfaceLocked(v, version)
		}
		return
	}
	v.Blocks = []sdk.Block{{ID: "map", Text: strings.Join(d.g.MapView(), "\n")}}
	v.Meters = metersLocked(st)
	v.Grid = d.gridLocked()
	d.logSurfaceLocked(v, version)
}

// logSurfaceLocked shows help text when toggled, else the log tail — a
// scroll region on version 3, a bounded block on version 2.
func (d *Delve) logSurfaceLocked(v *sdk.View, version int) {
	if d.showHelp {
		d.helpSurfaceLocked(v, version)
		return
	}
	log := d.g.LogTail(logTailLines)
	if version >= 3 {
		v.Region = &sdk.Region{ID: "log", Label: "Log", Text: strings.Join(log, "\n")}
		return
	}
	if tail := tailLines(log, v2LogLines); len(tail) > 0 {
		v.Blocks = append(v.Blocks, sdk.Block{ID: "log", Label: "Log", Text: strings.Join(tail, "\n")})
	}
}

// helpSurfaceLocked puts the controls text in the region, or a block on
// version 2, or items on version 1.
func (d *Delve) helpSurfaceLocked(v *sdk.View, version int) {
	switch {
	case version >= 3:
		v.Region = &sdk.Region{ID: "help", Label: "Help", Text: helpText}
	case version == 2:
		v.Blocks = append(v.Blocks, sdk.Block{ID: "help", Label: "Help", Text: helpText})
	default:
		for i, line := range tailLines(strings.Split(helpText, "\n"), v1LogItems) {
			v.Items = append(v.Items, textItem(fmt.Sprintf("help%02d", i), line))
		}
	}
}

// textItem renders a preformatted line as an item; blank lines would fail
// the contract's non-empty label rule, so they read as a quiet dot.
func textItem(id, line string) sdk.Item {
	if strings.TrimSpace(line) == "" {
		line = "·"
	}
	return sdk.Item{ID: id, Label: line}
}

// inventoryItemsLocked renders the pack as selectable items; the item id
// carries the engine's inventory id back on use/equip/drop.
func (d *Delve) inventoryItemsLocked(inv []engine.InvEntry) []sdk.Item {
	if len(inv) == 0 {
		return []sdk.Item{{ID: "pack_empty", Label: "the pack is empty", Detail: "loot lies below — pick it up"}}
	}
	items := make([]sdk.Item, 0, len(inv))
	for _, e := range inv {
		detail := kindLabel(e.Kind)
		if e.Equipped {
			detail += " · equipped"
		}
		if e.Detail != "" {
			detail += " · " + e.Detail
		}
		items = append(items, sdk.Item{ID: itemSID(e.ID), Label: e.Label, Detail: detail})
	}
	return items
}

// scoreItemsLocked renders the ledger as items for the door and
// end-of-run screens.
func (d *Delve) scoreItemsLocked() []sdk.Item {
	if len(d.scores) == 0 {
		return []sdk.Item{{ID: "ledger_empty", Label: "the ledger is blank", Detail: "no delver has returned — or died trying — yet"}}
	}
	items := make([]sdk.Item, 0, len(d.scores))
	for i, sc := range d.scores {
		name := sc.Name
		if name == "" {
			name = "a nameless delver"
		}
		items = append(items, sdk.Item{
			ID:     fmt.Sprintf("score_%d", i),
			Label:  fmt.Sprintf("%d. %s — %d", i+1, name, sc.Value),
			Detail: fmt.Sprintf("%s · depth %d · %d turns", sc.Outcome, sc.Depth, sc.Turns),
		})
	}
	return items
}

// runActionsLocked lists the action affordances for a live run, dimmed
// by the engine's flags. While a save waits pending only the choice and
// help stay live; the pad cells dim to match.
func (d *Delve) runActionsLocked(inv []engine.InvEntry, fl engine.Flags) []sdk.Action {
	if d.pending {
		return []sdk.Action{
			{ID: "continue", Label: "Continue"},
			{ID: "new", Label: "New delve"},
			{ID: "help", Label: "Help"},
		}
	}
	var usable, equippable bool
	for _, e := range inv {
		switch e.Kind {
		case engine.ItemPotion, engine.ItemScroll, engine.ItemFood:
			usable = true
		case engine.ItemWeapon, engine.ItemArmor:
			equippable = true
		}
	}
	return []sdk.Action{
		{ID: "pickup", Label: "Pick up", Disabled: !fl.CanPickup},
		{ID: "descend", Label: "Descend", Disabled: !fl.CanDescend},
		{ID: "ascend", Label: "Ascend", Disabled: !fl.CanAscend},
		{ID: "rest", Label: "Rest"},
		{ID: "use", Label: "Use", Disabled: !usable},
		{ID: "equip", Label: "Equip", Disabled: !equippable},
		{ID: "drop", Label: "Drop", Disabled: len(inv) == 0},
		{ID: "new", Label: "New delve"},
		{ID: "help", Label: "Help"},
	}
}

// gridLocked builds the 3x3 movement pad. The whole pad dims while a
// saved run waits pending — the dungeon is frozen until the choice.
func (d *Delve) gridLocked() *sdk.Grid {
	off := d.pending
	return &sdk.Grid{
		Label:   "Move",
		Columns: 3,
		Cells: []sdk.Cell{
			{ID: "nw", Label: "↖", Action: moveAction, Disabled: off},
			{ID: "n", Label: "↑", Action: moveAction, Disabled: off},
			{ID: "ne", Label: "↗", Action: moveAction, Disabled: off},
			{ID: "w", Label: "←", Action: moveAction, Disabled: off},
			{ID: "wait", Label: "·", Action: moveAction, Disabled: off},
			{ID: "e", Label: "→", Action: moveAction, Disabled: off},
			{ID: "sw", Label: "↙", Action: moveAction, Disabled: off},
			{ID: "s", Label: "↓", Action: moveAction, Disabled: off},
			{ID: "se", Label: "↘", Action: moveAction, Disabled: off},
		},
	}
}

// metersLocked renders the vitals. Every fraction guards its divisor —
// the contract forbids NaN and infinities.
func metersLocked(st engine.Stats) []sdk.Meter {
	xp := 1.0
	xpLabel := fmt.Sprintf("Lv %d · max", st.Level)
	if st.XPNext > 0 {
		xp = clamp01(float64(st.XP) / float64(st.XPNext))
		xpLabel = fmt.Sprintf("Lv %d · XP %d/%d", st.Level, st.XP, st.XPNext)
	}
	return []sdk.Meter{
		{ID: "hp", Label: fmt.Sprintf("HP %d/%d", st.HP, st.MaxHP), Value: frac(st.HP, st.MaxHP)},
		{ID: "satiety", Label: fmt.Sprintf("Satiety %d", st.Satiety), Value: frac(st.Satiety, engine.SatietyMax)},
		{ID: "xp", Label: xpLabel, Value: xp},
	}
}

// frac is a guarded numerator/denominator for meters.
func frac(n, d int) float64 {
	if d <= 0 {
		return 0
	}
	return clamp01(float64(n) / float64(d))
}

func clamp01(f float64) float64 {
	switch {
	case f < 0:
		return 0
	case f > 1:
		return 1
	}
	return f
}

// kindLabel names an item kind for inventory item details.
func kindLabel(k engine.ItemKind) string {
	switch k {
	case engine.ItemWeapon:
		return "weapon"
	case engine.ItemArmor:
		return "armor"
	case engine.ItemPotion:
		return "potion"
	case engine.ItemScroll:
		return "scroll"
	case engine.ItemFood:
		return "food"
	case engine.ItemGold:
		return "gold"
	case engine.ItemArtifact:
		return "artifact"
	}
	return "item"
}

// tailLines keeps the last n lines of s.
func tailLines(s []string, n int) []string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// tombstoneLines engraves the dead-run block: name, cause, standing.
func tombstoneLines(name string, sc engine.Score) []string {
	if name == "" {
		name = "A NAMELESS DELVER"
	}
	cause := sc.Outcome
	if cause == "" || cause == "won" {
		cause = "claimed by the Underhall"
	}
	return []string{
		"      .-~~~~~~~~~~~~~~~~-.",
		"     /                    \\",
		"    |      HERE LIES       |",
		"    |                      |",
		"    |" + center(strings.ToUpper(name), 22) + "|",
		"    |                      |",
		"    |" + center(cause, 22) + "|",
		"    |" + center(fmt.Sprintf("depth %d · %d points", sc.Depth, sc.Value), 22) + "|",
		"    |                      |",
		" ___|______________________|___",
	}
}

// victoryLines draws the won-run block: the Idol carried into daylight.
func victoryLines(name string, sc engine.Score) []string {
	if name == "" {
		name = "a nameless delver"
	}
	return []string{
		"        \\  |  /",
		"         \\ | /",
		"      .--~~~~~~~--.",
		"     | BRASS IDOL |",
		"      '--._____.--'",
		"",
		fmt.Sprintf("%s carried the Idol into the light", name),
		fmt.Sprintf("%d points · depth %d · %d turns", sc.Value, sc.Depth, sc.Turns),
	}
}

// center pads s to width w runes, truncating rather than overflowing
// the stone.
func center(s string, w int) string {
	if utf8.RuneCountInString(s) > w {
		s = string([]rune(s)[:w-1]) + "…"
	}
	pad := w - utf8.RuneCountInString(s)
	if pad < 0 {
		pad = 0
	}
	return strings.Repeat(" ", pad/2) + s + strings.Repeat(" ", pad-pad/2)
}

// titleLocked puts the run's end — or its pause — in the chrome.
func (d *Delve) titleLocked() string {
	if d.g == nil {
		return delveTitle
	}
	switch d.g.Outcome() {
	case engine.OutcomeDead:
		return delveTitle + " — you died"
	case engine.OutcomeWon:
		return delveTitle + " — you escaped!"
	}
	return delveTitle
}

// statusLocked picks the line under the view: transient notes win, then
// pending/end banners, then the honest where-you-are summary.
func (d *Delve) statusLocked() string {
	if d.statusNote != "" {
		return d.statusNote
	}
	if d.g == nil {
		return "the Brass Elephant's cellar stairs wait — name your delver and begin"
	}
	if d.pending {
		return "an earlier delve is in progress — Continue, or begin a New delve"
	}
	st := d.g.Stats()
	switch d.g.Outcome() {
	case engine.OutcomeDead:
		return fmt.Sprintf("the Underhall keeps this one — %d points, depth %d; New delve to try again",
			d.g.FinalScore().Value, st.Depth)
	case engine.OutcomeWon:
		return fmt.Sprintf("the Brass Idol is home — %d points; the ledger remembers",
			d.g.FinalScore().Value)
	}
	fl := d.g.Flags()
	notes := []string{fmt.Sprintf("Underhall — level %d · HP %d/%d · %d gold", st.Depth, st.HP, st.MaxHP, st.Gold)}
	if fl.HasArtifact {
		notes = append(notes, "the Brass Idol is yours — climb for the light")
	}
	if fl.Starving {
		notes = append(notes, "STARVING")
	} else if st.Satiety <= engine.SatietyHungry {
		notes = append(notes, "hungry")
	}
	if fl.Resting {
		notes = append(notes, "resting")
	}
	return strings.Join(notes, " · ")
}
