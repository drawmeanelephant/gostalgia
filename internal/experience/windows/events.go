package windows

// Lifecycle topic names. They are experience events on the internal bus, not
// additions to the public SDK contract: apps never observe windows.
const (
	EventOpened  = "window.opened"
	EventFocused = "window.focused"
	EventMoved   = "window.moved"
	EventClosed  = "window.closed"
)

// Opened reports a new window and its initial metadata.
type Opened struct {
	Window Window
}

func (Opened) Type() string { return EventOpened }

// FocusChanged reports both sides of a focus transition. An empty ID means no
// window had focus before the transition.
type FocusChanged struct {
	Previous ID
	Current  ID
}

func (FocusChanged) Type() string { return EventFocused }

// Moved reports a completed pointer-motion step or another explicit bounds
// change. The neutral API uses terminal-independent coordinates; adapters
// decide what one unit means.
type Moved struct {
	ID   ID
	From Bounds
	To   Bounds
}

func (Moved) Type() string { return EventMoved }

// Closed reports removal of a window.
type Closed struct {
	ID ID
}

func (Closed) Type() string { return EventClosed }
