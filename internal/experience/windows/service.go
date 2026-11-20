// Package windows is the toolkit-neutral window service. It owns window
// identity, titles, bounds, focus, and bottom-to-top z-order, and it reports
// lifecycle transitions on the existing event bus. It imports no toolkit:
// bounds are abstract units each adapter defines, which the shell reads as
// terminal cells inside its body. Apps never see this package; the shell keeps
// each window's presentation state beside the service.
package windows

import (
	"errors"
	"fmt"

	"gostalgia/internal/events"
)

// ID is stable for the lifetime of one open window.
type ID string

// Bounds is a window's position and size in adapter-defined logical units.
type Bounds struct {
	X      int
	Y      int
	Width  int
	Height int
}

// Window is one open window's neutral metadata.
type Window struct {
	ID     ID
	Title  string
	Bounds Bounds
}

// Service owns identity, focus, bottom-to-top z-order, and lifecycle. It is
// called on an experience event loop and is not safe for concurrent mutation.
type Service struct {
	bus     *events.Bus
	source  string
	windows map[ID]Window
	order   []ID
	focused ID
}

// NewService uses the supplied existing event bus. A nil bus gives the
// service a private one, for callers that observe no window events.
func NewService(bus *events.Bus, source string) *Service {
	if bus == nil {
		bus = events.NewBus()
	}
	if source == "" {
		source = "experience/windows"
	}
	return &Service{bus: bus, source: source, windows: make(map[ID]Window)}
}

// Open validates and opens a window, raises it, and focuses it.
func (s *Service) Open(w Window) error {
	if w.ID == "" {
		return errors.New("window: ID is required")
	}
	if _, exists := s.windows[w.ID]; exists {
		return fmt.Errorf("window: %q is already open", w.ID)
	}
	if w.Bounds.Width <= 0 || w.Bounds.Height <= 0 {
		return fmt.Errorf("window: %q has nonpositive bounds", w.ID)
	}
	s.windows[w.ID] = w
	s.order = append(s.order, w.ID)
	s.bus.Publish(s.source, Opened{Window: w})
	s.Focus(w.ID)
	return nil
}

// Focus raises id to the top and routes subsequent presentation input to it.
func (s *Service) Focus(id ID) bool {
	if _, ok := s.windows[id]; !ok {
		return false
	}
	previous := s.focused
	s.focused = id
	s.raise(id)
	if previous != id {
		s.bus.Publish(s.source, FocusChanged{Previous: previous, Current: id})
	}
	return true
}

func (s *Service) raise(id ID) {
	for i, candidate := range s.order {
		if candidate != id {
			continue
		}
		copy(s.order[i:], s.order[i+1:])
		s.order[len(s.order)-1] = id
		return
	}
}

// Move changes one window's bounds without changing focus or z-order.
func (s *Service) Move(id ID, to Bounds) bool {
	w, ok := s.windows[id]
	if !ok || to.Width <= 0 || to.Height <= 0 || w.Bounds == to {
		return false
	}
	from := w.Bounds
	w.Bounds = to
	s.windows[id] = w
	s.bus.Publish(s.source, Moved{ID: id, From: from, To: to})
	return true
}

// Retitle changes one window's title. A title is metadata, not a lifecycle
// transition, so it publishes no event.
func (s *Service) Retitle(id ID, title string) bool {
	w, ok := s.windows[id]
	if !ok || w.Title == title {
		return false
	}
	w.Title = title
	s.windows[id] = w
	return true
}

// Close removes id. Closing the focused window closes it first, then focuses
// the remaining topmost window, making the event order deterministic.
func (s *Service) Close(id ID) bool {
	if _, ok := s.windows[id]; !ok {
		return false
	}
	delete(s.windows, id)
	for i, candidate := range s.order {
		if candidate == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	s.bus.Publish(s.source, Closed{ID: id})
	if s.focused == id {
		if len(s.order) > 0 {
			s.Focus(s.order[len(s.order)-1])
		} else {
			s.focused = ""
			s.bus.Publish(s.source, FocusChanged{Previous: id})
		}
	}
	return true
}

// Get returns a snapshot of one open window.
func (s *Service) Get(id ID) (Window, bool) {
	w, ok := s.windows[id]
	return w, ok
}

// Focused returns the window that owns presentation input.
func (s *Service) Focused() (ID, bool) {
	_, ok := s.windows[s.focused]
	return s.focused, ok
}

// Len reports how many windows are open.
func (s *Service) Len() int { return len(s.order) }

// Order returns open window IDs from bottom to top.
func (s *Service) Order() []ID {
	return append([]ID(nil), s.order...)
}

// Windows returns open window snapshots from bottom to top.
func (s *Service) Windows() []Window {
	out := make([]Window, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, s.windows[id])
	}
	return out
}
