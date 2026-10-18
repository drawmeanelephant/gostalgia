package engine

// gen.go builds levels: a scatter of non-overlapping rooms joined by
// L-shaped corridors in a chain (each new room links to the previous
// one, with occasional loops to a random earlier room). Chaining makes
// full connectivity structural rather than incidental; the tests still
// BFS-verify it across seeds.

// Spawn and room budgets keep every floor bounded.
const (
	maxRooms    = 12
	maxMonsters = 14
	maxItems    = 10
)

// rect is a room outline in tile coordinates.
type rect struct{ x, y, w, h int }

func (r rect) center() Pos     { return Pos{X: r.x + r.w/2, Y: r.y + r.h/2} }
func (r rect) grow(n int) rect { return rect{r.x - n, r.y - n, r.w + 2*n, r.h + 2*n} }
func (r rect) overlaps(o rect) bool {
	return r.x < o.x+o.w && o.x < r.x+r.w && r.y < o.y+o.h && o.y < r.y+r.h
}
func (r rect) contains(p Pos) bool {
	return p.X >= r.x && p.X < r.x+r.w && p.Y >= r.y && p.Y < r.y+r.h
}

// genLevel builds the floor for depth d: rooms, corridors, both stairs,
// depth-weighted monsters and items, and on MaxDepth the vault holding
// the Brass Idol and the Gilded Matriarch.
func (g *Game) genLevel(depth int) *Level {
	l := &Level{
		Width:  MapWidth,
		Height: MapHeight,
		Tiles:  make([]TileKind, MapWidth*MapHeight),
		Seen:   make([]bool, MapWidth*MapHeight),
	}
	var rooms []rect
	for try := 0; try < 300 && len(rooms) < maxRooms; try++ {
		w := 4 + g.rng.IntN(8)
		h := 3 + g.rng.IntN(5)
		r := rect{
			x: 1 + g.rng.IntN(MapWidth-w-1),
			y: 1 + g.rng.IntN(MapHeight-h-1),
			w: w, h: h,
		}
		blocked := false
		for _, o := range rooms {
			if r.grow(1).overlaps(o) {
				blocked = true
				break
			}
		}
		if blocked {
			continue
		}
		rooms = append(rooms, r)
		carveRoom(l, r)
		if len(rooms) > 1 {
			g.carveCorridor(l, rooms[len(rooms)-2].center(), r.center())
		}
		// Occasional loop back to an earlier room keeps floors from
		// being pure chains.
		if len(rooms) > 2 && g.rng.IntN(3) == 0 {
			g.carveCorridor(l, rooms[g.rng.IntN(len(rooms)-1)].center(), r.center())
		}
	}
	if len(rooms) == 0 {
		rooms = []rect{{2, 2, MapWidth - 4, MapHeight - 4}}
		carveRoom(l, rooms[0])
	}

	up := rooms[0].center()
	l.Set(up, TileStairsUp)

	// Stairs down live in the last room — except on MaxDepth, where the
	// last room is the vault, so another room (or a free tile) hosts it.
	down := rooms[len(rooms)-1].center()
	if depth == MaxDepth {
		down = Pos{-1, -1}
		for i := len(rooms) - 2; i >= 1; i-- {
			if c := rooms[i].center(); !c.Eq(up) {
				down = c
				break
			}
		}
		if down.X < 0 {
			vr := rooms[len(rooms)-1]
			down = g.randomFloor(l, func(p Pos) bool {
				return p.Eq(up) || vr.contains(p)
			})
		}
	}
	if down.Eq(up) {
		if alt := g.randomFloor(l, func(p Pos) bool { return p.Eq(up) }); alt.X >= 0 {
			down = alt
		}
	}
	if down.X >= 0 {
		l.Set(down, TileStairsDown)
	}

	// The vault: the Brass Idol at the last room's heart, the Gilded
	// Matriarch guarding beside it.
	if depth == MaxDepth {
		vc := rooms[len(rooms)-1].center()
		g.placeItem(l, ArtifactID, vc, 0)
		if def := lookupMonster(BossID); def != nil {
			bp := vc
			for _, d := range AllDirs {
				np := vc.Add(d.Delta())
				if l.At(np) == TileFloor && l.monsterAt(np) == nil && l.itemAt(np) == nil {
					bp = np
					break
				}
			}
			g.placeMonster(l, def, bp)
		}
	}

	// Depth-weighted population. Monsters keep clear of the arrival
	// room so a fresh floor doesn't open with an ambush.
	nMon := 4 + depth
	if nMon > maxMonsters {
		nMon = maxMonsters
	}
	entry := rooms[0].grow(1)
	for i := 0; i < nMon; i++ {
		def := spawnMonster(g.rng, depth)
		if def == nil {
			break
		}
		p := g.randomFloor(l, func(p Pos) bool {
			return entry.contains(p) || l.monsterAt(p) != nil
		})
		if p.X < 0 {
			continue
		}
		g.placeMonster(l, def, p)
	}

	nIt := 4 + depth/2
	if nIt > maxItems {
		nIt = maxItems
	}
	for i := 0; i < nIt; i++ {
		def := spawnItem(g.rng, depth)
		if def == nil {
			break
		}
		p := g.randomFloor(l, func(p Pos) bool {
			return l.itemAt(p) != nil
		})
		if p.X < 0 {
			continue
		}
		bonus := 0
		if def.Kind == ItemGold {
			bonus = 3 + g.rng.IntN(5+depth*2)
		}
		g.placeItem(l, def.ID, p, bonus)
	}
	return l
}

