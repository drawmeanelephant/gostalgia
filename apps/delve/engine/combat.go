package engine

// combat.go resolves attacks in both directions. The model is small:
// damage is attack + weapon power - defense, wobbled by ±1, floored at
// 1, with a flat 10% whiff chance so armor and luck both matter.

import "fmt"

// attackStat is the player's effective attack: a level-scaled base plus
// the wielded weapon's power and per-instance bonus.
func (g *Game) attackStat() int {
	a := 2 + g.Player.Level
	if it := g.invItem(g.Player.WeaponID); it != nil {
		if def := lookupItem(it.Def); def != nil {
			a += def.Power + it.Bonus
		}
	}
	return a
}

// defenseStat is the player's effective soak: worn armor only.
func (g *Game) defenseStat() int {
	d := 0
	if it := g.invItem(g.Player.ArmorID); it != nil {
		if def := lookupItem(it.Def); def != nil {
			d += def.Power + it.Bonus
		}
	}
	return d
}

// playerAttack resolves one bump attack against m.
func (g *Game) playerAttack(l *Level, m *Monster) []Event {
	def := lookupMonster(m.Def)
	name := "the creature"
	defense := 0
	if def != nil {
		name = "the " + def.Name
		defense = def.Defense
	}
	if g.rng.IntN(10) == 0 {
		return []Event{{Kind: EvMiss, Text: "You miss " + name + "."}}
	}
	dmg := g.attackStat() + g.rng.IntN(3) - 1 - defense
	if dmg < 1 {
		dmg = 1
	}
	m.HP -= dmg
	evs := []Event{{Kind: EvHit, Text: fmt.Sprintf("You hit %s for %d.", name, dmg)}}
	if m.HP <= 0 {
		evs = append(evs, g.killMonster(l, m)...)
	}
	return evs
}

// monsterAttack resolves one attack on the player, melee or ranged.
func (g *Game) monsterAttack(m *Monster, def *MonsterDef, ranged bool) []Event {
	if g.rng.IntN(10) == 0 {
		return []Event{{Kind: EvMiss, Text: fmt.Sprintf("The %s misses you.", def.Name)}}
	}
	dmg := def.Attack + g.rng.IntN(3) - 1 - g.defenseStat()
	if dmg < 1 {
		dmg = 1
	}
	g.Player.HP -= dmg
	verb := "hits"
	if ranged {
		verb = "shoots"
	}
	evs := []Event{{Kind: EvHit, Text: fmt.Sprintf("The %s %s you for %d.", def.Name, verb, dmg)}}
	if g.Player.HP <= 0 {
		evs = append(evs, g.die(refer(def.Name))...)
	}
	return evs
}

// killMonster removes m from its floor and pays out XP (with any level
// gains that follow).
func (g *Game) killMonster(l *Level, m *Monster) []Event {
	def := lookupMonster(m.Def)
	name := "the creature"
	xp := 0
	if def != nil {
		name = "the " + def.Name
		xp = def.XP
	}
	for i := range l.Monsters {
		if &l.Monsters[i] == m {
			l.Monsters = append(l.Monsters[:i], l.Monsters[i+1:]...)
			break
		}
	}
	g.Player.Kills++
	evs := []Event{{Kind: EvDeath, Text: title(name) + " dies."}}
	return append(evs, g.awardXP(xp)...)
}

// awardXP adds XP and applies any level-ups it crosses: +4 MaxHP and
// +1 effective attack (via the level-scaled base) each time.
func (g *Game) awardXP(n int) []Event {
	g.Player.XP += n
	var evs []Event
	for g.Player.XP >= xpForNext(g.Player.Level) {
		g.Player.Level++
		g.Player.MaxHP += 4
		g.Player.HP += 4
		if g.Player.HP > g.Player.MaxHP {
			g.Player.HP = g.Player.MaxHP
		}
		evs = append(evs, Event{
			Kind: EvLevelUp,
			Text: fmt.Sprintf("You feel stronger — level %d!", g.Player.Level),
		})
	}
	return evs
}
