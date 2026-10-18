package engine

// content.go holds the game data: the bestiary, the item tables, and
// the weighted spawn/lookup helpers the engine calls. The content task
// owns this file and its content_*.go siblings; the contract is only
// that the unexported helpers keep their signatures, because engine
// code calls them.
//
// Spawn weighting: MinDepth is the earliest level a def may appear.
// Monsters and gear (weapons, armor) then hold full Weight for a short
// grace window and halve each level beyond it, so shallow natives thin
// out on deep floors instead of cutting off hard. Consumables —
// potions, scrolls, food, gold — keep flat Weight at every depth; the
// boss and the Brass Idol carry Weight 0 and are placed by scripted
// level generation, never by these helpers.

import "math/rand/v2"

// depthGrace is how many levels past MinDepth a decaying def keeps its
// full Weight before its effective spawn weight halves per level.
const depthGrace = 2

// monsterDefs is the bestiary, ordered by earliest appearance. Glyphs
// are unique ASCII letters (every other glyph value is reserved for
// terrain, the player, or items) and MinDepth spreads entries across
// 1..MaxDepth so each floor reads differently.
var monsterDefs = []MonsterDef{
	{ID: "giant_rat", Name: "giant rat", Glyph: 'r', HP: 4, Attack: 2, Defense: 0, XP: 2, AI: AIPack, MinDepth: 1, Weight: 14},
	{ID: "cellar_bat", Name: "cellar bat", Glyph: 'b', HP: 3, Attack: 1, Defense: 0, XP: 2, AI: AIWander, MinDepth: 1, Weight: 14},
	{ID: "kobold", Name: "kobold tapster", Glyph: 'k', HP: 6, Attack: 2, Defense: 0, XP: 4, AI: AIFlee, MinDepth: 1, Weight: 10},
	{ID: "cask_gremlin", Name: "cask gremlin", Glyph: 'G', HP: 7, Attack: 3, Defense: 0, XP: 5, AI: AIWander, MinDepth: 2, Weight: 10},
	{ID: "goblin", Name: "goblin", Glyph: 'g', HP: 8, Attack: 3, Defense: 0, XP: 6, AI: AIHunt, MinDepth: 2, Weight: 11},
	{ID: "pit_viper", Name: "pit viper", Glyph: 's', HP: 9, Attack: 4, Defense: 0, XP: 8, AI: AIHunt, MinDepth: 3, Weight: 9},
	{ID: "cellar_spider", Name: "cellar spider", Glyph: 'p', HP: 8, Attack: 3, Defense: 0, XP: 7, AI: AIPack, MinDepth: 3, Weight: 9},
	{ID: "orc", Name: "orc bruiser", Glyph: 'o', HP: 12, Attack: 5, Defense: 1, XP: 12, AI: AIHunt, MinDepth: 4, Weight: 9},
	{ID: "skeleton", Name: "cellarer's skeleton", Glyph: 'z', HP: 10, Attack: 4, Defense: 1, XP: 10, AI: AIHunt, MinDepth: 4, Weight: 9},
	{ID: "cultist", Name: "Underhall cultist", Glyph: 'c', HP: 10, Attack: 6, Defense: 0, XP: 14, AI: AIRanged, MinDepth: 5, Weight: 7, Range: 5},
	{ID: "drain_lurker", Name: "drain lurker", Glyph: 'u', HP: 14, Attack: 5, Defense: 1, XP: 13, AI: AIFlee, MinDepth: 5, Weight: 7},
	{ID: "mold_toad", Name: "mold toad", Glyph: 't', HP: 16, Attack: 7, Defense: 1, XP: 18, AI: AIRanged, MinDepth: 6, Weight: 6, Range: 4},
	{ID: "ogre", Name: "ogre bouncer", Glyph: 'O', HP: 22, Attack: 8, Defense: 2, XP: 26, AI: AIHunt, MinDepth: 6, Weight: 6},
	{ID: "barrow_wight", Name: "barrow wight", Glyph: 'w', HP: 18, Attack: 7, Defense: 2, XP: 30, AI: AIGuard, MinDepth: 7, Weight: 6},
	{ID: BossID, Name: "Gilded Matriarch", Glyph: 'M', HP: 40, Attack: 10, Defense: 4, XP: 200, AI: AIGuard, MinDepth: MaxDepth, Weight: 0},
}

