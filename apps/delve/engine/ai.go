package engine

// ai.go moves every monster once per player turn. Hunters share a BFS
// distance field computed from the player's tile each phase; monsters
// that can't reach the player fall back to a greedy step, and nobody
// walks through walls or occupied tiles.

// monsterPhase gives each living monster on the active floor one
// action, in spawn order.
func (g *Game) monsterPhase() []Event {
	l := g.level(g.Depth)
	dist := bfsField(l, g.Player.Pos)
	var evs []Event
	for i := range l.Monsters {
		if g.Out != OutcomeOngoing {
			break
		}
		m := &l.Monsters[i]
		def := lookupMonster(m.Def)
		if def == nil || m.HP <= 0 {
			continue
		}
		evs = append(evs, g.actMonster(l, m, def, dist)...)
	}
	return evs
}

// actMonster runs one monster's turn. adj implies LOS (adjacent cells
// always see each other), so it feeds sees directly.
func (g *Game) actMonster(l *Level, m *Monster, def *MonsterDef, dist []int) []Event {
	adj := cheb(m.Pos, g.Player.Pos) <= 1
	sees := adj || g.monsterSees(l, m)

	switch def.AI {
	case AIGuard:
		// Holds its lair until the player comes close, then hunts
		// forever. The anchor rides in State (y*MapWidth + x).
		if !m.Alert {
			if m.State == 0 {
				m.State = m.Pos.Y*MapWidth + m.Pos.X
			}
			if sees && cheb(m.Pos, g.Player.Pos) <= 6 {
				m.Alert = true
			} else {
				g.guardStep(l, m)
				return nil
			}
		}
	case AIPack:
		// First sighting wakes same-def allies within 3 tiles.
		if sees && !m.Alert {
			m.Alert = true
			for j := range l.Monsters {
				o := &l.Monsters[j]
				if o != m && o.Def == m.Def && cheb(o.Pos, m.Pos) <= 3 {
					o.Alert = true
				}
			}
			evs := msg("The " + def.Name + " calls to its kin!")
			if adj {
				return append(evs, g.monsterAttack(m, def, false)...)
			}
			g.huntStep(l, m, dist)
			return evs
		}
	case AIRanged:
		rn := def.Range
		if rn <= 0 {
			rn = 4
		}
		if !sees {
			g.wanderStep(l, m)
			return nil
		}
		d := cheb(m.Pos, g.Player.Pos)
		switch {
		case adj:
			return g.monsterAttack(m, def, false)
		case d < 3:
			g.fleeStep(l, m)
		case d > 5 || d > rn:
			g.huntStep(l, m, dist)
		case los(l, m.Pos, g.Player.Pos):
			return g.monsterAttack(m, def, true)
		default:
			g.huntStep(l, m, dist)
		}
		return nil
	case AIFlee:
		if sees && m.HP*10 < def.HP*3 {
			g.fleeStep(l, m)
			return nil
		}
	}

	// Hunt-capable kinds: chase when the player is sensed, otherwise
	// shuffle (wander/pack/flee) or hold (hunt).
	if !sees && !m.Alert {
		switch def.AI {
		case AIWander, AIPack, AIFlee:
			g.wanderStep(l, m)
		}
		return nil
	}
	if adj {
		return g.monsterAttack(m, def, false)
	}
	g.huntStep(l, m, dist)
	return nil
}

// monsterSees reports whether m can perceive the player: within sight
// range with a clear line.
func (g *Game) monsterSees(l *Level, m *Monster) bool {
	return cheb(m.Pos, g.Player.Pos) <= SightRadius && los(l, m.Pos, g.Player.Pos)
}

