package services

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"gostalgia/internal/ipc"
	"gostalgia/internal/security"
	"gostalgia/internal/service"
	"gostalgia/platform"
)

// Synthesis bounds. The route accepts only a bounded, declarative tone
// sequence — never file paths, host commands, or unbounded audio — and the
// runtime renders PCM in-process at a fixed sample rate.
const (
	soundSampleRate     = 22050 // Hz, 16-bit mono PCM
	maxSoundNotes       = 64
	minSoundNoteMs      = 10
	maxSoundNoteMs      = 2000
	maxSoundDurationMs  = 10000
	minSoundFrequencyHz = 20.0
	maxSoundFrequencyHz = 8000.0
	maxSoundName        = 24
)

// soundAmplitude is deliberately conservative: square and sawtooth waves
// carry harmonics that clip harshly at full scale.
const soundAmplitude = 0.3

// soundEnvelopeMs is the per-note linear attack/release, avoiding the
// audible click of an instantaneous edge.
const soundEnvelopeMs = 5

var soundNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// soundWaveShapes enumerates the oscillator shapes callers may request.
var soundWaveShapes = map[string]bool{
	"sine":     true,
	"square":   true,
	"triangle": true,
	"sawtooth": true,
}

// soundNote is one declarative tone: frequency_hz 0 renders a rest.
type soundNote struct {
	FrequencyHz float64 `json:"frequency_hz"`
	DurationMs  int     `json:"duration_ms"`
	Wave        string  `json:"wave"`
}

// soundPresets are runtime-defined named clips so apps can request common
// effects without spelling out notes. They obey the same bounds as
// caller-supplied sequences by construction.
var soundPresets = map[string][]soundNote{
	"bark": {
		{FrequencyHz: 150, DurationMs: 80, Wave: "square"},
		{FrequencyHz: 0, DurationMs: 40, Wave: "sine"},
		{FrequencyHz: 110, DurationMs: 140, Wave: "square"},
	},
	"beep": {
		{FrequencyHz: 880, DurationMs: 150, Wave: "sine"},
	},
	"chime": {
		{FrequencyHz: 660, DurationMs: 140, Wave: "sine"},
		{FrequencyHz: 0, DurationMs: 50, Wave: "sine"},
		{FrequencyHz: 880, DurationMs: 160, Wave: "sine"},
		{FrequencyHz: 0, DurationMs: 50, Wave: "sine"},
		{FrequencyHz: 1100, DurationMs: 220, Wave: "sine"},
	},
}

// SoundService exposes capability-gated host audio playback. It owns the
// "sound/*" method namespace.
type SoundService struct {
	ctx *service.Context
}

// NewSound creates a new SoundService.
func NewSound() *SoundService {
	return &SoundService{}
}

func (s *SoundService) Name() string      { return "sound" }
func (s *SoundService) Depends() []string { return nil }

func (s *SoundService) Init(ctx *service.Context) error {
	s.ctx = ctx
	return nil
}

func (s *SoundService) Start(ctx context.Context) error {
	return s.ctx.Router.HandleRoutes(map[string]ipc.Route{
		"sound/play":   {Handler: s.play, Policy: ipc.Cap(security.CapSound)},
		"sound/status": {Handler: s.status, Policy: ipc.Baseline},
	})
}

func (s *SoundService) Stop(ctx context.Context) error {
	s.ctx.Router.UnhandlePrefix("sound/")
	return nil
}

type playSoundParams struct {
	Sound string      `json:"sound,omitempty"`
	Notes []soundNote `json:"notes,omitempty"`
}

func (s *SoundService) play(ctx context.Context, req ipc.Request) (any, error) {
	var p playSoundParams
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, fmt.Errorf("sound/play: invalid parameters: %w", err)
	}

	notes, err := resolveSoundNotes(p)
	if err != nil {
		return nil, err
	}

	player := platform.GetAudioPlayer()
	if !player.Available() {
		return nil, fmt.Errorf("sound/play: %w", platform.ErrAudioUnsupported)
	}

	pcm := synthesizePCM(notes)
	wav := encodeWAV(pcm)
	if err := player.PlayWAV(wav); err != nil {
		return nil, fmt.Errorf("sound/play: %w", err)
	}

	return map[string]any{
		"played":      true,
		"player":      player.Name(),
		"notes":       len(notes),
		"duration_ms": soundDurationMs(notes),
		"bytes":       len(wav),
	}, nil
}

func (s *SoundService) status(ctx context.Context, req ipc.Request) (any, error) {
	player := platform.GetAudioPlayer()
	presets := make([]string, 0, len(soundPresets))
	for name := range soundPresets {
		presets = append(presets, name)
	}
	sort.Strings(presets)
	return map[string]any{
		"supported":       player.Available(),
		"player":          player.Name(),
		"sample_rate":     soundSampleRate,
		"max_notes":       maxSoundNotes,
		"max_duration_ms": maxSoundDurationMs,
		"presets":         presets,
	}, nil
}

