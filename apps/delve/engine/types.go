// Package engine is Delve's pure game core: procedural levels,
// field-of-view, turn resolution, monster AI, items, hunger, experience,
// permadeath, and deterministic seeded RNG.
//
// Delve — Beneath the Brass Elephant. The cellar of the Brass Elephant
// tavern opens into the Underhall: MaxDepth levels of generated dungeon.
// The run is won by taking the Brass Idol artifact from the Vault on the
// deepest level and carrying it back up the stairs on level 1. Death is
// permanent; a run ends in OutcomeWon, OutcomeDead, or continues as
// OutcomeOngoing.
//
// HARD CONTRACT: this file is the shared boundary between the engine,
// the content tables, and the app shell. It is owned by the contract
// skeleton commit. Engine logic lives in other files of this package;
// content tables live in content*.go; neither may edit this file. The
// engine package imports ONLY the standard library — never sdk, never
// gostalgia internals — so it stays a deterministic, IO-free core that
// is trivially testable and safely behind the dependency fences.
package engine

// Glyph vocabulary (the app renders MapView verbatim in a Block; these
// are the agreed characters):
//
//	'@' player      '#' wall       '.' floor
//	'<' stairs up   '>' stairs down
//	'!' potion      '?' scroll     '/' weapon    '[' armor
//	'%' food        '$' gold       '*' artifact
//	monsters use letters (see content tables)
const (
	GlyphPlayer     = '@'
	GlyphWall       = '#'
	GlyphFloor      = '.'
	GlyphStairsUp   = '<'
	GlyphStairsDown = '>'
	GlyphPotion     = '!'
	GlyphScroll     = '?'
	GlyphWeapon     = '/'
	GlyphArmor      = '['
	GlyphFood       = '%'
	GlyphGold       = '$'
	GlyphArtifact   = '*'
)

// MaxDepth is the number of levels in the Underhall. The boss and the
// Brass Idol live on level MaxDepth; the surface stairs sit on level 1.
const MaxDepth = 8

// MapWidth and MapHeight bound every generated level. MapView returns
// exactly MapHeight lines of exactly MapWidth runes so it always fits
// the presentation Block budget (32 lines / 16KiB).
const (
	MapWidth  = 48
	MapHeight = 22
)

// SightRadius is the field-of-view radius in tiles (Chebyshev).
const SightRadius = 8

// Pos is a tile coordinate.
type Pos struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// Add returns p moved by d.
func (p Pos) Add(d Pos) Pos { return Pos{X: p.X + d.X, Y: p.Y + d.Y} }

// Eq reports whether two positions coincide.
func (p Pos) Eq(o Pos) bool { return p.X == o.X && p.Y == o.Y }

// Dir is one of the eight compass directions.
type Dir int

const (
	DirN Dir = iota
	DirNE
	DirE
	DirSE
	DirS
	DirSW
	DirW
	DirNW
)

// AllDirs lists every direction for iteration.
var AllDirs = [8]Dir{DirN, DirNE, DirE, DirSE, DirS, DirSW, DirW, DirNW}

// Delta returns the unit step for d.
func (d Dir) Delta() Pos {
	switch d {
	case DirN:
		return Pos{0, -1}
	case DirNE:
		return Pos{1, -1}
	case DirE:
		return Pos{1, 0}
	case DirSE:
		return Pos{1, 1}
	case DirS:
		return Pos{0, 1}
	case DirSW:
		return Pos{-1, 1}
	case DirW:
		return Pos{-1, 0}
	case DirNW:
		return Pos{-1, -1}
	}
	return Pos{}
}

// TileKind is one map cell.
type TileKind uint8

const (
	TileWall TileKind = iota
	TileFloor
	TileStairsUp
	TileStairsDown
)

// Level is one generated floor. Tiles is row-major W*H. Seen marks tiles
// the player has ever had in view (explored memory; the renderer draws
// remembered-but-unseen tiles while CurrentlyVisible reports live FOV).
type Level struct {
	Width    int        `json:"w"`
	Height   int        `json:"h"`
	Tiles    []TileKind `json:"tiles"`
	Seen     []bool     `json:"seen"`
	Monsters []Monster  `json:"monsters,omitempty"`
	Items    []Item     `json:"items,omitempty"`
}

// InBounds reports whether p is inside the level.
func (l *Level) InBounds(p Pos) bool {
	return p.X >= 0 && p.Y >= 0 && p.X < l.Width && p.Y < l.Height
}

// At returns the tile at p, or TileWall out of bounds.
func (l *Level) At(p Pos) TileKind {
	if !l.InBounds(p) {
		return TileWall
	}
	return l.Tiles[p.Y*l.Width+p.X]
}

