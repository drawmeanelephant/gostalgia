package engine

// game.go is the turn loop and the level plumbing: command handlers
// that move the player or change depth, per-turn upkeep (hunger,
// monster phase, FOV refresh), and the small Level lookup helpers the
// rest of the engine leans on.

import (
	"fmt"
	"strings"
)

// xpPerLevel scales the cumulative XP threshold for the next level:
// level n advances at n*xpPerLevel total XP.
const xpPerLevel = 15

// xpForNext is the cumulative XP needed to leave level n.
func xpForNext(level int) int { return level * xpPerLevel }

// msg wraps a line of in-fiction failure or information.
func msg(text string) []Event {
	return []Event{{Kind: EvMessage, Text: text}}
}

// emit logs every event text (bounded by LogCap) and returns the batch.
func (g *Game) emit(evs []Event) []Event {
	for _, e := range evs {
		g.log(e.Text)
	}
	return evs
}

// log appends one line, dropping the oldest once LogCap is reached.
func (g *Game) log(s string) {
	if len(g.Log) >= LogCap {
		copy(g.Log, g.Log[1:])
		g.Log = g.Log[:len(g.Log)-1]
	}
	g.Log = append(g.Log, s)
}

// hasKind reports whether evs carries an event of kind k.
func hasKind(evs []Event, k EventKind) bool {
	for _, e := range evs {
		if e.Kind == k {
			return true
		}
	}
	return false
}

// level returns the floor at depth d (1-based), generating it lazily on
// first visit so unseen depths cost nothing and visited floors persist.
func (g *Game) level(d int) *Level {
	l := g.Levels[d-1]
	if l == nil {
		l = g.genLevel(d)
		g.Levels[d-1] = l
	}
	return l
}

// takeTurn runs the world for one turn after a player action: monsters
// act, satiety drains, hunger bites, and the FOV cache refreshes.
func (g *Game) takeTurn() []Event {
	var evs []Event
	g.Player.Turns++
	evs = append(evs, g.monsterPhase()...)
	if g.Out != OutcomeOngoing {
		return evs
	}
	g.Player.Satiety--
	evs = append(evs, g.hungerEvents()...)
	g.computeFOV()
	return evs
}

// hungerEvents fires the crossing warnings and, at zero satiety, the
// starvation tick that can end the run.
func (g *Game) hungerEvents() []Event {
	switch s := g.Player.Satiety; {
	case s <= 0:
		dmg := 1 + g.rng.IntN(2)
		g.Player.HP -= dmg
		evs := []Event{{Kind: EvHunger, Text: fmt.Sprintf("You are starving! (-%d hp)", dmg)}}
		if g.Player.HP <= 0 {
			evs = append(evs, g.die("starvation")...)
		}
		return evs
	case s == SatietyHungry:
		return []Event{{Kind: EvHunger, Text: "Your stomach growls."}}
	case s == SatietyHungry/2:
		return []Event{{Kind: EvHunger, Text: "You feel faint with hunger."}}
	}
	return nil
}

// die ends the run with a cause for the tombstone.
func (g *Game) die(cause string) []Event {
	g.Out = OutcomeDead
	g.DeathCause = cause
	return []Event{
		{Kind: EvDeath, Text: "You die."},
		{Kind: EvLose, Text: "The Underhall keeps your bones."},
	}
}

// cmdMove steps one tile in d, or attacks whatever stands there. Walls
// and invalid directions cost no turn.
func (g *Game) cmdMove(d Dir) ([]Event, bool) {
	delta := d.Delta()
	if delta == (Pos{}) {
		return msg("You can't go that way."), false
	}
	l := g.level(g.Depth)
	dst := g.Player.Pos.Add(delta)
	if m := l.monsterAt(dst); m != nil {
		return g.playerAttack(l, m), true
	}
	if !l.Walkable(dst) {
		return msg("You bump into a wall."), false
	}
	g.Player.Pos = dst
	var evs []Event
	if it := l.itemAt(dst); it != nil {
		if def := lookupItem(it.Def); def != nil {
			evs = append(evs, Event{Kind: EvMessage, Text: "You see here: " + def.Name + "."})
		}
	}
	return evs, true
}

// cmdRest waits up to 20 turns, stopping early when a monster enters
// view or a hunger warning fires.
func (g *Game) cmdRest() []Event {
	var evs []Event
	g.lastRest = true
	rested := 0
	for i := 0; i < 20; i++ {
		if g.anyMonsterVisible() {
			if rested == 0 {
				evs = append(evs, Event{Kind: EvMessage, Text: "You cannot rest with a monster in sight."})
			}
			break
		}
		turn := g.takeTurn()
		evs = append(evs, turn...)
		rested++
		if g.Out != OutcomeOngoing || hasKind(turn, EvHunger) {
			break
		}
	}
	if rested == 20 {
		evs = append(evs, Event{Kind: EvMessage, Text: "You rest a while."})
	}
	return g.emit(evs)
}

