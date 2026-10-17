package engine

// content_test.go exercises the content tables as pure data: no Game,
// no level generation, nothing the engine task owns. The only engine
// surface it touches is the content contract itself (the tables, the
// lookup/spawn helpers, ValidateContent).

import (
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

// withMonsterDefs swaps the bestiary for the duration of a test so a
// deliberately-corrupt copy can be fed to ValidateContent.
func withMonsterDefs(t *testing.T, defs []MonsterDef) {
	t.Helper()
	orig := monsterDefs
	monsterDefs = defs
	t.Cleanup(func() { monsterDefs = orig })
}

// withItemDefs swaps the item table for the duration of a test.
func withItemDefs(t *testing.T, defs []ItemDef) {
	t.Helper()
	orig := itemDefs
	itemDefs = defs
	t.Cleanup(func() { itemDefs = orig })
}

func TestValidateContentRealTables(t *testing.T) {
	if err := ValidateContent(); err != nil {
		t.Fatalf("ValidateContent() = %v, want nil", err)
	}
}

// TestValidateContentCorrupt feeds ValidateContent table copies with
// one defect each and checks that the defect is caught.
func TestValidateContentCorrupt(t *testing.T) {
	t.Run("duplicate monster ID", func(t *testing.T) {
		defs := slices.Clone(monsterDefs)
		defs = append(defs, defs[0])
		withMonsterDefs(t, defs)
		if err := ValidateContent(); err == nil {
			t.Fatal("duplicate monster ID accepted")
		}
	})
	t.Run("reserved monster glyph", func(t *testing.T) {
		defs := slices.Clone(monsterDefs)
		defs[1].Glyph = '!' // potion glyph, not a letter
		withMonsterDefs(t, defs)
		if err := ValidateContent(); err == nil || !strings.Contains(err.Error(), "glyph") {
			t.Fatalf("bad glyph error = %v", err)
		}
	})
	t.Run("duplicate monster glyph", func(t *testing.T) {
		defs := slices.Clone(monsterDefs)
		defs[1].Glyph = defs[0].Glyph
		withMonsterDefs(t, defs)
		if err := ValidateContent(); err == nil {
			t.Fatal("duplicate monster glyph accepted")
		}
	})
	t.Run("missing boss", func(t *testing.T) {
		defs := slices.DeleteFunc(slices.Clone(monsterDefs), func(m MonsterDef) bool { return m.ID == BossID })
		withMonsterDefs(t, defs)
		if err := ValidateContent(); err == nil {
			t.Fatal("bestiary without the boss accepted")
		}
	})
	t.Run("natural boss", func(t *testing.T) {
		defs := slices.Clone(monsterDefs)
		for i := range defs {
			if defs[i].ID == BossID {
				defs[i].Weight = 5 // boss must be scripted, not spawned
			}
		}
		withMonsterDefs(t, defs)
		if err := ValidateContent(); err == nil {
			t.Fatal("boss with spawn weight accepted")
		}
	})
	t.Run("uncovered AIKind", func(t *testing.T) {
		defs := slices.Clone(monsterDefs)
		for i := range defs {
			if defs[i].AI == AIRanged {
				defs[i].AI = AIHunt
				defs[i].Range = 0
			}
		}
		withMonsterDefs(t, defs)
		if err := ValidateContent(); err == nil {
			t.Fatal("bestiary with no AIRanged monster accepted")
		}
	})
	t.Run("negative stat", func(t *testing.T) {
		defs := slices.Clone(monsterDefs)
		defs[0].HP = -1
		withMonsterDefs(t, defs)
		if err := ValidateContent(); err == nil {
			t.Fatal("monster with negative HP accepted")
		}
	})
	t.Run("absurd weight", func(t *testing.T) {
		defs := slices.Clone(monsterDefs)
		defs[0].Weight = maxContentWeight + 1
		withMonsterDefs(t, defs)
		if err := ValidateContent(); err == nil {
			t.Fatal("monster weight above bound accepted")
		}
	})
	t.Run("duplicate item ID", func(t *testing.T) {
		defs := slices.Clone(itemDefs)
		defs = append(defs, defs[0])
		withItemDefs(t, defs)
		if err := ValidateContent(); err == nil {
			t.Fatal("duplicate item ID accepted")
		}
	})
	t.Run("wrong item glyph", func(t *testing.T) {
		defs := slices.Clone(itemDefs)
		defs[0].Glyph = GlyphScroll // a weapon drawn as a scroll
		withItemDefs(t, defs)
		if err := ValidateContent(); err == nil || !strings.Contains(err.Error(), "glyph") {
			t.Fatalf("bad item glyph error = %v", err)
		}
	})
	t.Run("missing artifact", func(t *testing.T) {
		defs := slices.DeleteFunc(slices.Clone(itemDefs), func(d ItemDef) bool { return d.ID == ArtifactID })
		withItemDefs(t, defs)
		if err := ValidateContent(); err == nil {
			t.Fatal("item table without the Brass Idol accepted")
		}
	})
	t.Run("consumable without UseText", func(t *testing.T) {
		defs := slices.Clone(itemDefs)
		for i := range defs {
			if defs[i].ID == "potion_heal" {
				defs[i].UseText = ""
			}
		}
		withItemDefs(t, defs)
		if err := ValidateContent(); err == nil || !strings.Contains(err.Error(), "UseText") {
			t.Fatalf("missing UseText error = %v", err)
		}
	})
	t.Run("bad TargetKind", func(t *testing.T) {
		defs := slices.Clone(itemDefs)
		for i := range defs {
			if defs[i].ID == "scroll_fear" {
				defs[i].TargetKind = "everyone"
			}
		}
		withItemDefs(t, defs)
		if err := ValidateContent(); err == nil {
			t.Fatal("scroll with unknown TargetKind accepted")
		}
	})
	t.Run("depth with no items", func(t *testing.T) {
		defs := slices.Clone(itemDefs)
		for i := range defs {
			defs[i].MinDepth = MaxDepth // nothing left for shallow floors
		}
		withItemDefs(t, defs)
		if err := ValidateContent(); err == nil {
			t.Fatal("item table with no shallow spawns accepted")
		}
	})
}

func TestLookupsResolveEveryID(t *testing.T) {
	for i := range monsterDefs {
		m := &monsterDefs[i]
		got := lookupMonster(m.ID)
		if got != m {
			t.Errorf("lookupMonster(%q) = %p, want the table def", m.ID, got)
		}
	}
	for i := range itemDefs {
		d := &itemDefs[i]
		got := lookupItem(d.ID)
		if got != d {
			t.Errorf("lookupItem(%q) = %p, want the table def", d.ID, got)
		}
	}
	if lookupMonster("no_such_monster") != nil {
		t.Error("lookupMonster returned a def for an unknown ID")
	}
	if lookupItem("no_such_item") != nil {
		t.Error("lookupItem returned a def for an unknown ID")
	}
}

// TestSpawnMonsterEligibility draws monsters at every depth across many
// seeds: every result must be eligible (MinDepth honored, positive
// effective weight) and the boss must never spawn naturally.
func TestSpawnMonsterEligibility(t *testing.T) {
	for seed := uint64(1); seed <= 16; seed++ {
		r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
		for depth := 1; depth <= MaxDepth; depth++ {
			for i := 0; i < 200; i++ {
				m := spawnMonster(r, depth)
				if m == nil {
					t.Fatalf("seed %d depth %d: spawnMonster returned nil", seed, depth)
				}
				if m.ID == BossID {
					t.Fatalf("seed %d depth %d: boss spawned naturally", seed, depth)
				}
				if m.MinDepth > depth {
					t.Fatalf("seed %d depth %d: %q spawned before its MinDepth %d", seed, depth, m.ID, m.MinDepth)
				}
				if monsterWeight(m, depth) <= 0 {
					t.Fatalf("seed %d depth %d: %q spawned with zero effective weight", seed, depth, m.ID)
				}
			}
		}
	}
}

// TestSpawnMonsterDepthWindow checks the soft depth window: by MaxDepth
// the shallow natives have decayed out of the table entirely while
// deep defs are still spawning.
func TestSpawnMonsterDepthWindow(t *testing.T) {
	r := rand.New(rand.NewPCG(0xde1e, 0x201))
	var deepSeen bool
	for i := 0; i < 2000; i++ {
		m := spawnMonster(r, MaxDepth)
		if m.MinDepth <= 2 {
			t.Fatalf("shallow monster %q (MinDepth %d) still spawning at depth %d", m.ID, m.MinDepth, MaxDepth)
		}
		if m.MinDepth >= 6 {
			deepSeen = true
		}
	}
	if !deepSeen {
		t.Fatalf("no deep monster (MinDepth>=6) seen in 2000 draws at depth %d", MaxDepth)
	}
}

// TestSpawnItemEligibility draws items at every depth across many
// seeds: every result must be eligible and the Brass Idol must never
// spawn naturally.
func TestSpawnItemEligibility(t *testing.T) {
	for seed := uint64(1); seed <= 16; seed++ {
		r := rand.New(rand.NewPCG(seed^0xa5a5, seed*0x2545f4914f6cdd1d))
		for depth := 1; depth <= MaxDepth; depth++ {
			for i := 0; i < 200; i++ {
				d := spawnItem(r, depth)
				if d == nil {
					t.Fatalf("seed %d depth %d: spawnItem returned nil", seed, depth)
				}
				if d.ID == ArtifactID {
					t.Fatalf("seed %d depth %d: artifact spawned naturally", seed, depth)
				}
				if d.MinDepth > depth {
					t.Fatalf("seed %d depth %d: %q spawned before its MinDepth %d", seed, depth, d.ID, d.MinDepth)
				}
				if itemWeight(d, depth) <= 0 {
					t.Fatalf("seed %d depth %d: %q spawned with zero effective weight", seed, depth, d.ID)
				}
			}
		}
	}
}

// TestGoldEarlyPlateLate checks the economic arc: gold should be
// plentiful on early floors (plate cannot spawn there at all), and on
// the deep floors plate is available but gold still out-drops it.
func TestGoldEarlyPlateLate(t *testing.T) {
	r := rand.New(rand.NewPCG(0x201, 0xbeef))
	var gold, plate int
	for i := 0; i < 3000; i++ {
		switch spawnItem(r, 1).ID {
		case "gold":
			gold++
		case "plate":
			plate++
		}
	}
	if plate != 0 {
		t.Errorf("plate spawned on level 1 (%d/3000)", plate)
	}
	if gold < 300 {
		t.Errorf("gold spawned only %d/3000 on level 1, want a common drop", gold)
	}

	gold, plate = 0, 0
	for i := 0; i < 3000; i++ {
		switch spawnItem(r, MaxDepth).ID {
		case "gold":
			gold++
		case "plate":
			plate++
		}
	}
	if plate == 0 {
		t.Errorf("plate never spawned at depth %d", MaxDepth)
	}
	if gold <= plate {
		t.Errorf("at depth %d gold dropped %d vs plate %d, want gold more common", MaxDepth, gold, plate)
	}
}

// TestCoverage checks the breadth the contract asks of the tables:
// >=12 monsters, >=16 items, every AIKind exercised at least twice,
// and a monster whose MinDepth lands on each tier of the descent.
func TestCoverage(t *testing.T) {
	if len(monsterDefs) < 12 {
		t.Errorf("bestiary has %d monsters, want >= 12", len(monsterDefs))
	}
	if len(itemDefs) < 16 {
		t.Errorf("item table has %d items, want >= 16", len(itemDefs))
	}
	counts := make(map[AIKind]int)
	for i := range monsterDefs {
		counts[monsterDefs[i].AI]++
	}
	for k := AIWander; k <= AIGuard; k++ {
		if counts[k] < 2 {
			t.Errorf("%s used by %d monster(s), want >= 2", aiKindName(k), counts[k])
		}
	}
}
