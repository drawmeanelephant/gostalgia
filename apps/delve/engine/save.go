package engine

// save.go is the whole persistence story: a versioned JSON DTO that
// carries everything Marshal needs — run state plus the PCG's binary
// snapshot — so a resumed game replays the exact same rng stream.

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
)

// saveVersion stamps the DTO; Unmarshal refuses anything else.
const saveVersion = 1

// saveDTO is the serialized form of a run. PCG is the generator's
// MarshalBinary output, keeping resumed runs bit-deterministic.
type saveDTO struct {
	Version    int      `json:"version"`
	Config     Config   `json:"config"`
	Player     Player   `json:"player"`
	Depth      int      `json:"depth"`
	Levels     []*Level `json:"levels"`
	Log        []string `json:"log"`
	Out        Outcome  `json:"outcome"`
	DeathCause string   `json:"death_cause,omitempty"`
	NextID     int      `json:"next_id"`
	Rested     bool     `json:"rested,omitempty"`
	PCG        []byte   `json:"pcg"`
}

// marshalDTO encodes the DTO; a thin wrapper so engine.go's Marshal
// stays readable.
func marshalDTO(d saveDTO) ([]byte, error) { return json.Marshal(d) }

// unmarshalDTO decodes and sanity-checks a save, then rebuilds the
// runtime caches (rng from the PCG snapshot, fresh FOV scratch).
func unmarshalDTO(data []byte) (*Game, error) {
	var d saveDTO
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("delve engine: unreadable save: %w", err)
	}
	if d.Version != saveVersion {
		return nil, fmt.Errorf("delve engine: save version %d, expected %d", d.Version, saveVersion)
	}
	if d.Depth < 1 || d.Depth > MaxDepth || len(d.Levels) != MaxDepth {
		return nil, errors.New("delve engine: save has corrupt dungeon state")
	}
	if d.Levels[d.Depth-1] == nil {
		return nil, errors.New("delve engine: save is missing the active level")
	}
	for _, l := range d.Levels {
		if l == nil {
			continue
		}
		if l.Width <= 0 || l.Height <= 0 ||
			len(l.Tiles) != l.Width*l.Height || len(l.Seen) != l.Width*l.Height {
			return nil, errors.New("delve engine: save has a corrupt level")
		}
	}
	pcg := new(rand.PCG)
	if err := pcg.UnmarshalBinary(d.PCG); err != nil {
		return nil, fmt.Errorf("delve engine: corrupt rng state: %w", err)
	}
	g := &Game{
		Player:     d.Player,
		Depth:      d.Depth,
		Levels:     d.Levels,
		Log:        d.Log,
		Out:        d.Out,
		DeathCause: d.DeathCause,
		cfg:        d.Config,
		pcg:        pcg,
		rng:        rand.New(pcg),
		nextID:     max(d.NextID, 1),
		lastRest:   d.Rested,
	}
	g.computeFOV()
	return g, nil
}
