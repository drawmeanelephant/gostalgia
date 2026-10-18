package engine

// items.go handles the pack: pickup (gold absorbs, the rest need pack
// room), drop, equip toggles, and use — potions, scrolls, and food,
// with scroll/potion targeting driven by ItemDef.TargetKind.

import "fmt"

// cmdPickup takes the topmost item under the player.
func (g *Game) cmdPickup() ([]Event, bool) {
	l := g.level(g.Depth)
	it := l.itemAt(g.Player.Pos)
	if it == nil {
		return msg("There is nothing here to pick up."), false
	}
	def := lookupItem(it.Def)
	if def == nil {
		return msg("It's stuck fast."), false
	}
	item := *it
	if def.Kind == ItemGold {
		amt := item.Bonus
		if amt <= 0 {
			amt = 5 + g.rng.IntN(10)
		}
		g.Player.Gold += amt
		g.removeGroundItem(l, item.ID)
		return []Event{{Kind: EvPickup, Text: fmt.Sprintf("You scoop up %d gold.", amt)}}, true
	}
	if len(g.Player.Inventory) >= InventoryCap {
		return msg("Your pack is full."), false
	}
	g.removeGroundItem(l, item.ID)
	item.Pos = Pos{-1, -1}
	g.Player.Inventory = append(g.Player.Inventory, item)
	return []Event{{Kind: EvPickup, Text: "You pick up " + refer(def.Name) + "."}}, true
}

// removeGroundItem deletes the ground item with the given id.
func (g *Game) removeGroundItem(l *Level, id int) {
	for i := range l.Items {
		if l.Items[i].ID == id {
			l.Items = append(l.Items[:i], l.Items[i+1:]...)
			return
		}
	}
}

// cmdDrop places an inventory item on the floor under the player,
// unequipping it first if needed.
func (g *Game) cmdDrop(id int) ([]Event, bool) {
	i := g.invIndex(id)
	if i < 0 {
		return msg("You aren't carrying that."), false
	}
	it := g.Player.Inventory[i]
	if it.ID == g.Player.WeaponID {
		g.Player.WeaponID = 0
	}
	if it.ID == g.Player.ArmorID {
		g.Player.ArmorID = 0
	}
	g.Player.Inventory = append(g.Player.Inventory[:i], g.Player.Inventory[i+1:]...)
	it.Pos = g.Player.Pos
	l := g.level(g.Depth)
	l.Items = append(l.Items, it)
	name := it.Def
	if def := lookupItem(it.Def); def != nil {
		name = def.Name
	}
	return []Event{{Kind: EvDrop, Text: "You drop " + refer(name) + "."}}, true
}

// cmdEquip toggles a weapon or armor item by id.
func (g *Game) cmdEquip(id int) ([]Event, bool) {
	it := g.invItem(id)
	if it == nil {
		return msg("You aren't carrying that."), false
	}
	def := lookupItem(it.Def)
	if def == nil {
		return msg("You can't equip that."), false
	}
	switch def.Kind {
	case ItemWeapon:
		if g.Player.WeaponID == id {
			g.Player.WeaponID = 0
			return msg("You put away the " + def.Name + "."), true
		}
		g.Player.WeaponID = id
		return msg("You wield the " + def.Name + "."), true
	case ItemArmor:
		if g.Player.ArmorID == id {
			g.Player.ArmorID = 0
			return msg("You remove the " + def.Name + "."), true
		}
		g.Player.ArmorID = id
		return msg("You don the " + def.Name + "."), true
	}
	return msg("You can't equip that."), false
}

