package engine

// fov.go is visibility: a raycast FOV over the Chebyshev SightRadius
// box. Every in-box cell gets a Bresenham line from the player; the
// cell is visible if nothing opaque precedes it, and walls show as the
// last visible cell of a ray. The same line walk answers monster LOS
// questions.

// computeFOV refreshes g.visible for the active level and folds newly
// seen tiles into the level's Seen memory.
func (g *Game) computeFOV() {
	l := g.level(g.Depth)
	if len(g.visible) != len(l.Tiles) {
		g.visible = make([]bool, len(l.Tiles))
	} else {
		for i := range g.visible {
			g.visible[i] = false
		}
	}
	c := g.Player.Pos
	y0, y1 := max(0, c.Y-SightRadius), min(l.Height-1, c.Y+SightRadius)
	x0, x1 := max(0, c.X-SightRadius), min(l.Width-1, c.X+SightRadius)
	for y := y0; y <= y1; y++ {
		for x := x0; x <= x1; x++ {
			g.castRay(l, c, Pos{x, y})
		}
	}
	for i, v := range g.visible {
		if v {
			l.Seen[i] = true
		}
	}
}

// castRay walks the line from→to, marking cells visible until (and
// including) the first wall or the map edge.
func (g *Game) castRay(l *Level, from, to Pos) {
	forLine(from, to, func(p Pos) bool {
		if !l.InBounds(p) {
			return false
		}
		g.visible[p.Y*l.Width+p.X] = true
		return l.At(p) != TileWall
	})
}

// los reports whether a sees b: no wall strictly between them.
func los(l *Level, a, b Pos) bool {
	ok := true
	forLine(a, b, func(p Pos) bool {
		if p.Eq(a) || p.Eq(b) {
			return true
		}
		if !l.InBounds(p) || l.At(p) == TileWall {
			ok = false
			return false
		}
		return true
	})
	return ok
}

// forLine walks a Bresenham line from a to b inclusive, calling f per
// cell; f returning false stops the walk early.
func forLine(a, b Pos, f func(Pos) bool) {
	x0, y0, x1, y1 := a.X, a.Y, b.X, b.Y
	dx := x1 - x0
	if dx < 0 {
		dx = -dx
	}
	dy := y0 - y1
	if dy > 0 {
		dy = -dy
	}
	sx, sy := 1, 1
	if x0 > x1 {
		sx = -1
	}
	if y0 > y1 {
		sy = -1
	}
	err := dx + dy
	for {
		if !f(Pos{x0, y0}) || (x0 == x1 && y0 == y1) {
			return
		}
		e2 := 2 * err
		if e2 >= dy {
			err += dy
			x0 += sx
		}
		if e2 <= dx {
			err += dx
			y0 += sy
		}
	}
}

// cheb is the Chebyshev distance between two tiles.
func cheb(a, b Pos) int {
	dx := a.X - b.X
	if dx < 0 {
		dx = -dx
	}
	dy := a.Y - b.Y
	if dy < 0 {
		dy = -dy
	}
	return max(dx, dy)
}
