package engine

// content.go holds the game data: the bestiary, the item tables, and
// the weighted spawn/lookup helpers the engine calls. THE CONTENT TASK
// OWNS THIS FILE and may replace it wholesale and add sibling
// content_*.go files — the contract is only that the unexported helpers
// below keep their signatures, because engine code calls them.
//
// This stub ships the minimum viable table (a rat, the boss, the
// artifact, and a few staples) so the contract skeleton compiles and
// links. The content task replaces it with the real bestiary (~14
// monsters across the AI kinds), item tables (~18 entries across all
// ItemKinds), per-depth weighting, and validation.

import "math/rand/v2"

// monsterDefs is the bestiary, ordered by earliest appearance.
var monsterDefs = []MonsterDef{
	{ID: "rat", Name: "giant rat", Glyph: 'r', HP: 4, Attack: 2, Defense: 0, XP: 2, AI: AIPack, MinDepth: 1, Weight: 10},
	{ID: BossID, Name: "Gilded Matriarch", Glyph: 'M', HP: 40, Attack: 10, Defense: 4, XP: 200, AI: AIGuard, MinDepth: MaxDepth, Weight: 0},
}

// itemDefs is the item table, ordered by earliest appearance.
var itemDefs = []ItemDef{
	{ID: "ration", Name: "iron ration", Glyph: GlyphFood, Kind: ItemFood, Power: 400, MinDepth: 1, Weight: 8, UseText: "You eat the ration. It tastes like pennies."},
	{ID: "heal_potion", Name: "potion of healing", Glyph: GlyphPotion, Kind: ItemPotion, Power: 12, MinDepth: 1, Weight: 8, UseText: "You quaff the potion. Warmth spreads through you."},
	{ID: "dagger", Name: "dagger", Glyph: GlyphWeapon, Kind: ItemWeapon, Power: 3, MinDepth: 1, Weight: 6},
	{ID: "rags", Name: "moth-eaten rags", Glyph: GlyphArmor, Kind: ItemArmor, Power: 1, MinDepth: 1, Weight: 6},
	{ID: "gold", Name: "gold coins", Glyph: GlyphGold, Kind: ItemGold, Power: 0, MinDepth: 1, Weight: 10},
	{ID: ArtifactID, Name: "the Brass Idol", Glyph: GlyphArtifact, Kind: ItemArtifact, Power: 0, MinDepth: MaxDepth, Weight: 0},
}

// lookupMonster returns the def for id, or nil.
func lookupMonster(id string) *MonsterDef {
	for i := range monsterDefs {
		if monsterDefs[i].ID == id {
			return &monsterDefs[i]
		}
	}
	return nil
}

// lookupItem returns the def for id, or nil.
func lookupItem(id string) *ItemDef {
	for i := range itemDefs {
		if itemDefs[i].ID == id {
			return &itemDefs[i]
		}
	}
	return nil
}

// spawnMonster picks a weighted random def eligible at depth (Weight>0,
// MinDepth<=depth). Falls back to the first eligible def; nil if none.
func spawnMonster(r *rand.Rand, depth int) *MonsterDef {
	var total int
	for i := range monsterDefs {
		m := &monsterDefs[i]
		if m.Weight > 0 && m.MinDepth <= depth {
			total += m.Weight
		}
	}
	if total == 0 {
		return nil
	}
	roll := r.IntN(total)
	for i := range monsterDefs {
		m := &monsterDefs[i]
		if m.Weight > 0 && m.MinDepth <= depth {
			roll -= m.Weight
			if roll < 0 {
				return m
			}
		}
	}
	return nil
}

// spawnItem picks a weighted random item eligible at depth.
func spawnItem(r *rand.Rand, depth int) *ItemDef {
	var total int
	for i := range itemDefs {
		d := &itemDefs[i]
		if d.Weight > 0 && d.MinDepth <= depth {
			total += d.Weight
		}
	}
	if total == 0 {
		return nil
	}
	roll := r.IntN(total)
	for i := range itemDefs {
		d := &itemDefs[i]
		if d.Weight > 0 && d.MinDepth <= depth {
			roll -= d.Weight
			if roll < 0 {
				return d
			}
		}
	}
	return nil
}

// ValidateContent checks internal table consistency: unique IDs,
// glyphs present, sane numbers, boss and artifact exist and are
// depth-pinned to MaxDepth, every level 1..MaxDepth can spawn at least
// one monster and one item. Engine tests and app Init may call it; it
// must not depend on a Game.
func ValidateContent() error { return nil }
