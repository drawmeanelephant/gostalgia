// Command windowdemo runs the deliberately unwired experimental bloom window
// adapter. It is not registered with gostalgia shell or the runtime.
package main

import (
	"context"
	"fmt"
	"os"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"gostalgia/internal/events"
	"gostalgia/internal/experience/experimental/windows"
	"gostalgia/internal/experience/experimental/windows/bloomadapter"
	"gostalgia/internal/experience/experimental/windows/fixtures"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
)

func main() {
	bus := events.NewBus()
	service := windows.NewService(bus, "experimental/window-demo")
	if err := service.Open(windows.Window{
		ID: "screen", Title: "Screen / form", Bounds: windows.Bounds{X: 1, Y: 1, Width: 38, Height: 20},
		Presentation: fixtures.ScreenData(),
	}); err != nil {
		fatal(err)
	}
	if err := service.Open(windows.Window{
		ID: "pad", Title: "Pad / blocks and grid", Bounds: windows.Bounds{X: 41, Y: 1, Width: 38, Height: 20},
		Presentation: fixtures.PadData(),
	}); err != nil {
		fatal(err)
	}
	model := bloomadapter.New(service, ui.New(theme.Nostalgia(), ui.ANSI256))
	program := tea.NewProgram(model, tea.WithContext(context.Background()), tea.WithColorProfile(colorprofile.TrueColor))
	if _, err := program.Run(); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "experimental window demo:", err)
	os.Exit(1)
}