// itemDefs is the item table, ordered by earliest appearance. Item
// glyphs are fixed by ItemKind (every potion draws '!', every weapon
// '/', and so on); uniqueness lives on the monster side. Power's
// meaning is per-kind: weapon damage, armor soak, heal/buff magnitude,
// scroll effect magnitude, food satiety, gold pile size.
var itemDefs = []ItemDef{
	{ID: "dagger", Name: "dagger", Glyph: GlyphWeapon, Kind: ItemWeapon, Power: 3, MinDepth: 1, Weight: 8},
	{ID: "club", Name: "taproom club", Glyph: GlyphWeapon, Kind: ItemWeapon, Power: 4, MinDepth: 1, Weight: 8},
	{ID: "rags", Name: "moth-eaten rags", Glyph: GlyphArmor, Kind: ItemArmor, Power: 1, MinDepth: 1, Weight: 8},
	{ID: "potion_heal", Name: "potion of healing", Glyph: GlyphPotion, Kind: ItemPotion, Power: 14, MinDepth: 1, Weight: 10,
		UseText: "You quaff the potion; it tastes of brandy and sawdust, and your wounds knit."},
	{ID: "iron_ration", Name: "iron ration", Glyph: GlyphFood, Kind: ItemFood, Power: 400, MinDepth: 1, Weight: 9,
		UseText: "You eat the ration; it tastes like pennies, but it keeps you standing."},
	{ID: "pickled_egg", Name: "pickled egg", Glyph: GlyphFood, Kind: ItemFood, Power: 180, MinDepth: 1, Weight: 7,
		UseText: "You eat the pickled egg; vinegar and sawdust, a Brass Elephant classic."},
	{ID: "gold", Name: "gold coins", Glyph: GlyphGold, Kind: ItemGold, Power: 20, MinDepth: 1, Weight: 14},
	{ID: "short_sword", Name: "short sword", Glyph: GlyphWeapon, Kind: ItemWeapon, Power: 5, MinDepth: 2, Weight: 8},
	{ID: "leather", Name: "leather jerkin", Glyph: GlyphArmor, Kind: ItemArmor, Power: 2, MinDepth: 2, Weight: 7},
	{ID: "potion_vigor", Name: "potion of vigor", Glyph: GlyphPotion, Kind: ItemPotion, Power: 5, MinDepth: 2, Weight: 7,
		UseText: "You drink deep; the cellarer's strength settles into your arms."},
	{ID: "scroll_teleport", Name: "scroll of teleport", Glyph: GlyphScroll, Kind: ItemScroll, Power: 0, MinDepth: 2, Weight: 6,
		UseText: "You read the scroll; the Underhall folds, and you are elsewhere."},
	{ID: "moldy_cheese", Name: "cellar cheese", Glyph: GlyphFood, Kind: ItemFood, Power: 260, MinDepth: 2, Weight: 6,
		UseText: "You eat the cellar cheese; the mold bites back, but it fills the belly."},
	{ID: "scroll_mapping", Name: "scroll of mapping", Glyph: GlyphScroll, Kind: ItemScroll, Power: 0, MinDepth: 3, Weight: 5,
		UseText: "Ink crawls off the scroll and sketches the level into your mind."},
	{ID: "mace", Name: "brass-knobbed mace", Glyph: GlyphWeapon, Kind: ItemWeapon, Power: 6, MinDepth: 4, Weight: 6},
	{ID: "chain", Name: "chain shirt", Glyph: GlyphArmor, Kind: ItemArmor, Power: 3, MinDepth: 4, Weight: 6},
	{ID: "potion_firebreath", Name: "potion of fire-breath", Glyph: GlyphPotion, Kind: ItemPotion, Power: 10, MinDepth: 4, Weight: 6,
		TargetKind: TargetNearest,
		UseText:    "The liquor burns going down, then burns again — outward."},
	{ID: "scroll_fear", Name: "scroll of fear", Glyph: GlyphScroll, Kind: ItemScroll, Power: 0, MinDepth: 5, Weight: 4,
		TargetKind: TargetAllVisible,
		UseText:    "You read the scroll; somewhere below an elephant trumpets, and the dark things flinch."},
	{ID: "pike", Name: "vault pike", Glyph: GlyphWeapon, Kind: ItemWeapon, Power: 8, MinDepth: 6, Weight: 5},
	{ID: "plate", Name: "plate harness", Glyph: GlyphArmor, Kind: ItemArmor, Power: 4, MinDepth: 6, Weight: 5},
	{ID: "scroll_immolation", Name: "scroll of immolation", Glyph: GlyphScroll, Kind: ItemScroll, Power: 12, MinDepth: 6, Weight: 4,
		TargetKind: TargetAllVisible,
		UseText:    "The scroll ignites in your hands; fire answers everything it can see."},
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

// decayWeight halves w once per level beyond minDepth+depthGrace.
// Shifting a bounded positive weight by any depth distance is safe and
// lands on zero, which is exactly the "thinned out" we want.
func decayWeight(w, minDepth, depth int) int {
	if over := depth - minDepth - depthGrace; over > 0 {
		return w >> over
	}
	return w
}

// monsterWeight is m's effective spawn weight at depth: zero before
// MinDepth or when Weight is zero, decaying past the grace window.
func monsterWeight(m *MonsterDef, depth int) int {
	if m.Weight <= 0 || depth < m.MinDepth {
		return 0
	}
	return decayWeight(m.Weight, m.MinDepth, depth)
}

// itemWeight is d's effective spawn weight at depth. Weapons and armor
// decay like monsters so obsolete gear stops littering deep floors;
// everything else keeps its flat Weight at any eligible depth.
func itemWeight(d *ItemDef, depth int) int {
	if d.Weight <= 0 || depth < d.MinDepth {
		return 0
	}
	if d.Kind == ItemWeapon || d.Kind == ItemArmor {
		return decayWeight(d.Weight, d.MinDepth, depth)
	}
	return d.Weight
}

// spawnMonster picks a def at random, weighted by effective weight at
// depth. Returns nil only when nothing can spawn (a corrupt table —
// ValidateContent guarantees coverage of every level).
func spawnMonster(r *rand.Rand, depth int) *MonsterDef {
	var total int
	for i := range monsterDefs {
		total += monsterWeight(&monsterDefs[i], depth)
	}
	if total <= 0 {
		return nil
	}
	roll := r.IntN(total)
	for i := range monsterDefs {
		if w := monsterWeight(&monsterDefs[i], depth); w > 0 {
			if roll -= w; roll < 0 {
				return &monsterDefs[i]
			}
		}
	}
	return nil
}

// spawnItem picks a def at random, weighted by effective weight at
// depth. Returns nil only when nothing can spawn.
func spawnItem(r *rand.Rand, depth int) *ItemDef {
	var total int
	for i := range itemDefs {
		total += itemWeight(&itemDefs[i], depth)
	}
	if total <= 0 {
		return nil
	}
	roll := r.IntN(total)
	for i := range itemDefs {
		if w := itemWeight(&itemDefs[i], depth); w > 0 {
			if roll -= w; roll < 0 {
				return &itemDefs[i]
			}
		}
	}
	return nil
}
