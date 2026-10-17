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
//
// THE STUBS BELOW ARE CONTRACT PLACEHOLDERS. The engine task replaces
// every body; nothing here is load-bearing yet.

import (
	"errors"
	"math/rand/v2"
)

// errPending marks stubbed entry points until the engine lands. Callers
// must treat it as "not implemented", not a game error.
var errPending = errors.New("delve engine: pending implementation")

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

	cfg     Config
	rng     *rand.Rand
	pcg     *rand.PCG
	nextID  int
	visible []bool // FOV cache for the active level, W*H
}

// LogCap bounds the in-game message history. LogTail exposes the tail;
// older lines drop off.
const LogCap = 256

// New starts a run. cfg.Seed fixes all randomness; cfg.Name rides the
// score ledger. The player spawns on a fresh level 1 near the up
// stairs. The Brass Idol and the boss are placed when level MaxDepth is
// first generated (on descend from MaxDepth-1).
func New(cfg Config) (*Game, error) {
	return nil, errPending
}

// Command resolves one player action and every monster reaction it
// provokes, appends event texts to Log (bounded to LogCap), and returns
// this turn's events for immediate UI feedback. A finished game ignores
// further commands except returning a gentle EvMessage.
func (g *Game) Command(c Cmd) []Event {
	return []Event{{Kind: EvMessage, Text: errPending.Error()}}
}

// MapView renders the active level as exactly MapHeight lines of
// MapWidth runes using the glyph vocabulary in types.go: live FOV shows
// everything; remembered-but-unseen tiles show terrain only (no
// monsters/items); unexplored cells are spaces.
func (g *Game) MapView() []string { return nil }

// LogTail returns the last n lines of the message log (all of them when
// n <= 0 or n > len).
func (g *Game) LogTail(n int) []string { return nil }

// Stats returns the meter snapshot for the UI.
func (g *Game) Stats() Stats { return Stats{} }

// Flags reports the contextual action affordances.
func (g *Game) Flags() Flags { return Flags{} }

// Inventory lists the pack in stable order for item selection.
func (g *Game) Inventory() []InvEntry { return nil }

// Outcome reports whether the run is ongoing, won, or dead.
func (g *Game) Outcome() Outcome { return g.Out }

// FinalScore computes the ledger entry for a finished run. Calling it
// on an ongoing run returns the current standing.
func (g *Game) FinalScore() Score { return Score{} }

// Marshal snapshots the complete run — including RNG state — so a
// resumed game continues deterministically. The format is versioned
// JSON; Unmarshal rejects other versions and malformed bytes.
func (g *Game) Marshal() ([]byte, error) { return nil, errPending }

// Unmarshal restores a run produced by Marshal. It returns an error for
// corrupt data or a version mismatch; the app layer turns that into
// "previous save unreadable — starting fresh" healing.
func Unmarshal(data []byte) (*Game, error) { return nil, errPending }
