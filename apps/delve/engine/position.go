package engine

// position.go exposes engine truth to the presentation layer without
// widening the frozen types.go contract. A map focal point must come from
// the player's actual tile, never from parsing rendered glyphs: glyphs are
// view data, and remembered-only tiles or overlapping monsters would make
// them ambiguous.

// PlayerPosition reports the delver's tile on the active level.
func (g *Game) PlayerPosition() Pos { return g.Player.Pos }
