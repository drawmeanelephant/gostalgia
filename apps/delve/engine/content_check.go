package engine

// content_check.go is the table linter. ValidateContent runs in tests
// and may run at app init; it inspects the content tables only, never
// a Game, so the engine does not need to exist for it to work.

import "fmt"

// Sane-bound ceilings for table numbers. Weight is capped so a typo
// can never starve the weighted draws; Power is capped well above
// anything a def should need (the largest legit value is food satiety,
// under SatietyMax).
const (
	maxContentWeight = 1000
	maxContentPower  = 1000
)

// aiKindName names k for validation errors without depending on
// String methods the engine task may or may not define.
func aiKindName(k AIKind) string {
	switch k {
	case AIWander:
		return "AIWander"
	case AIHunt:
		return "AIHunt"
	case AIFlee:
		return "AIFlee"
	case AIRanged:
		return "AIRanged"
	case AIPack:
		return "AIPack"
	case AIGuard:
		return "AIGuard"
	}
	return fmt.Sprintf("AIKind(%d)", int(k))
}

// itemKindName names k for validation errors.
func itemKindName(k ItemKind) string {
	switch k {
	case ItemWeapon:
		return "ItemWeapon"
	case ItemArmor:
		return "ItemArmor"
	case ItemPotion:
		return "ItemPotion"
	case ItemScroll:
		return "ItemScroll"
	case ItemFood:
		return "ItemFood"
	case ItemGold:
		return "ItemGold"
	case ItemArtifact:
		return "ItemArtifact"
	}
	return fmt.Sprintf("ItemKind(%d)", int(k))
}

// kindGlyph returns the canonical map glyph for an ItemKind. Items are
// identified by kind glyph — every potion is '!' — so unlike monster
// letters the item glyphs deliberately repeat within the table.
func kindGlyph(k ItemKind) (rune, bool) {
	switch k {
	case ItemWeapon:
		return GlyphWeapon, true
	case ItemArmor:
		return GlyphArmor, true
	case ItemPotion:
		return GlyphPotion, true
	case ItemScroll:
		return GlyphScroll, true
	case ItemFood:
		return GlyphFood, true
	case ItemGold:
		return GlyphGold, true
	case ItemArtifact:
		return GlyphArtifact, true
	}
	return 0, false
}

// isMonsterGlyph reports whether g is a legal monster letter: an ASCII
// letter, which by construction excludes every reserved glyph ('@',
// terrain, and all the item characters).
func isMonsterGlyph(g rune) bool {
	return (g >= 'a' && g <= 'z') || (g >= 'A' && g <= 'Z')
}

// validTargetKind reports whether s is one of the Target* constants.
func validTargetKind(s string) bool {
	return s == TargetSelf || s == TargetNearest || s == TargetAllVisible
}

// consumable reports whether items of kind k are used up by CmdUseItem
// and therefore need a UseText line.
func consumable(k ItemKind) bool {
	return k == ItemPotion || k == ItemScroll || k == ItemFood
}

// ValidateContent checks internal table consistency: unique IDs;
// monster glyphs that are unique letters; item glyphs canonical for
// their kind; stats, weights, depths, and powers in sane bounds; every
// AIKind and ItemKind represented; every consumable carrying a
// UseText; the boss and the Brass Idol pinned to MaxDepth at Weight 0;
// and at least one monster and one item spawnable on every level
// 1..MaxDepth. It returns the first problem found.
func ValidateContent() error {
	if err := validateMonsters(); err != nil {
		return err
	}
	if err := validateItems(); err != nil {
		return err
	}
	if err := validateFixtures(); err != nil {
		return err
	}
	for depth := 1; depth <= MaxDepth; depth++ {
		if !anyMonsterAt(depth) {
			return fmt.Errorf("delve content: no monster can spawn at depth %d", depth)
		}
		if !anyItemAt(depth) {
			return fmt.Errorf("delve content: no item can spawn at depth %d", depth)
		}
	}
	return nil
}

// validateMonsters lints the bestiary: unique IDs and letter glyphs,
// known AIKinds, non-negative stats, bounded weights and depths, and
// coverage of every AIKind.
func validateMonsters() error {
	ids := make(map[string]bool, len(monsterDefs))
	glyphs := make(map[rune]string, len(monsterDefs))
	seenAI := make(map[AIKind]bool)
	for i := range monsterDefs {
		m := &monsterDefs[i]
		if m.ID == "" {
			return fmt.Errorf("delve content: monsterDefs[%d] has an empty ID", i)
		}
		if ids[m.ID] {
			return fmt.Errorf("delve content: duplicate monster ID %q", m.ID)
		}
		ids[m.ID] = true
		if m.AI > AIGuard {
			return fmt.Errorf("delve content: monster %q has unknown AIKind %d", m.ID, int(m.AI))
		}
		seenAI[m.AI] = true
		if m.Name == "" {
			return fmt.Errorf("delve content: monster %q has an empty name", m.ID)
		}
		if !isMonsterGlyph(m.Glyph) {
			return fmt.Errorf("delve content: monster %q glyph %q is not an ASCII letter", m.ID, m.Glyph)
		}
		if other, dup := glyphs[m.Glyph]; dup {
			return fmt.Errorf("delve content: monster %q glyph %q is already used by %q", m.ID, m.Glyph, other)
		}
		glyphs[m.Glyph] = m.ID
		if m.HP < 0 || m.Attack < 0 || m.Defense < 0 || m.XP < 0 {
			return fmt.Errorf("delve content: monster %q has a negative stat (hp=%d attack=%d defense=%d xp=%d)",
				m.ID, m.HP, m.Attack, m.Defense, m.XP)
		}
		if m.Weight < 0 || m.Weight > maxContentWeight {
			return fmt.Errorf("delve content: monster %q weight %d outside 0..%d", m.ID, m.Weight, maxContentWeight)
		}
		if m.MinDepth < 1 || m.MinDepth > MaxDepth {
			return fmt.Errorf("delve content: monster %q MinDepth %d outside 1..%d", m.ID, m.MinDepth, MaxDepth)
		}
		if m.Range < 0 {
			return fmt.Errorf("delve content: monster %q has negative Range %d", m.ID, m.Range)
		}
		if m.AI == AIRanged && m.Range <= 0 {
			return fmt.Errorf("delve content: monster %q is AIRanged but Range=%d", m.ID, m.Range)
		}
	}
	for k := AIWander; k <= AIGuard; k++ {
		if !seenAI[k] {
			return fmt.Errorf("delve content: no monster uses %s", aiKindName(k))
		}
	}
	return nil
}

