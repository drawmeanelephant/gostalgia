package engine

// engine.go is the public API boundary between the game core and the
// Delve app shell. The implementation task owns the bodies here and may
// add any number of sibling files (gen.go, fov.go, ai.go, combat.go,
// items.go, save.go, render.go, ...). Rules:
//
//   - types.go is frozen contract — do not edit it.
//   - content*.go is owned by the content task — read its lookup
//     helpers (lookupMonster, lookupItem, spawnMonster, spawnItem,
//     ValidateContent) but do not edit those files.
//   - The package imports only the standard library. No sdk, no
//     gostalgia internals, no IO, no wall clock — Marshal/Unmarshal are
//     the only persistence path and they operate on bytes.
//   - Everything flows through Config.Seed (math/rand/v2 PCG) so a
//     seeded run plus a command stream is fully deterministic.
//   - Invalid commands never error: they produce an EvMessage in the
//     returned slice, matching the house style (in-fiction failures).

import (
	"fmt"
	"math/rand/v2"
)

// Game is one run. Exported fields serialize directly through Marshal;
// unexported fields are runtime caches (rng, FOV scratch) rebuilt or
// carried through the save DTO. Depth is 1-based: Levels[Depth-1] is
// the active floor; nil entries are floors not yet visited.
type Game struct {
	Player Player   `json:"player"`
	Depth  int      `json:"depth"`
	Levels []*Level `json:"levels"`
	Log    []string `json:"log"`
	Out    Outcome  `json:"outcome"`
	// DeathCause names what ended the run ("a goblin", "starvation")
	// for the tombstone and score ledger.
	DeathCause string `json:"death_cause,omitempty"`

	cfg      Config
	rng      *rand.Rand
	pcg      *rand.PCG
	nextID   int
	visible  []bool // FOV cache for the active level, W*H
	lastRest bool   // last command was CmdRest
}

// LogCap bounds the in-game message history. LogTail exposes the tail;
// older lines drop off.
const LogCap = 256

// startHP is a fresh delver's hit points.
const startHP = 20

// New starts a run. cfg.Seed fixes all randomness; cfg.Name rides the
// score ledger. The player spawns on a fresh level 1 near the up
// stairs. The Brass Idol and the boss are placed when level MaxDepth is
// first generated (on descend from MaxDepth-1).
func New(cfg Config) (*Game, error) {
	pcg := rand.NewPCG(cfg.Seed, cfg.Seed^0x9e3779b97f4a7c15)
	g := &Game{
		Depth:  1,
		Levels: make([]*Level, MaxDepth),
		cfg:    cfg,
		pcg:    pcg,
		rng:    rand.New(pcg),
		nextID: 1,
	}
	g.Player = Player{
		Name:    cfg.Name,
		HP:      startHP,
		MaxHP:   startHP,
		Level:   1,
		Satiety: SatietyMax,
	}
	l := g.level(1)
	p := l.findTile(TileStairsUp)
	if p.X < 0 {
		p = l.findTile(TileFloor)
	}
	g.Player.Pos = p
	g.computeFOV()
	g.log("You slip through the cellar of the Brass Elephant into the Underhall.")
	return g, nil
}

// Command resolves one player action and every monster reaction it
// provokes, appends event texts to Log (bounded to LogCap), and returns
// this turn's events for immediate UI feedback. A finished game ignores
// further commands except returning a gentle EvMessage.
func (g *Game) Command(c Cmd) []Event {
	if g.Out != OutcomeOngoing {
		return g.emit(msg("The run is over."))
	}
	g.lastRest = false
	var evs []Event
	var took bool
	switch c.Kind {
	case CmdMove:
		evs, took = g.cmdMove(c.Dir)
	case CmdWait:
		took = true
	case CmdRest:
		return g.cmdRest()
	case CmdPickup:
		evs, took = g.cmdPickup()
	case CmdDescend:
		evs, took = g.cmdDescend()
	case CmdAscend:
		evs, took = g.cmdAscend()
	case CmdUseItem:
		evs, took = g.cmdUseItem(c.ItemID)
	case CmdEquip:
		evs, took = g.cmdEquip(c.ItemID)
	case CmdDrop:
		evs, took = g.cmdDrop(c.ItemID)
	default:
		evs = msg("Nothing happens.")
	}
	if took && g.Out == OutcomeOngoing {
		evs = append(evs, g.takeTurn()...)
	}
	return g.emit(evs)
}