// cmdUseItem activates a potion, scroll, or food by inventory id.
// TargetKind picks the effect's scope: self, the nearest visible
// monster, or every visible monster.
func (g *Game) cmdUseItem(id int) ([]Event, bool) {
	i := g.invIndex(id)
	if i < 0 {
		return msg("You aren't carrying that."), false
	}
	it := g.Player.Inventory[i]
	def := lookupItem(it.Def)
	if def == nil {
		return msg("Nothing happens."), false
	}
	switch def.Kind {
	case ItemFood:
		g.Player.Satiety += def.Power
		if g.Player.Satiety > SatietyMax {
			g.Player.Satiety = SatietyMax
		}
		g.consumeItem(i)
		return useMsg(def), true
	case ItemPotion, ItemScroll:
		return g.cmdUseTargeted(i, it, def)
	}
	return msg("You can't use that."), false
}

// cmdUseTargeted resolves a potion or scroll against its TargetKind.
func (g *Game) cmdUseTargeted(i int, it Item, def *ItemDef) ([]Event, bool) {
	l := g.level(g.Depth)
	switch def.TargetKind {
	case TargetNearest:
		m := g.nearestVisibleMonster(l)
		if m == nil {
			return msg("No target in sight."), false
		}
		g.consumeItem(i)
		evs := useMsg(def)
		return append(evs, g.zapMonster(l, m, def)...), true
	case TargetAllVisible:
		ids := g.visibleMonsterIDs(l)
		if len(ids) == 0 {
			return msg("No target in sight."), false
		}
		g.consumeItem(i)
		evs := useMsg(def)
		for _, id := range ids {
			if m := l.monsterByID(id); m != nil {
				evs = append(evs, g.zapMonster(l, m, def)...)
			}
		}
		return evs, true
	default: // TargetSelf
		g.consumeItem(i)
		evs := useMsg(def)
		if def.Kind == ItemPotion {
			g.Player.HP += def.Power
			if g.Player.HP > g.Player.MaxHP {
				g.Player.HP = g.Player.MaxHP
			}
		} else {
			// A self-targeted scroll is a word of awareness: it maps
			// the floor.
			for j := range l.Seen {
				l.Seen[j] = true
			}
		}
		return evs, true
	}
}

// zapMonster applies a targeted usable's power as damage to m.
func (g *Game) zapMonster(l *Level, m *Monster, def *ItemDef) []Event {
	dmg := def.Power
	if dmg < 1 {
		dmg = 1
	}
	m.HP -= dmg
	name := "the creature"
	if md := lookupMonster(m.Def); md != nil {
		name = "the " + md.Name
	}
	evs := []Event{{Kind: EvHit, Text: fmt.Sprintf("The %s strikes %s for %d.", def.Name, name, dmg)}}
	if m.HP <= 0 {
		evs = append(evs, g.killMonster(l, m)...)
	}
	return evs
}

// consumeItem removes inventory slot i (used items are spent).
func (g *Game) consumeItem(i int) {
	g.Player.Inventory = append(g.Player.Inventory[:i], g.Player.Inventory[i+1:]...)
}

// useMsg is the EvUse line for a consumed item.
func useMsg(def *ItemDef) []Event {
	t := def.UseText
	if t == "" {
		t = "You use the " + def.Name + "."
	}
	return []Event{{Kind: EvUse, Text: t}}
}

// nearestVisibleMonster returns the live-FOV monster closest to the
// player, or nil.
func (g *Game) nearestVisibleMonster(l *Level) *Monster {
	var best *Monster
	bd := 1 << 30
	for i := range l.Monsters {
		m := &l.Monsters[i]
		p := m.Pos
		if !l.InBounds(p) || !g.visible[p.Y*l.Width+p.X] {
			continue
		}
		if d := cheb(p, g.Player.Pos); d < bd {
			bd = d
			best = m
		}
	}
	return best
}

// visibleMonsterIDs lists ids of monsters inside the live FOV (id
// snapshot, since the monster slice can shrink mid-effect).
func (g *Game) visibleMonsterIDs(l *Level) []int {
	var ids []int
	for i := range l.Monsters {
		p := l.Monsters[i].Pos
		if l.InBounds(p) && p.Y*l.Width+p.X < len(g.visible) && g.visible[p.Y*l.Width+p.X] {
			ids = append(ids, l.Monsters[i].ID)
		}
	}
	return ids
}
