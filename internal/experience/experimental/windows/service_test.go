package windows

import (
	"os/exec"
	"strings"
	"testing"

	"gostalgia/internal/events"
	"gostalgia/internal/experience/experimental/windows/fixtures"
)

func TestServicePublishesTypedLifecycleInTransitionOrder(t *testing.T) {
	bus := events.NewBus()
	var envelopes []events.Envelope
	cancel := bus.Subscribe("*", func(envelope events.Envelope) {
		envelopes = append(envelopes, envelope)
	})
	defer cancel()
	service := NewService(bus, "test/windows")
	screen := Window{ID: "screen", Title: "Screen", Bounds: Bounds{X: 1, Y: 2, Width: 30, Height: 12}, Presentation: fixtures.ScreenData()}
	pad := Window{ID: "pad", Title: "Pad", Bounds: Bounds{X: 32, Y: 2, Width: 30, Height: 12}, Presentation: fixtures.PadData()}
	if err := service.Open(screen); err != nil {
		t.Fatal(err)
	}
	if err := service.Open(pad); err != nil {
		t.Fatal(err)
	}
	if !service.Focus("screen") {
		t.Fatal("focus screen")
	}
	to := Bounds{X: 3, Y: 4, Width: 30, Height: 12}
	if !service.Move("screen", to) {
		t.Fatal("move screen")
	}
	if !service.Close("screen") {
		t.Fatal("close screen")
	}
	if len(envelopes) != 8 {
		t.Fatalf("got %d envelopes, want 8: %#v", len(envelopes), envelopes)
	}
	for _, envelope := range envelopes {
		if envelope.Source != "test/windows" {
			t.Fatalf("empty or wrong source: %+v", envelope)
		}
	}
	if _, ok := envelopes[0].Payload.(Opened); !ok || envelopes[0].Payload.(Opened).Window.ID != "screen" {
		t.Fatalf("first payload = %#v", envelopes[0].Payload)
	}
	if got := envelopes[1].Payload.(FocusChanged); got.Current != "screen" || got.Previous != "" {
		t.Fatalf("initial focus = %+v", got)
	}
	if _, ok := envelopes[2].Payload.(Opened); !ok || envelopes[2].Payload.(Opened).Window.ID != "pad" {
		t.Fatalf("third payload = %#v", envelopes[2].Payload)
	}
	if got := envelopes[4].Payload.(FocusChanged); got.Previous != "pad" || got.Current != "screen" {
		t.Fatalf("focus transition = %+v", got)
	}
	if got := envelopes[5].Payload.(Moved); got.ID != "screen" || got.To != to {
		t.Fatalf("move transition = %+v", got)
	}
	if got := envelopes[6].Payload.(Closed); got.ID != "screen" || got.Presentation.Title != "Screen" {
		t.Fatalf("close transition = %+v", got)
	}
	if got := envelopes[7].Payload.(FocusChanged); got.Previous != "screen" || got.Current != "pad" {
		t.Fatalf("close focus transition = %+v", got)
	}
	// The ninth event is the explicit transition to no focus when the last
	// window closes. Close the survivor and keep that behavior pinned.
	if !service.Close("pad") {
		t.Fatal("close pad")
	}
	if got := envelopes[9].Payload.(FocusChanged); got.Previous != "pad" || got.Current != "" {
		t.Fatalf("final focus transition = %+v", got)
	}
}

func TestServiceRejectsInvalidTransitionsWithoutEvents(t *testing.T) {
	bus := events.NewBus()
	service := NewService(bus, "test/windows")
	count := 0
	cancel := bus.Subscribe("*", func(events.Envelope) { count++ })
	defer cancel()
	if err := service.Open(Window{}); err == nil {
		t.Fatal("empty window opened")
	}
	if service.Focus("missing") || service.Move("missing", Bounds{Width: 1, Height: 1}) || service.Close("missing") {
		t.Fatal("missing-window transition succeeded")
	}
	if count != 0 {
		t.Fatalf("invalid transitions published %d events", count)
	}
}

func TestNeutralAPIAndProductionDependencyBoundaries(t *testing.T) {
	neutral := exec.Command("go", "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", "gostalgia/internal/experience/experimental/windows")
	out, err := neutral.CombinedOutput()
	if err != nil {
		t.Fatalf("list neutral dependencies: %v\n%s", err, out)
	}
	for _, pkg := range strings.Fields(string(out)) {
		if strings.HasPrefix(pkg, "bloomfx.dev/") || strings.HasPrefix(pkg, "charm.land/") || strings.Contains(pkg, "charmbracelet") {
			t.Errorf("neutral API imports toolkit package %s", pkg)
		}
	}
	production := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", "gostalgia/cmd/gostalgia", "gostalgia/internal/experience/shell")
	out, err = production.CombinedOutput()
	if err != nil {
		t.Fatalf("list production dependencies: %v\n%s", err, out)
	}
	for _, pkg := range strings.Fields(string(out)) {
		if strings.Contains(pkg, "/experimental/windows") {
			t.Errorf("production package depends on experiment: %s", pkg)
		}
	}
}