// validateItems lints the item table: unique IDs, canonical kind
// glyphs, known ItemKinds and TargetKinds, bounded numbers, UseText on
// consumables, and coverage of every ItemKind.
func validateItems() error {
	ids := make(map[string]bool, len(itemDefs))
	seenKind := make(map[ItemKind]bool)
	for i := range itemDefs {
		d := &itemDefs[i]
		if d.ID == "" {
			return fmt.Errorf("delve content: itemDefs[%d] has an empty ID", i)
		}
		if ids[d.ID] {
			return fmt.Errorf("delve content: duplicate item ID %q", d.ID)
		}
		ids[d.ID] = true
		want, ok := kindGlyph(d.Kind)
		if !ok {
			return fmt.Errorf("delve content: item %q has unknown ItemKind %d", d.ID, int(d.Kind))
		}
		seenKind[d.Kind] = true
		if d.Name == "" {
			return fmt.Errorf("delve content: item %q has an empty name", d.ID)
		}
		if d.Glyph != want {
			return fmt.Errorf("delve content: item %q glyph %q does not match %s glyph %q",
				d.ID, d.Glyph, itemKindName(d.Kind), want)
		}
		if d.Power < 0 || d.Power > maxContentPower {
			return fmt.Errorf("delve content: item %q power %d outside 0..%d", d.ID, d.Power, maxContentPower)
		}
		if d.Weight < 0 || d.Weight > maxContentWeight {
			return fmt.Errorf("delve content: item %q weight %d outside 0..%d", d.ID, d.Weight, maxContentWeight)
		}
		if d.MinDepth < 1 || d.MinDepth > MaxDepth {
			return fmt.Errorf("delve content: item %q MinDepth %d outside 1..%d", d.ID, d.MinDepth, MaxDepth)
		}
		if !validTargetKind(d.TargetKind) {
			return fmt.Errorf("delve content: item %q has unknown TargetKind %q", d.ID, d.TargetKind)
		}
		if d.TargetKind != TargetSelf && d.Kind != ItemPotion && d.Kind != ItemScroll {
			return fmt.Errorf("delve content: item %q has TargetKind %q but %s cannot target",
				d.ID, d.TargetKind, itemKindName(d.Kind))
		}
		if consumable(d.Kind) && d.UseText == "" {
			return fmt.Errorf("delve content: consumable %q has no UseText", d.ID)
		}
	}
	for k := ItemWeapon; k <= ItemArtifact; k++ {
		if !seenKind[k] {
			return fmt.Errorf("delve content: no item of kind %s", itemKindName(k))
		}
	}
	return nil
}

// validateFixtures checks the scripted endgame pair: the Gilded
// Matriarch and the Brass Idol must exist, sit at MaxDepth, and carry
// Weight 0 so level generation places them deliberately instead of the
// spawn tables sprinkling them around.
func validateFixtures() error {
	boss := lookupMonster(BossID)
	if boss == nil {
		return fmt.Errorf("delve content: boss %q missing from monsterDefs", BossID)
	}
	if boss.MinDepth != MaxDepth || boss.Weight != 0 {
		return fmt.Errorf("delve content: boss %q must have MinDepth=%d and Weight=0 (got %d, %d)",
			BossID, MaxDepth, boss.MinDepth, boss.Weight)
	}
	if boss.AI != AIGuard {
		return fmt.Errorf("delve content: boss %q must be AIGuard, got %s", BossID, aiKindName(boss.AI))
	}
	art := lookupItem(ArtifactID)
	if art == nil {
		return fmt.Errorf("delve content: artifact %q missing from itemDefs", ArtifactID)
	}
	if art.MinDepth != MaxDepth || art.Weight != 0 {
		return fmt.Errorf("delve content: artifact %q must have MinDepth=%d and Weight=0 (got %d, %d)",
			ArtifactID, MaxDepth, art.MinDepth, art.Weight)
	}
	if art.Kind != ItemArtifact {
		return fmt.Errorf("delve content: artifact %q must be ItemArtifact, got %s",
			ArtifactID, itemKindName(art.Kind))
	}
	return nil
}

// anyMonsterAt reports whether anything can spawn at depth.
func anyMonsterAt(depth int) bool {
	for i := range monsterDefs {
		if monsterWeight(&monsterDefs[i], depth) > 0 {
			return true
		}
	}
	return false
}

// anyItemAt reports whether anything can spawn at depth.
func anyItemAt(depth int) bool {
	for i := range itemDefs {
		if itemWeight(&itemDefs[i], depth) > 0 {
			return true
		}
	}
	return false
}