// cmdDescend takes the stairs down to a deeper floor.
func (g *Game) cmdDescend() ([]Event, bool) {
	l := g.level(g.Depth)
	if l.At(g.Player.Pos) != TileStairsDown {
		return msg("There are no stairs here."), false
	}
	if g.Depth >= MaxDepth {
		return msg("The way below is choked with rubble."), false
	}
	g.Depth++
	nl := g.level(g.Depth)
	p := nl.findTile(TileStairsUp)
	if p.X < 0 {
		p = nl.findTile(TileFloor)
	}
	g.Player.Pos = p
	g.computeFOV()
	return []Event{{Kind: EvDescend, Text: fmt.Sprintf("You descend to level %d.", g.Depth)}}, true
}

// cmdAscend takes the stairs up. Leaving level 1 with the Brass Idol
// wins the run; without it the delver refuses to leave empty-handed.
func (g *Game) cmdAscend() ([]Event, bool) {
	l := g.level(g.Depth)
	if l.At(g.Player.Pos) != TileStairsUp {
		return msg("There are no stairs here."), false
	}
	if g.Depth == 1 {
		if !g.hasArtifact() {
			return msg("Not without the Brass Idol."), false
		}
		g.Out = OutcomeWon
		return []Event{{Kind: EvWin, Text: "You climb into the lamplight, the Brass Idol heavy in your pack."}}, false
	}
	g.Depth--
	nl := g.level(g.Depth)
	p := nl.findTile(TileStairsDown)
	if p.X < 0 {
		p = nl.findTile(TileFloor)
	}
	g.Player.Pos = p
	g.computeFOV()
	return []Event{{Kind: EvAscend, Text: fmt.Sprintf("You climb back to level %d.", g.Depth)}}, true
}

// --- Level lookups ---------------------------------------------------

// monsterAt returns the monster standing on p, or nil.
func (l *Level) monsterAt(p Pos) *Monster {
	for i := range l.Monsters {
		if l.Monsters[i].Pos.Eq(p) {
			return &l.Monsters[i]
		}
	}
	return nil
}

// monsterByID finds a live monster by its id.
func (l *Level) monsterByID(id int) *Monster {
	for i := range l.Monsters {
		if l.Monsters[i].ID == id {
			return &l.Monsters[i]
		}
	}
	return nil
}

// itemAt returns the topmost item lying on p, or nil.
func (l *Level) itemAt(p Pos) *Item {
	for i := range l.Items {
		if l.Items[i].Pos.Eq(p) {
			return &l.Items[i]
		}
	}
	return nil
}

// findTile locates the first tile of kind k, or {-1,-1}.
func (l *Level) findTile(k TileKind) Pos {
	for y := 0; y < l.Height; y++ {
		for x := 0; x < l.Width; x++ {
			if l.Tiles[y*l.Width+x] == k {
				return Pos{X: x, Y: y}
			}
		}
	}
	return Pos{-1, -1}
}

// --- small Game helpers -----------------------------------------------

// invIndex finds the inventory slot holding item id, or -1.
func (g *Game) invIndex(id int) int {
	for i := range g.Player.Inventory {
		if g.Player.Inventory[i].ID == id {
			return i
		}
	}
	return -1
}

// invItem returns the carried item with the given id, or nil.
func (g *Game) invItem(id int) *Item {
	if i := g.invIndex(id); i >= 0 {
		return &g.Player.Inventory[i]
	}
	return nil
}

// hasArtifact reports whether the Brass Idol rides in the pack.
func (g *Game) hasArtifact() bool {
	for _, it := range g.Player.Inventory {
		if it.Def == ArtifactID {
			return true
		}
	}
	return false
}

// anyMonsterVisible reports whether the live FOV holds a monster.
func (g *Game) anyMonsterVisible() bool {
	l := g.level(g.Depth)
	for i := range l.Monsters {
		p := l.Monsters[i].Pos
		if l.InBounds(p) && p.Y*l.Width+p.X < len(g.visible) && g.visible[p.Y*l.Width+p.X] {
			return true
		}
	}
	return false
}

// refer renders a monster name with an article for prose: "a goblin"
// for common nouns, "the Gilded Matriarch" for proper ones.
func refer(name string) string {
	if name == "" {
		return "something"
	}
	if c := name[0]; c >= 'A' && c <= 'Z' {
		return "the " + name
	}
	return "a " + name
}

// title uppercases the first letter of s for sentence starts.
func title(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
