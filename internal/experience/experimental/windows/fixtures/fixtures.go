// Package fixtures retains the existing populated shell presentation fixtures
// for the experimental window demonstration and their original shell tests.
package fixtures

import "gostalgia/sdk"

// ScreenData is the version-1 item/form/action fixture used by the shell.
func ScreenData() sdk.View {
	return sdk.View{
		Version: 1, Instance: "launch", Title: "Screen", State: sdk.ViewReady,
		Items:   []sdk.Item{{ID: "one", Label: "One", Detail: "first"}, {ID: "two", Label: "Two", Detail: "second"}},
		Fields:  []sdk.Field{{ID: "text", Label: "Text", Required: true}},
		Actions: []sdk.Action{{ID: "send", Label: "Send"}},
	}
}

// PadData is the version-2 fixture exercising blocks, meters, and a grid with
// cell-declared actions, a datum cell, and a disabled cell.
func PadData() sdk.View {
	return sdk.View{
		Version: 2, Instance: "launch", Title: "Pad", State: sdk.ViewReady,
		Items:   []sdk.Item{{ID: "one", Label: "One", Detail: "first"}},
		Fields:  []sdk.Field{{ID: "text", Label: "Text"}},
		Actions: []sdk.Action{{ID: "adopt", Label: "Adopt"}},
		Blocks:  []sdk.Block{{ID: "art", Label: "Mascot", Text: "(\\_/)\n(o.o)\n> ^ <"}},
		Meters: []sdk.Meter{
			{ID: "charge", Label: "Charge", Value: 0.5},
			{ID: "hunger", Label: "Hunger", Value: 1.4},
		},
		Grid: &sdk.Grid{Label: "Pad", Columns: 2, Cells: []sdk.Cell{
			{ID: "nw", Label: "NW", Action: "press"},
			{ID: "ne", Label: "NE", Action: "press"},
			{ID: "sw", Label: "SW"},
			{ID: "se", Label: "SE", Disabled: true},
		}},
		Status: "waiting",
	}
}