// Set writes the tile at p. Out-of-bounds writes are ignored.
func (l *Level) Set(p Pos, k TileKind) {
	if l.InBounds(p) {
		l.Tiles[p.Y*l.Width+p.X] = k
	}
}

// Walkable reports whether a creature can stand on p (floor and stairs).
func (l *Level) Walkable(p Pos) bool {
	k := l.At(p)
	return k == TileFloor || k == TileStairsUp || k == TileStairsDown
}

// AIKind names a monster behavior pattern.
type AIKind uint8

const (
	// AIWander shuffles randomly until the player is seen, then hunts.
	AIWander AIKind = iota
	// AIHunt seeks the player whenever visible (or heard nearby).
	AIHunt
	// AIFlee hunts but retreats when badly hurt.
	AIFlee
	// AIRanged keeps distance and attacks from range.
	AIRanged
	// AIPack wakes nearby allies when it spots the player.
	AIPack
	// AIGuard holds position near its lair until the player is close,
	// then hunts relentlessly. Boss behavior.
	AIGuard
)

// Monster is one live creature on a level.
type Monster struct {
	ID    int    `json:"id"`
	Def   string `json:"def"` // MonsterDef.ID
	Pos   Pos    `json:"pos"`
	HP    int    `json:"hp"`
	Alert bool   `json:"alert,omitempty"`
	// State is small AI scratch space owned by the engine (cooldowns,
	// pack flags, lair anchor). Content tables must not rely on it.
	State int `json:"state,omitempty"`
}

// ItemKind classifies what using an item does.
type ItemKind uint8

const (
	ItemWeapon ItemKind = iota
	ItemArmor
	ItemPotion
	ItemScroll
	ItemFood
	ItemGold
	ItemArtifact
)

// Item is one concrete object. On the ground it carries Pos; carried
// items use Pos{-1,-1}. Bonus is per-instance variance (a +2 mace).
type Item struct {
	ID    int    `json:"id"`
	Def   string `json:"def"` // ItemDef.ID
	Pos   Pos    `json:"pos"`
	Bonus int    `json:"bonus,omitempty"`
}

// Carried reports whether the item is in a pack rather than on a tile.
func (it Item) Carried() bool { return it.Pos.X < 0 }

// Player is the delver.
type Player struct {
	Name      string `json:"name,omitempty"`
	Pos       Pos    `json:"pos"`
	HP        int    `json:"hp"`
	MaxHP     int    `json:"max_hp"`
	Level     int    `json:"level"`
	XP        int    `json:"xp"`
	Satiety   int    `json:"satiety"` // drains each turn; see Satiety* consts
	Gold      int    `json:"gold"`
	Inventory []Item `json:"inventory,omitempty"`
	WeaponID  int    `json:"weapon_id,omitempty"` // equipped item IDs, 0 = none
	ArmorID   int    `json:"armor_id,omitempty"`
	Turns     int    `json:"turns"`
	Kills     int    `json:"kills"`
}

// Satiety thresholds. New starts at SatietyMax; each turn drains one.
// At or below SatietyHungry a warning event fires; at zero the player
// starves (periodic damage) until they eat or die.
const (
	SatietyMax    = 1000
	SatietyHungry = 200
)

// InventoryCap bounds carried items (excluding absorbed gold piles).
const InventoryCap = 24

// Outcome is the run's end state.
type Outcome uint8

const (
	OutcomeOngoing Outcome = iota
	OutcomeWon
	OutcomeDead
)

// CmdKind names a player command.
type CmdKind uint8

const (
	// CmdMove steps Dir tiles; bumping a monster attacks it. Invalid
	// moves (walls) cost no turn.
	CmdMove CmdKind = iota
	// CmdWait passes one turn.
	CmdWait
	// CmdRest waits until interrupted: hunger warning, monster in view,
	// or 20 turns. Costs up to 20 turns.
	CmdRest
	// CmdPickup takes the item under the player. Gold absorbs directly;
	// anything else needs pack room.
	CmdPickup
	// CmdDescend takes stairs down; only valid standing on '>'.
	CmdDescend
	// CmdAscend takes stairs up; valid on '<'. Ascending level 1 with
	// the Brass Idol wins the run.
	CmdAscend
	// CmdUseItem consumes/activates inventory item ItemID (quaff
	// potion, read scroll, eat food).
	CmdUseItem
	// CmdEquip equips weapon or armor ItemID; equipping the already
	// equipped item unequips it.
	CmdEquip
	// CmdDrop places item ItemID on the floor under the player.
	CmdDrop
)

// Cmd is one player input. Dir applies to CmdMove; ItemID to the
// inventory commands.
type Cmd struct {
	Kind   CmdKind `json:"kind"`
	Dir    Dir     `json:"dir,omitempty"`
	ItemID int     `json:"item_id,omitempty"`
}

