package engine

import "testing"

// TestConnectivity checks that every generated floor is fully
// connected: BFS from the up stairs must reach every walkable tile and
// both staircases, across many seeds and all depths.
func TestConnectivity(t *testing.T) {
	for seed := uint64(1); seed <= 40; seed++ {
		g := newTestGame(t, seed)
		for d := 1; d <= MaxDepth; d++ {
			l := g.level(d)
			up := l.findTile(TileStairsUp)
			if up.X < 0 {
				t.Fatalf("seed %d depth %d: no stairs up", seed, d)
			}
			dist := bfsField(l, up)
			walkable, reached := 0, 0
			for i, k := range l.Tiles {
				if k == TileWall {
					continue
				}
				walkable++
				if dist[i] >= 0 {
					reached++
				}
			}
			if reached != walkable {
				t.Fatalf("seed %d depth %d: %d of %d walkable tiles reachable",
					seed, d, reached, walkable)
			}
			down := l.findTile(TileStairsDown)
			if down.X < 0 {
				t.Fatalf("seed %d depth %d: no stairs down", seed, d)
			}
			if dist[down.Y*l.Width+down.X] < 0 {
				t.Fatalf("seed %d depth %d: stairs down unreachable", seed, d)
			}
		}
	}
}

// TestDepthEightVault checks the deepest floor holds the Brass Idol and
// the Gilded Matriarch, plus a way back up.
func TestDepthEightVault(t *testing.T) {
	g := newTestGame(t, 17)
	l := g.level(MaxDepth)
	var boss, idol bool
	for _, m := range l.Monsters {
		if m.Def == BossID {
			boss = true
		}
	}
	for _, it := range l.Items {
		if it.Def == ArtifactID {
			idol = true
		}
	}
	if !boss {
		t.Fatal("no boss on MaxDepth")
	}
	if !idol {
		t.Fatal("no artifact on MaxDepth")
	}
	if l.findTile(TileStairsUp).X < 0 {
		t.Fatal("MaxDepth has no way back up")
	}
}

// TestSpawnCaps bounds the population of every floor.
func TestSpawnCaps(t *testing.T) {
	for seed := uint64(1); seed <= 12; seed++ {
		g := newTestGame(t, seed)
		for d := 1; d <= MaxDepth; d++ {
			l := g.level(d)
			if len(l.Monsters) > maxMonsters+1 { // +1 for the boss
				t.Fatalf("seed %d depth %d: %d monsters", seed, d, len(l.Monsters))
			}
			if len(l.Items) > maxItems+1 { // +1 for the artifact
				t.Fatalf("seed %d depth %d: %d items", seed, d, len(l.Items))
			}
		}
	}
}

// TestFOV verifies the FOV is radius-bounded and folds into Seen.
func TestFOV(t *testing.T) {
	g := newTestGame(t, 9)
	l := g.Levels[0]
	vis := 0
	for i, v := range g.visible {
		if !v {
			continue
		}
		vis++
		p := Pos{X: i % l.Width, Y: i / l.Width}
		if cheb(p, g.Player.Pos) > SightRadius {
			t.Fatalf("visible tile beyond radius at %v", p)
		}
		if !l.Seen[i] {
			t.Fatalf("visible tile %v not marked seen", p)
		}
	}
	if vis == 0 {
		t.Fatal("nothing visible")
	}
	if vis > (2*SightRadius+1)*(2*SightRadius+1) {
		t.Fatalf("%d visible tiles exceeds radius box", vis)
	}
	if pi := g.Player.Pos.Y*l.Width + g.Player.Pos.X; !g.visible[pi] {
		t.Fatal("player tile not visible")
	}
}

// TestMapViewDimensions checks the renderer's output contract: exactly
// MapHeight lines of exactly MapWidth runes.
func TestMapViewDimensions(t *testing.T) {
	g := newTestGame(t, 4)
	view := g.MapView()
	if len(view) != MapHeight {
		t.Fatalf("%d rows, want %d", len(view), MapHeight)
	}
	for i, row := range view {
		if n := len([]rune(row)); n != MapWidth {
			t.Fatalf("row %d: %d runes, want %d", i, n, MapWidth)
		}
	}
	if view[g.Player.Pos.Y][g.Player.Pos.X] != GlyphPlayer {
		t.Fatal("player glyph missing from view")
	}
}

// TestLevelsPersist verifies revisiting a floor keeps its state.
func TestLevelsPersist(t *testing.T) {
	g := newTestGame(t, 6)
	l1 := g.Levels[0]
	l1.Monsters = nil
	l1.Items = nil
	g.Player.Pos = l1.findTile(TileStairsDown)
	if evs := g.Command(Cmd{Kind: CmdDescend}); !hasKind(evs, EvDescend) {
		t.Fatalf("descend failed: %v", evs)
	}
	if g.Depth != 2 {
		t.Fatalf("depth %d, want 2", g.Depth)
	}
	// Mark level 2, then come back up and down; it must be the same
	// object, still holding the mark.
	l2 := g.Levels[1]
	l2.Monsters = nil
	g.Player.Pos = l2.findTile(TileStairsUp)
	g.Command(Cmd{Kind: CmdAscend})
	if g.Depth != 1 || g.Levels[0] != l1 {
		t.Fatal("level 1 state not preserved")
	}
	g.Player.Pos = l1.findTile(TileStairsDown)
	g.Command(Cmd{Kind: CmdDescend})
	if g.Levels[1] != l2 {
		t.Fatal("level 2 regenerated instead of persisting")
	}
}