// MapView renders the active level as exactly MapHeight lines of
// MapWidth runes using the glyph vocabulary in types.go: live FOV shows
// everything; remembered-but-unseen tiles show terrain only (no
// monsters/items); unexplored cells are spaces.
func (g *Game) MapView() []string {
	l := g.level(g.Depth)
	rows := make([]string, l.Height)
	for y := 0; y < l.Height; y++ {
		row := make([]rune, l.Width)
		for x := 0; x < l.Width; x++ {
			i := y*l.Width + x
			switch {
			case i < len(g.visible) && g.visible[i]:
				row[x] = g.liveGlyph(l, Pos{x, y})
			case l.Seen[i]:
				row[x] = terrainGlyph(l.Tiles[i])
			default:
				row[x] = ' '
			}
		}
		rows[y] = string(row)
	}
	return rows
}

// liveGlyph picks what a visible cell draws: the player, then a
// monster, then an item, then terrain.
func (g *Game) liveGlyph(l *Level, p Pos) rune {
	if g.Player.Pos.Eq(p) {
		return GlyphPlayer
	}
	if m := l.monsterAt(p); m != nil {
		if def := lookupMonster(m.Def); def != nil {
			return def.Glyph
		}
		return '?'
	}
	if it := l.itemAt(p); it != nil {
		if def := lookupItem(it.Def); def != nil {
			return def.Glyph
		}
		return GlyphScroll
	}
	return terrainGlyph(l.At(p))
}

// terrainGlyph maps a tile kind to its drawn rune.
func terrainGlyph(k TileKind) rune {
	switch k {
	case TileFloor:
		return GlyphFloor
	case TileStairsUp:
		return GlyphStairsUp
	case TileStairsDown:
		return GlyphStairsDown
	}
	return GlyphWall
}

// LogTail returns the last n lines of the message log (all of them when
// n <= 0 or n > len).
func (g *Game) LogTail(n int) []string {
	if n <= 0 || n > len(g.Log) {
		n = len(g.Log)
	}
	return append([]string(nil), g.Log[len(g.Log)-n:]...)
}

// Stats returns the meter snapshot for the UI.
func (g *Game) Stats() Stats {
	p := &g.Player
	s := Stats{
		HP:      p.HP,
		MaxHP:   p.MaxHP,
		Level:   p.Level,
		XP:      p.XP,
		XPNext:  xpForNext(p.Level) - p.XP,
		Satiety: p.Satiety,
		Gold:    p.Gold,
		Depth:   g.Depth,
		Attack:  g.attackStat(),
		Defense: g.defenseStat(),
		Turns:   p.Turns,
		Kills:   p.Kills,
	}
	if s.XPNext < 0 {
		s.XPNext = 0
	}
	if it := g.invItem(p.WeaponID); it != nil {
		if def := lookupItem(it.Def); def != nil {
			s.Weapon = def.Name
		}
	}
	if it := g.invItem(p.ArmorID); it != nil {
		if def := lookupItem(it.Def); def != nil {
			s.Armor = def.Name
		}
	}
	return s
}

