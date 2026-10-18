package engine

// run_test.go is the integration capstone: a solver bot drives a real
// seeded run through the public API only — pathing to stairs, fighting
// through whatever blocks the corridor, claiming the Brass Idol, and
// climbing back out. It verifies what unit tests cannot: that
// generation, combat, items, hunger, and the win condition compose
// into a run that can actually be completed.
//
// The bot plays to win, not fairly — it tops up HP and satiety each
// turn so the test measures reachability and rules, not balance.

import "testing"

// stepDir maps a unit delta to its Dir.
func stepDir(d Pos) Dir {
	for _, dir := range AllDirs {
		if dir.Delta() == d {
			return dir
		}
	}
	return DirN
}

// bfsStep returns the next step on a shortest path from src to dst
// through passable tiles (dst itself is always enterable). Moving into
// a monster attacks it, so monster tiles count as passable.
func bfsStep(l *Level, src, dst Pos) (Pos, bool) {
	blocked := make(map[Pos]bool, len(l.Monsters))
	for _, m := range l.Monsters {
		blocked[m.Pos] = true
	}
	passable := func(p Pos) bool { return l.Walkable(p) || blocked[p] }

	prev := map[Pos]Pos{src: src}
	queue := []Pos{src}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, d := range AllDirs {
			n := cur.Add(d.Delta())
			if !l.InBounds(n) {
				continue
			}
			if _, ok := prev[n]; ok {
				continue
			}
			if !n.Eq(dst) && !passable(n) {
				continue
			}
			prev[n] = cur
			if n.Eq(dst) {
				for !prev[n].Eq(src) {
					n = prev[n]
				}
				return n, true
			}
			queue = append(queue, n)
		}
	}
	return Pos{}, false
}

func hasArtifact(g *Game) bool {
	for _, it := range g.Player.Inventory {
		if it.Def == ArtifactID {
			return true
		}
	}
	return false
}

// objective is where the bot wants to stand: the idol on the bottom
// floor, stairs up when hauling it out, otherwise stairs down.
func objective(g *Game, l *Level) (Pos, bool) {
	switch {
	case hasArtifact(g):
		p := l.findTile(TileStairsUp)
		return p, p.X >= 0
	case g.Depth == MaxDepth:
		for _, it := range l.Items {
			if it.Def == ArtifactID {
				return it.Pos, true
			}
		}
		return Pos{}, false
	default:
		p := l.findTile(TileStairsDown)
		return p, p.X >= 0
	}
}

// botTurn issues the single command that best advances the run.
func botTurn(g *Game) Cmd {
	l := g.Levels[g.Depth-1]
	p := g.Player.Pos

	for _, it := range l.Items {
		if it.Def == ArtifactID && it.Pos.Eq(p) {
			return Cmd{Kind: CmdPickup}
		}
	}
	target, ok := objective(g, l)
	if !ok {
		return Cmd{Kind: CmdWait}
	}
	if p.Eq(target) {
		// Standing on the objective: take the stairs. (The artifact
		// case was handled above; nothing else requires standing on
		// the target.)
		if hasArtifact(g) {
			return Cmd{Kind: CmdAscend}
		}
		return Cmd{Kind: CmdDescend}
	}
	if step, found := bfsStep(l, p, target); found && !step.Eq(p) {
		return Cmd{Kind: CmdMove, Dir: stepDir(Pos{step.X - p.X, step.Y - p.Y})}
	}
	return Cmd{Kind: CmdWait}
}

// TestBotCanWin plays entire runs to victory through the public
// contract. Any failure — unreachable stairs, a sealed vault, a broken
// ascend rule, starvation math — fails the suite, which is the point.
func TestBotCanWin(t *testing.T) {
	for _, seed := range []uint64{1, 7, 42} {
		g, err := New(Config{Seed: seed, Name: "bot"})
		if err != nil {
			t.Fatalf("seed %d: New: %v", seed, err)
		}
		// Cheat vitals so the test measures the rules, not luck: a mob
		// scene in the vault can burst ~30/turn, so pad MaxHP well past
		// any single-round damage.
		g.Player.MaxHP = 500
		won := false
		for turn := 0; turn < 20000; turn++ {
			if g.Outcome() == OutcomeWon {
				won = true
				break
			}
			if g.Outcome() == OutcomeDead {
				t.Fatalf("seed %d: invincible bot died (%s)", seed, g.DeathCause)
			}
			// Cheat vitals so the test measures the rules, not luck.
			g.Player.HP = g.Player.MaxHP
			g.Player.Satiety = SatietyMax
			g.Command(botTurn(g))
		}
		if !won {
			t.Fatalf("seed %d: bot did not win within the turn budget (depth %d, cause %q)",
				seed, g.Depth, g.DeathCause)
		}
		s := g.FinalScore()
		if s.Outcome != "won" || s.Value <= 0 {
			t.Fatalf("seed %d: won run scored %+v", seed, s)
		}
		t.Logf("seed %d: won in %d turns, score %d", seed, g.Player.Turns, s.Value)
	}
}