// resolveSoundNotes validates the request and resolves it to a concrete,
// bounded note sequence: either a named preset or an explicit list.
func resolveSoundNotes(p playSoundParams) ([]soundNote, error) {
	p.Sound = strings.TrimSpace(p.Sound)
	switch {
	case p.Sound != "" && len(p.Notes) > 0:
		return nil, fmt.Errorf("sound/play: pass either a named sound or notes, not both")
	case p.Sound == "" && len(p.Notes) == 0:
		return nil, fmt.Errorf("sound/play: either a named sound or a notes list is required")
	}

	if p.Sound != "" {
		if len(p.Sound) > maxSoundName || !soundNamePattern.MatchString(p.Sound) {
			return nil, fmt.Errorf("sound/play: invalid sound name %q", p.Sound)
		}
		notes, ok := soundPresets[p.Sound]
		if !ok {
			return nil, fmt.Errorf("sound/play: unknown sound %q", p.Sound)
		}
		return notes, nil
	}

	if len(p.Notes) > maxSoundNotes {
		return nil, fmt.Errorf("sound/play: %d notes exceeds the %d-note limit", len(p.Notes), maxSoundNotes)
	}
	total := 0
	for i, n := range p.Notes {
		if !soundWaveShapes[n.Wave] {
			return nil, fmt.Errorf("sound/play: note %d: wave must be sine, square, triangle, or sawtooth", i)
		}
		if n.DurationMs < minSoundNoteMs || n.DurationMs > maxSoundNoteMs {
			return nil, fmt.Errorf("sound/play: note %d: duration_ms must be %d-%d", i, minSoundNoteMs, maxSoundNoteMs)
		}
		if n.FrequencyHz != 0 && (n.FrequencyHz < minSoundFrequencyHz || n.FrequencyHz > maxSoundFrequencyHz) {
			return nil, fmt.Errorf("sound/play: note %d: frequency_hz must be 0 (rest) or %g-%g", i, minSoundFrequencyHz, maxSoundFrequencyHz)
		}
		total += n.DurationMs
	}
	if total > maxSoundDurationMs {
		return nil, fmt.Errorf("sound/play: total duration %dms exceeds the %dms limit", total, maxSoundDurationMs)
	}
	return p.Notes, nil
}

func soundDurationMs(notes []soundNote) int {
	total := 0
	for _, n := range notes {
		total += n.DurationMs
	}
	return total
}

// synthesizePCM renders the note sequence to 16-bit mono PCM samples.
// Note boundaries fall on cumulative sample positions, so per-note
// truncation cannot shrink the clip below its declared duration: the total
// is always floor(total_ms * sample_rate / 1000) samples.
func synthesizePCM(notes []soundNote) []int16 {
	total := soundDurationMs(notes)
	out := make([]int16, 0, total*soundSampleRate/1000)
	fadeSamples := soundEnvelopeMs * soundSampleRate / 1000
	elapsedMs := 0
	for _, n := range notes {
		start := elapsedMs * soundSampleRate / 1000
		elapsedMs += n.DurationMs
		count := elapsedMs*soundSampleRate/1000 - start
		if n.FrequencyHz == 0 {
			// A rest is true silence, not a DC offset from waveSample(0).
			out = append(out, make([]int16, count)...)
			continue
		}
		for i := 0; i < count; i++ {
			// Linear attack/release envelope kills the edge click.
			env := 1.0
			if i < fadeSamples {
				env = float64(i) / float64(fadeSamples)
			}
			if rem := count - i; rem < fadeSamples && float64(rem)/float64(fadeSamples) < env {
				env = float64(rem) / float64(fadeSamples)
			}
			phase := float64(i) * n.FrequencyHz / soundSampleRate
			out = append(out, int16(env*soundAmplitude*32767*waveSample(n.Wave, phase)))
		}
	}
	return out
}

// waveSample evaluates an oscillator shape at a phase measured in cycles.
func waveSample(wave string, phase float64) float64 {
	frac := phase - math.Floor(phase)
	switch wave {
	case "square":
		if frac < 0.5 {
			return 1
		}
		return -1
	case "triangle":
		return 4*math.Abs(frac-0.5) - 1
	case "sawtooth":
		return 2*frac - 1
	default: // sine
		return math.Sin(2 * math.Pi * frac)
	}
}

// encodeWAV wraps 16-bit mono PCM samples in a canonical RIFF/WAVE
// container: a 44-byte header every standard host player accepts.
func encodeWAV(samples []int16) []byte {
	dataLen := uint32(len(samples) * 2)
	var buf bytes.Buffer
	buf.Grow(44 + int(dataLen))
	buf.WriteString("RIFF")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(36)+dataLen)
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(16)) // fmt chunk size
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))  // PCM
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))  // mono
	_ = binary.Write(&buf, binary.LittleEndian, uint32(soundSampleRate))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(soundSampleRate*2)) // byte rate
	_ = binary.Write(&buf, binary.LittleEndian, uint16(2))                 // block align
	_ = binary.Write(&buf, binary.LittleEndian, uint16(16))                // bits per sample
	buf.WriteString("data")
	_ = binary.Write(&buf, binary.LittleEndian, dataLen)
	for _, s := range samples {
		_ = binary.Write(&buf, binary.LittleEndian, s)
	}
	return buf.Bytes()
}