// Flags reports the contextual action affordances.
func (g *Game) Flags() Flags {
	l := g.level(g.Depth)
	t := l.At(g.Player.Pos)
	return Flags{
		CanDescend:  t == TileStairsDown && g.Depth < MaxDepth,
		CanAscend:   t == TileStairsUp,
		CanPickup:   l.itemAt(g.Player.Pos) != nil,
		HasArtifact: g.hasArtifact(),
		Resting:     g.lastRest && !g.anyMonsterVisible(),
		Starving:    g.Player.Satiety <= 0,
	}
}

// Inventory lists the pack in stable order for item selection.
func (g *Game) Inventory() []InvEntry {
	out := make([]InvEntry, 0, len(g.Player.Inventory))
	for i, it := range g.Player.Inventory {
		e := InvEntry{ID: it.ID, Kind: defKindOf(it.Def)}
		name := it.Def
		if def := lookupItem(it.Def); def != nil {
			name = def.Name
		}
		if it.Bonus != 0 {
			name = fmt.Sprintf("%+d %s", it.Bonus, name)
		}
		e.Equipped = it.ID == g.Player.WeaponID || it.ID == g.Player.ArmorID
		if e.Equipped {
			name += " (equipped)"
		}
		e.Label = fmt.Sprintf("%c) %s", 'a'+i, name)
		if def := lookupItem(it.Def); def != nil {
			e.Detail = itemDetail(it, def)
		}
		out = append(out, e)
	}
	return out
}

// defKindOf resolves an item def id to its kind.
func defKindOf(id string) ItemKind {
	if def := lookupItem(id); def != nil {
		return def.Kind
	}
	return ItemWeapon
}

// itemDetail is the secondary description for an inventory line.
func itemDetail(it Item, def *ItemDef) string {
	switch def.Kind {
	case ItemWeapon:
		return fmt.Sprintf("weapon, power %d", def.Power+it.Bonus)
	case ItemArmor:
		return fmt.Sprintf("armor, soak %d", def.Power+it.Bonus)
	case ItemFood:
		return fmt.Sprintf("food, %d satiety", def.Power)
	case ItemPotion:
		return fmt.Sprintf("potion, power %d", def.Power)
	case ItemScroll:
		return fmt.Sprintf("scroll, power %d", def.Power)
	case ItemArtifact:
		return "the prize itself"
	}
	return ""
}

// Outcome reports whether the run is ongoing, won, or dead.
func (g *Game) Outcome() Outcome { return g.Out }

// FinalScore computes the ledger entry for a finished run. Calling it
// on an ongoing run returns the current standing.
func (g *Game) FinalScore() Score {
	v := g.Player.Gold + g.Player.XP + g.Depth*50
	out := "ongoing"
	switch g.Out {
	case OutcomeWon:
		v += 1000
		out = "won"
	case OutcomeDead:
		if g.DeathCause == "starvation" {
			out = "starved"
		} else {
			out = "slain by " + g.DeathCause
		}
	}
	return Score{
		Name:    g.Player.Name,
		Value:   v,
		Depth:   g.Depth,
		Outcome: out,
		Turns:   g.Player.Turns,
	}
}

// Marshal snapshots the complete run — including RNG state — so a
// resumed game continues deterministically. The format is versioned
// JSON; Unmarshal rejects other versions and malformed bytes.
func (g *Game) Marshal() ([]byte, error) {
	st, err := g.pcg.MarshalBinary()
	if err != nil {
		return nil, err
	}
	return marshalDTO(saveDTO{
		Version:    saveVersion,
		Config:     g.cfg,
		Player:     g.Player,
		Depth:      g.Depth,
		Levels:     g.Levels,
		Log:        g.Log,
		Out:        g.Out,
		DeathCause: g.DeathCause,
		NextID:     g.nextID,
		Rested:     g.lastRest,
		PCG:        st,
	})
}

// Unmarshal restores a run produced by Marshal. It returns an error for
// corrupt data or a version mismatch; the app layer turns that into
// "previous save unreadable — starting fresh" healing.
func Unmarshal(data []byte) (*Game, error) { return unmarshalDTO(data) }
