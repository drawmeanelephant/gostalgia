package engine

import (
	"bytes"
	"encoding/json"
	"testing"
)

func newTestGame(t *testing.T, seed uint64) *Game {
	t.Helper()
	g, err := New(Config{Seed: seed, Name: "tester"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return g
}

// adjacentFloor returns a walkable tile next to the player plus the
// direction leading to it.
func adjacentFloor(t *testing.T, g *Game) (Dir, Pos) {
	t.Helper()
	l := g.Levels[g.Depth-1]
	for _, d := range AllDirs {
		p := g.Player.Pos.Add(d.Delta())
		if l.Walkable(p) {
			return d, p
		}
	}
	t.Fatal("player has no walkable neighbor")
	return 0, Pos{}
}

// TestDeterminism runs two games on one seed through one command
// stream and demands byte-identical saves.
func TestDeterminism(t *testing.T) {
	cmds := []Cmd{
		{Kind: CmdMove, Dir: DirE},
		{Kind: CmdMove, Dir: DirS},
		{Kind: CmdWait},
		{Kind: CmdPickup},
		{Kind: CmdMove, Dir: DirN},
		{Kind: CmdWait},
		{Kind: CmdRest},
		{Kind: CmdUseItem, ItemID: 4242},
	}
	g1 := newTestGame(t, 77)
	g2 := newTestGame(t, 99)
	g2b := newTestGame(t, 77) // same seed as g1
	for _, c := range cmds {
		g1.Command(c)
		g2.Command(c)
		g2b.Command(c)
	}
	b1, err := g1.Marshal()
	if err != nil {
		t.Fatalf("marshal g1: %v", err)
	}
	b2, err := g2b.Marshal()
	if err != nil {
		t.Fatalf("marshal g2b: %v", err)
	}
	if !bytes.Equal(b1, b2) {
		t.Fatal("same seed + same commands produced different saves")
	}
	b3, err := g2.Marshal()
	if err != nil {
		t.Fatalf("marshal g2: %v", err)
	}
	if bytes.Equal(b1, b3) {
		t.Fatal("different seeds produced identical saves")
	}
}

// TestCombatKills places a weak rat beside the player and beats it to
// death, checking XP and kill credit.
func TestCombatKills(t *testing.T) {
	g := newTestGame(t, 1)
	l := g.Levels[0]
	l.Monsters = nil
	dir, spot := adjacentFloor(t, g)
	l.Monsters = append(l.Monsters, Monster{ID: 900, Def: "giant_rat", Pos: spot, HP: 2, Alert: true})
	for i := 0; i < 12 && len(l.Monsters) > 0; i++ {
		g.Command(Cmd{Kind: CmdMove, Dir: dir})
	}
	if len(l.Monsters) != 0 {
		t.Fatal("the rat endured")
	}
	if g.Player.Kills != 1 {
		t.Fatalf("kills = %d, want 1", g.Player.Kills)
	}
	if g.Player.XP != 2 {
		t.Fatalf("xp = %d, want 2 (rat def)", g.Player.XP)
	}
}

// TestLevelUp forces a big XP award and checks the level-up bonuses.
func TestLevelUp(t *testing.T) {
	g := newTestGame(t, 2)
	evs := g.awardXP(xpForNext(1))
	if g.Player.Level != 2 {
		t.Fatalf("level = %d, want 2", g.Player.Level)
	}
	if g.Player.MaxHP != startHP+4 {
		t.Fatalf("maxhp = %d, want %d", g.Player.MaxHP, startHP+4)
	}
	if !hasKind(evs, EvLevelUp) {
		t.Fatal("no level-up event")
	}
}

// TestHungerStarves runs satiety down: warning, then lethal ticks.
func TestHungerStarves(t *testing.T) {
	g := newTestGame(t, 3)
	l := g.Levels[0]
	l.Monsters = nil

	// The crossing warning at the hungry threshold.
	g.Player.Satiety = SatietyHungry + 1
	evs := g.Command(Cmd{Kind: CmdWait})
	if !hasKind(evs, EvHunger) {
		t.Fatal("no hunger warning at threshold")
	}

	g.Player.Satiety = 2
	dead := false
	for i := 0; i < 80; i++ {
		evs := g.Command(Cmd{Kind: CmdWait})
		if hasKind(evs, EvLose) {
			dead = true
			break
		}
	}
	if !dead {
		t.Fatal("starvation never finished the run")
	}
	if g.Outcome() != OutcomeDead {
		t.Fatalf("outcome = %d, want dead", g.Outcome())
	}
	if g.DeathCause != "starvation" {
		t.Fatalf("death cause %q, want starvation", g.DeathCause)
	}
	if got := g.FinalScore().Outcome; got != "starved" {
		t.Fatalf("score outcome %q, want starved", got)
	}
}

// TestPermadeath freezes the world once the run is over: commands
// after death produce a message and mutate nothing.
func TestPermadeath(t *testing.T) {
	g := newTestGame(t, 8)
	l := g.Levels[0]
	l.Monsters = nil
	g.Player.Satiety = 0
	for i := 0; i < 60 && g.Outcome() == OutcomeOngoing; i++ {
		g.Command(Cmd{Kind: CmdWait})
	}
	if g.Outcome() != OutcomeDead {
		t.Fatal("expected a dead run")
	}
	before, err := g.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	evs := g.Command(Cmd{Kind: CmdMove, Dir: DirN})
	if len(evs) != 1 || evs[0].Kind != EvMessage {
		t.Fatalf("post-death command gave %v", evs)
	}
	after, err := g.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Marshal embeds the log, so compare everything but Log itself.
	var b, a saveDTO
	if err := json.Unmarshal(before, &b); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(after, &a); err != nil {
		t.Fatal(err)
	}
	b.Log, a.Log = nil, nil
	bp, _ := json.Marshal(b.Player)
	ap, _ := json.Marshal(a.Player)
	if !bytes.Equal(bp, ap) || b.Depth != a.Depth || !bytes.Equal(b.PCG, a.PCG) {
		t.Fatal("world mutated after death")
	}
	for i := range b.Levels {
		bl, _ := json.Marshal(b.Levels[i])
		al, _ := json.Marshal(a.Levels[i])
		if !bytes.Equal(bl, al) {
			t.Fatalf("level %d mutated after death", i)
		}
	}
}

// TestArtifactWin carries the Idol up the level-1 stairs.
func TestArtifactWin(t *testing.T) {
	g := newTestGame(t, 5)
	l := g.Levels[0]
	l.Monsters = nil
	g.Player.Inventory = append(g.Player.Inventory,
		Item{ID: 901, Def: ArtifactID, Pos: Pos{-1, -1}})
	g.Player.Pos = l.findTile(TileStairsUp)
	evs := g.Command(Cmd{Kind: CmdAscend})
	if !hasKind(evs, EvWin) {
		t.Fatalf("no win event: %v", evs)
	}
	if g.Outcome() != OutcomeWon {
		t.Fatalf("outcome = %d, want won", g.Outcome())
	}
	if got := g.FinalScore().Outcome; got != "won" {
		t.Fatalf("score outcome %q, want won", got)
	}
}

// TestAscendWithoutIdol refuses to leave empty-handed.
func TestAscendWithoutIdol(t *testing.T) {
	g := newTestGame(t, 5)
	l := g.Levels[0]
	g.Player.Pos = l.findTile(TileStairsUp)
	turns := g.Player.Turns
	evs := g.Command(Cmd{Kind: CmdAscend})
	if g.Outcome() != OutcomeOngoing {
		t.Fatal("run ended without the idol")
	}
	if len(evs) != 1 || evs[0].Kind != EvMessage {
		t.Fatalf("expected a refusal message, got %v", evs)
	}
	if g.Player.Turns != turns {
		t.Fatal("refused ascend consumed a turn")
	}
}

// TestInvalidCommandsAreFree checks that nonsense inputs answer with a
// message and cost no turn.
func TestInvalidCommands(t *testing.T) {
	g := newTestGame(t, 10)
	free := []Cmd{
		{Kind: CmdMove, Dir: Dir(250)},   // not a direction
		{Kind: CmdPickup},                // empty tile (almost surely)
		{Kind: CmdDescend},               // not on stairs
		{Kind: CmdUseItem, ItemID: 4242}, // not carried
		{Kind: CmdEquip, ItemID: 4242},   // not carried
		{Kind: CmdDrop, ItemID: 4242},    // not carried
		{Kind: CmdKind(200)},             // not a command
	}
	for _, c := range free {
		turns := g.Player.Turns
		evs := g.Command(c)
		if len(evs) == 0 || evs[0].Kind != EvMessage {
			t.Fatalf("cmd %+v gave no message: %v", c, evs)
		}
		if g.Player.Turns != turns {
			t.Fatalf("cmd %+v consumed a turn", c)
		}
	}
}

// TestWallBumpIsFree checks walking into stone costs no turn.
func TestWallBumpIsFree(t *testing.T) {
	g := newTestGame(t, 13)
	l := g.Levels[0]
	l.Monsters = nil
	// Find a wall-adjacent floor and stand on it.
	var pos Pos
	var dir Dir
	found := false
	for y := 1; y < l.Height-1 && !found; y++ {
		for x := 1; x < l.Width-1 && !found; x++ {
			p := Pos{x, y}
			if !l.Walkable(p) {
				continue
			}
			for _, d := range AllDirs {
				if !l.Walkable(p.Add(d.Delta())) {
					pos, dir, found = p, d, true
					break
				}
			}
		}
	}
	if !found {
		t.Skip("no wall-adjacent floor")
	}
	g.Player.Pos = pos
	turns := g.Player.Turns
	evs := g.Command(Cmd{Kind: CmdMove, Dir: dir})
	if g.Player.Turns != turns {
		t.Fatal("wall bump consumed a turn")
	}
	if !g.Player.Pos.Eq(pos) {
		t.Fatal("player moved through a wall")
	}
	if len(evs) == 0 || evs[0].Kind != EvMessage {
		t.Fatalf("wall bump gave %v", evs)
	}
}

// TestPotionHeals quaffs a healing potion.
func TestPotionHeals(t *testing.T) {
	g := newTestGame(t, 14)
	l := g.Levels[0]
	l.Monsters = nil
	g.Player.HP = 5
	g.Player.Inventory = append(g.Player.Inventory,
		Item{ID: 800, Def: "potion_heal", Pos: Pos{-1, -1}})
	evs := g.Command(Cmd{Kind: CmdUseItem, ItemID: 800})
	if !hasKind(evs, EvUse) {
		t.Fatalf("no use event: %v", evs)
	}
	if g.Player.HP != 19 {
		t.Fatalf("hp = %d, want 19", g.Player.HP)
	}
	if len(g.Player.Inventory) != 0 {
		t.Fatal("potion not consumed")
	}
}

// TestEquipToggle wields and unwields a dagger.
func TestEquipToggle(t *testing.T) {
	g := newTestGame(t, 15)
	l := g.Levels[0]
	l.Monsters = nil
	g.Player.Inventory = append(g.Player.Inventory,
		Item{ID: 810, Def: "dagger", Pos: Pos{-1, -1}})
	base := g.attackStat()
	g.Command(Cmd{Kind: CmdEquip, ItemID: 810})
	if g.Player.WeaponID != 810 {
		t.Fatal("dagger not equipped")
	}
	if g.attackStat() != base+3 {
		t.Fatalf("attack = %d, want %d", g.attackStat(), base+3)
	}
	g.Command(Cmd{Kind: CmdEquip, ItemID: 810})
	if g.Player.WeaponID != 0 {
		t.Fatal("dagger not unequipped")
	}
	if g.attackStat() != base {
		t.Fatal("attack did not revert")
	}
}

// TestGoldAbsorbs checks that gold skips the pack.
func TestGoldAbsorbs(t *testing.T) {
	g := newTestGame(t, 16)
	l := g.Levels[0]
	l.Monsters = nil
	l.Items = append(l.Items, Item{ID: 820, Def: "gold", Pos: g.Player.Pos, Bonus: 13})
	before := len(l.Items)
	evs := g.Command(Cmd{Kind: CmdPickup})
	if !hasKind(evs, EvPickup) {
		t.Fatalf("no pickup event: %v", evs)
	}
	if g.Player.Gold != 13 {
		t.Fatalf("gold = %d, want 13", g.Player.Gold)
	}
	if len(g.Player.Inventory) != 0 || len(l.Items) != before-1 {
		t.Fatal("gold should not occupy the pack or the floor")
	}
}

// TestDescendBlockedAtMaxDepth refuses to go below the vault floor.
func TestDescendBlockedAtMaxDepth(t *testing.T) {
	g := newTestGame(t, 18)
	l := g.level(MaxDepth)
	g.Depth = MaxDepth
	g.computeFOV()
	g.Player.Pos = l.findTile(TileStairsDown)
	turns := g.Player.Turns
	evs := g.Command(Cmd{Kind: CmdDescend})
	if g.Depth != MaxDepth {
		t.Fatal("descended past MaxDepth")
	}
	if g.Player.Turns != turns {
		t.Fatal("blocked descend consumed a turn")
	}
	if len(evs) == 0 || evs[0].Kind != EvMessage {
		t.Fatalf("blocked descend gave %v", evs)
	}
}

// TestMarshalRoundTrip saves mid-run, restores, and continues both
// copies through identical commands to identical saves.
func TestMarshalRoundTrip(t *testing.T) {
	g := newTestGame(t, 21)
	for _, c := range []Cmd{
		{Kind: CmdMove, Dir: DirE},
		{Kind: CmdMove, Dir: DirS},
		{Kind: CmdWait},
		{Kind: CmdMove, Dir: DirW},
	} {
		g.Command(c)
	}
	data, err := g.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	h, err := Unmarshal(data)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	hp, _ := json.Marshal(h.Player)
	gp, _ := json.Marshal(g.Player)
	if !bytes.Equal(hp, gp) {
		t.Fatal("player state did not round-trip")
	}
	if h.Depth != g.Depth || h.Outcome() != g.Outcome() {
		t.Fatal("run state did not round-trip")
	}
	// The rng stream must resume identically.
	next := Cmd{Kind: CmdWait}
	g.Command(next)
	h.Command(next)
	b1, _ := g.Marshal()
	b2, _ := h.Marshal()
	if !bytes.Equal(b1, b2) {
		t.Fatal("resumed run diverged from the original")
	}
}

// TestUnmarshalRejects checks the bad-input paths.
func TestUnmarshalRejects(t *testing.T) {
	if _, err := Unmarshal([]byte("{not json")); err == nil {
		t.Fatal("accepted garbage")
	}
	if _, err := Unmarshal([]byte(`{"version":99}`)); err == nil {
		t.Fatal("accepted a foreign version")
	}
	if _, err := Unmarshal([]byte(`{"version":1,"depth":1,"levels":[null]}`)); err == nil {
		t.Fatal("accepted a truncated level list")
	}
}

// TestLogCap bounds the message history.
func TestLogCap(t *testing.T) {
	g := newTestGame(t, 22)
	for i := 0; i < LogCap+50; i++ {
		g.log("noise")
	}
	if len(g.Log) != LogCap {
		t.Fatalf("log len %d, want cap %d", len(g.Log), LogCap)
	}
	if tail := g.LogTail(10); len(tail) != 10 || tail[0] != "noise" {
		t.Fatal("LogTail broken")
	}
}

// TestValidateContent blesses the stub tables.
func TestValidateContent(t *testing.T) {
	if err := ValidateContent(); err != nil {
		t.Fatalf("ValidateContent: %v", err)
	}
}