// EventKind classifies a log event. The app maps kinds to sound blips;
// Text is always present for the message log.
type EventKind uint8

const (
	EvMessage EventKind = iota // neutral information
	EvHit                      // something took damage
	EvMiss                     // an attack missed
	EvDeath                    // a monster (or the player) died
	EvLevelUp                  // player gained a level
	EvPickup                   // item taken
	EvDrop                     // item dropped
	EvUse                      // potion quaffed / scroll read / food eaten
	EvDescend                  // went down a level
	EvAscend                   // went up a level
	EvHunger                   // hunger warning / starvation tick
	EvWin                      // run won (terminal)
	EvLose                     // run lost (terminal)
)

// Event is one thing that happened while resolving a command.
type Event struct {
	Kind EventKind `json:"kind"`
	Text string    `json:"text"`
}

// Config seeds a new run.
type Config struct {
	// Seed fixes all randomness: generation, drops, combat rolls. Two
	// runs with the same seed and command stream are identical, which
	// is what makes engine tests deterministic.
	Seed uint64
	// Name rides on the tombstone/score ledger. Optional.
	Name string
}

// Stats is the meter-and-status snapshot for the UI.
type Stats struct {
	HP, MaxHP       int
	Level, XP       int
	XPNext          int // XP needed for the next level (0 = max level)
	Satiety         int
	Gold, Depth     int
	Attack, Defense int // effective values including equipment
	Weapon, Armor   string
	Turns, Kills    int
}

// Flags reports contextual affordances for the UI's action states.
type Flags struct {
	CanDescend  bool // standing on '>'
	CanAscend   bool // standing on '<'
	CanPickup   bool // an item lies under the player
	HasArtifact bool // Brass Idol is in the pack
	Resting     bool // last command was CmdRest and it is still safe
	Starving    bool
}

// InvEntry is one inventory line for the UI.
type InvEntry struct {
	ID       int
	Label    string // "a) +2 mace (equipped)"
	Detail   string // weapon/armor power, potion flavor, etc.
	Equipped bool
	Kind     ItemKind
}

// MonsterDef is one row of the bestiary (content tables). Engine code
// reads these; it never mutates them.
type MonsterDef struct {
	ID       string `json:"id"`
	Name     string `json:"name"`  // "goblin", used in log lines ("The goblin hits you.")
	Glyph    rune   `json:"glyph"` // letter drawn on the map
	HP       int    `json:"hp"`
	Attack   int    `json:"attack"`  // nominal damage per hit
	Defense  int    `json:"defense"` // damage soaked per hit
	XP       int    `json:"xp"`      // award on kill
	AI       AIKind `json:"ai"`
	MinDepth int    `json:"min_depth"`       // first level it may spawn on (1-based)
	Weight   int    `json:"weight"`          // relative spawn frequency
	Range    int    `json:"range,omitempty"` // AIRanged attack distance (tiles); 0 = melee only
}

// ItemDef is one row of the item tables.
type ItemDef struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`  // "potion of vigor", "+1 short sword"
	Glyph    rune     `json:"glyph"` // one of the item glyphs above
	Kind     ItemKind `json:"kind"`
	Power    int      `json:"power"`     // weapon damage, armor soak, heal amount, food satiety, scroll effect magnitude — meaning by Kind
	MinDepth int      `json:"min_depth"` // first level it may spawn on
	Weight   int      `json:"weight"`
	// UseText is the log line template when consumed, e.g. "You quaff
	// the potion. Warmth spreads through you." Ignored for non-usables.
	UseText string `json:"use_text,omitempty"`
	// TargetKind for scrolls/potions that affect a monster in view:
	// ScrollTarget* values. Empty = self/no target.
	TargetKind string `json:"target_kind,omitempty"`
}

// Scroll and potion target kinds for ItemDef.TargetKind.
const (
	TargetSelf       = ""        // effect applies to the drinker/reader
	TargetNearest    = "nearest" // nearest visible monster
	TargetAllVisible = "all"     // every visible monster
)

// ArtifactID is the ItemDef.ID of the Brass Idol. Carrying it up the
// level-1 stairs wins the run.
const ArtifactID = "brass_idol"

// BossID is the MonsterDef.ID of the Gilded Matriarch.
const BossID = "gilded_matriarch"

// Score is one finished run in the ledger the app persists. The engine
// computes it; the app owns writing the file.
type Score struct {
	Name    string `json:"name"`
	Value   int    `json:"value"`
	Depth   int    `json:"depth"`
	Outcome string `json:"outcome"` // "won" / "slain by <monster>" / "starved"
	Turns   int    `json:"turns"`
}
