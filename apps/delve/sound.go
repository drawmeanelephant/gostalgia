// sound.go maps engine event kinds to sound/play blips: hits beep,
// deaths bark (there are dogs in this dungeon), level-ups and the win
// arpeggiate upward, the lose stinger descends, pickups and uses get a
// soft blip. Everything is fire-and-forget — the grant is probed once
// per launch and every call error is swallowed, so a silent host never
// turns sound into a delve error.
package delve

import (
	"context"

	"gostalgia/apps/delve/engine"
	"gostalgia/sdk"
)

// soundCapability, soundRoute, and soundStatusRoute name the audio grant
// and the routes served by the host audio adapter.
const (
	soundCapability  = sdk.CapSound
	soundRoute       = "sound/play"
	soundStatusRoute = "sound/status"
)

// Tri-state for the sound probe: unknown until checked once, then latched.
const (
	soundUnknown = iota
	soundOff
	soundOn
)

// clipNote is one entry of a sound/play notes payload: frequency_hz 0 is
// a rest and wave is required on every note.
type clipNote struct {
	FrequencyHz float64 `json:"frequency_hz"`
	DurationMs  int     `json:"duration_ms"`
	Wave        string  `json:"wave"`
}

// soundOrder ranks event kinds so a busy turn fires its most important
// blips first within the in-flight player bound.
var soundOrder = []engine.EventKind{
	engine.EvWin, engine.EvLose, engine.EvDeath, engine.EvLevelUp,
	engine.EvHit, engine.EvPickup, engine.EvUse,
}

// soundParams renders one event kind as a sound/play params payload; nil
// means the kind stays silent (ambient messages, misses, hunger, stairs).
func soundParams(k engine.EventKind) map[string]any {
	notes := func(freqs []float64, ms int, wave string) map[string]any {
		out := make([]clipNote, len(freqs))
		for i, f := range freqs {
			out[i] = clipNote{FrequencyHz: f, DurationMs: ms, Wave: wave}
		}
		return map[string]any{"notes": out}
	}
	switch k {
	case engine.EvHit:
		return map[string]any{"sound": "beep"}
	case engine.EvDeath:
		return map[string]any{"sound": "bark"}
	case engine.EvLevelUp:
		return notes([]float64{523.25, 659.25, 783.99}, 90, "square")
	case engine.EvWin:
		return notes([]float64{523.25, 659.25, 783.99, 1046.5}, 110, "square")
	case engine.EvLose:
		return notes([]float64{220, 174.61, 146.83}, 180, "triangle")
	case engine.EvPickup, engine.EvUse:
		return notes([]float64{659.25}, 60, "sine")
	}
	return nil
}

// playSounds emits blips for a command's events, one per mapped kind in
// priority order, bounded so a long rest cannot saturate the player.
// Every failure is swallowed: no grant, an unsupported host, or a busy
// adapter must never surface as a delve error.
func (d *Delve) playSounds(ctx context.Context, events []engine.Event) {
	if len(events) == 0 || !d.soundReady(ctx) {
		return
	}
	seen := map[engine.EventKind]bool{}
	for _, ev := range events {
		seen[ev.Kind] = true
	}
	fired := 0
	for _, kind := range soundOrder {
		if !seen[kind] || fired >= 3 {
			continue
		}
		if params := soundParams(kind); params != nil {
			_ = d.app.Call(ctx, soundRoute, params, nil)
			fired++
		}
	}
}

// soundReady resolves the audio story once per launch — does this
// instance's grant carry the sound capability, and does the host audio
// adapter report itself supported? The result latches; probe failures
// latch off rather than spending doomed sound/play calls.
func (d *Delve) soundReady(ctx context.Context) bool {
	d.mu.Lock()
	sound := d.sound
	d.mu.Unlock()
	if sound == soundUnknown {
		sound = soundOff
		var who struct {
			Capabilities []string `json:"capabilities"`
		}
		if err := d.app.Call(ctx, "session/whoami", nil, &who); err == nil {
			for _, cap := range who.Capabilities {
				if cap == soundCapability {
					sound = soundOn
				}
			}
		}
		if sound == soundOn {
			var st struct {
				Supported bool `json:"supported"`
			}
			if err := d.app.Call(ctx, soundStatusRoute, nil, &st); err != nil || !st.Supported {
				sound = soundOff
			}
		}
		d.mu.Lock()
		d.sound = sound
		d.mu.Unlock()
	}
	return sound == soundOn
}