// huntStep moves m one tile along the BFS distance field toward the
// player. Unreachable monsters take a greedy approach instead.
func (g *Game) huntStep(l *Level, m *Monster, dist []int) {
	cur := m.Pos.Y*l.Width + m.Pos.X
	bd := dist[cur]
	if bd < 0 {
		bd = 1 << 30
	}
	best := Pos{-1, -1}
	for _, d := range AllDirs {
		np := m.Pos.Add(d.Delta())
		if !l.InBounds(np) || np.Eq(g.Player.Pos) {
			continue
		}
		nd := dist[np.Y*l.Width+np.X]
		if nd >= 0 && nd < bd && g.canStep(l, m, np) {
			bd = nd
			best = np
		}
	}
	if best.X >= 0 {
		m.Pos = best
		return
	}
	if dist[cur] < 0 {
		// No route to the player; drift closer by raw distance.
		base := cheb(m.Pos, g.Player.Pos)
		for _, d := range AllDirs {
			np := m.Pos.Add(d.Delta())
			if cheb(np, g.Player.Pos) < base && g.moveMonster(l, m, np) {
				return
			}
		}
	}
}

// fleeStep moves m to the neighbor farthest from the player.
func (g *Game) fleeStep(l *Level, m *Monster) {
	bd := cheb(m.Pos, g.Player.Pos)
	best := Pos{-1, -1}
	for _, d := range AllDirs {
		np := m.Pos.Add(d.Delta())
		if nd := cheb(np, g.Player.Pos); nd > bd && g.canStep(l, m, np) {
			bd = nd
			best = np
		}
	}
	if best.X >= 0 {
		m.Pos = best
	}
}

// wanderStep is a random one-tile shuffle; blocked tries do nothing.
func (g *Game) wanderStep(l *Level, m *Monster) {
	d := AllDirs[g.rng.IntN(len(AllDirs))]
	g.moveMonster(l, m, m.Pos.Add(d.Delta()))
}

// guardStep walks an unalerted guard back toward its lair anchor.
func (g *Game) guardStep(l *Level, m *Monster) {
	anchor := Pos{X: m.State % MapWidth, Y: m.State / MapWidth}
	if m.Pos.Eq(anchor) || !l.Walkable(anchor) {
		return
	}
	cur := cheb(m.Pos, anchor)
	for _, d := range AllDirs {
		np := m.Pos.Add(d.Delta())
		if cheb(np, anchor) < cur && g.moveMonster(l, m, np) {
			return
		}
	}
}

// canStep reports whether m may occupy dst: walkable, unoccupied, not
// the player's tile, and not a diagonal squeeze between two walls.
func (g *Game) canStep(l *Level, m *Monster, dst Pos) bool {
	if !l.Walkable(dst) || l.monsterAt(dst) != nil || g.Player.Pos.Eq(dst) {
		return false
	}
	if dst.X != m.Pos.X && dst.Y != m.Pos.Y {
		return l.Walkable(Pos{dst.X, m.Pos.Y}) || l.Walkable(Pos{m.Pos.X, dst.Y})
	}
	return true
}

// moveMonster applies a one-tile move if canStep allows it.
func (g *Game) moveMonster(l *Level, m *Monster, dst Pos) bool {
	if !g.canStep(l, m, dst) {
		return false
	}
	m.Pos = dst
	return true
}

// bfsField floods the floor from the player's tile, returning distance
// in steps per cell (-1 = unreachable). Diagonal steps may not squeeze
// between two walls, matching canStep.
func bfsField(l *Level, from Pos) []int {
	dist := make([]int, l.Width*l.Height)
	for i := range dist {
		dist[i] = -1
	}
	if !l.Walkable(from) {
		return dist
	}
	q := []Pos{from}
	dist[from.Y*l.Width+from.X] = 0
	for len(q) > 0 {
		p := q[0]
		q = q[1:]
		pd := dist[p.Y*l.Width+p.X]
		for _, d := range AllDirs {
			np := p.Add(d.Delta())
			if !l.Walkable(np) {
				continue
			}
			if np.X != p.X && np.Y != p.Y &&
				!l.Walkable(Pos{np.X, p.Y}) && !l.Walkable(Pos{p.X, np.Y}) {
				continue
			}
			i := np.Y*l.Width + np.X
			if dist[i] >= 0 {
				continue
			}
			dist[i] = pd + 1
			q = append(q, np)
		}
	}
	return dist
}