// carveRoom floors over r.
func carveRoom(l *Level, r rect) {
	for y := r.y; y < r.y+r.h; y++ {
		for x := r.x; x < r.x+r.w; x++ {
			l.Set(Pos{x, y}, TileFloor)
		}
	}
}

// carveCorridor joins a to b with an L-shaped passage (which leg runs
// first is a coin flip). Only walls become floor; existing floor,
// stairs, and later-placed contents are untouched.
func (g *Game) carveCorridor(l *Level, a, b Pos) {
	carve := func(p Pos) {
		if l.At(p) == TileWall {
			l.Set(p, TileFloor)
		}
	}
	hseg := func(x0, x1, y int) {
		if x0 > x1 {
			x0, x1 = x1, x0
		}
		for x := x0; x <= x1; x++ {
			carve(Pos{x, y})
		}
	}
	vseg := func(y0, y1, x int) {
		if y0 > y1 {
			y0, y1 = y1, y0
		}
		for y := y0; y <= y1; y++ {
			carve(Pos{x, y})
		}
	}
	if g.rng.IntN(2) == 0 {
		hseg(a.X, b.X, a.Y)
		vseg(a.Y, b.Y, b.X)
	} else {
		vseg(a.Y, b.Y, a.X)
		hseg(a.X, b.X, b.Y)
	}
}

// randomFloor returns a random floor tile avoiding the reject
// predicate, or {-1,-1} after 100 tries.
func (g *Game) randomFloor(l *Level, reject func(Pos) bool) Pos {
	for t := 0; t < 100; t++ {
		p := Pos{X: g.rng.IntN(MapWidth), Y: g.rng.IntN(MapHeight)}
		if l.At(p) == TileFloor && !reject(p) {
			return p
		}
	}
	return Pos{-1, -1}
}

// placeMonster spawns def at p, stamping the AI anchor for guards.
func (g *Game) placeMonster(l *Level, def *MonsterDef, p Pos) {
	m := Monster{ID: g.nextID, Def: def.ID, Pos: p, HP: def.HP}
	g.nextID++
	if def.AI == AIGuard {
		m.State = p.Y*MapWidth + p.X
	}
	l.Monsters = append(l.Monsters, m)
}

// placeItem drops defID at p with the given per-instance bonus.
func (g *Game) placeItem(l *Level, defID string, p Pos, bonus int) {
	l.Items = append(l.Items, Item{ID: g.nextID, Def: defID, Pos: p, Bonus: bonus})
	g.nextID++
}
