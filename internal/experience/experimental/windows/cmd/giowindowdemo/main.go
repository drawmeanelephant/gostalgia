//go:build giodemo

// Command giowindowdemo runs the deliberately unwired experimental Gio window
// adapter in a real gioui.org/app window. It is not registered with gostalgia
// shell or the runtime. It requires -tags giodemo: gioui.org/app uses cgo on
// macOS and needs X11/Wayland/EGL development packages on Linux, so the file
// stays out of default builds and CI.
package main

import (
	"fmt"
	"os"

	"gioui.org/app"
	"gioui.org/op"
	"gostalgia/internal/events"
	"gostalgia/internal/experience/experimental/windows"
	"gostalgia/internal/experience/experimental/windows/fixtures"
	"gostalgia/internal/experience/experimental/windows/gioadapter"
	"gostalgia/internal/experience/theme"
)

func main() {
	go func() {
		var w app.Window
		w.Option(app.Title("gostalgia gio window spike"), app.Size(700, 420))
		if err := loop(&w); err != nil {
			fmt.Fprintln(os.Stderr, "experimental gio window demo:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}()
	app.Main()
}

func loop(w *app.Window) error {
	bus := events.NewBus()
	service := windows.NewService(bus, "experimental/gio-window-demo")
	if err := service.Open(windows.Window{
		ID: "screen", Title: "Screen / form", Bounds: windows.Bounds{X: 8, Y: 8, Width: 280, Height: 240},
		Presentation: fixtures.ScreenData(),
	}); err != nil {
		return err
	}
	if err := service.Open(windows.Window{
		ID: "pad", Title: "Pad / blocks and grid", Bounds: windows.Bounds{X: 320, Y: 8, Width: 280, Height: 240},
		Presentation: fixtures.PadData(),
	}); err != nil {
		return err
	}
	model := gioadapter.New(service, theme.Nostalgia())
	var ops op.Ops
	for {
		switch e := w.Event().(type) {
		case app.DestroyEvent:
			return e.Err
		case app.FrameEvent:
			gtx := app.NewContext(&ops, e)
			model.Layout(gtx)
			e.Frame(gtx.Ops)
		}
		if model.Quit {
			return nil
		}
	}
}
