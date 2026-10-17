// Package apps wires the environment's builtin applications: their
// manifest + factory registrations. The runtime seeds manifest files.
package apps

import (
	"fmt"

	"gostalgia/apps/adventure"
	"gostalgia/apps/calculator"
	"gostalgia/apps/compendium"
	"gostalgia/apps/delve"
	"gostalgia/apps/dogcalc"
	"gostalgia/apps/echo"
	"gostalgia/apps/files"
	"gostalgia/apps/markview"
	"gostalgia/apps/musictoy"
	"gostalgia/apps/notes"
	"gostalgia/apps/petwatch"
	"gostalgia/apps/pomodoro"
	"gostalgia/apps/rss"
	"gostalgia/apps/settings"
	"gostalgia/apps/sysmon"
	"gostalgia/apps/todo"
	"gostalgia/apps/weather"
	"gostalgia/sdk"
)

// Registrar is bootstrap wiring, not a runtime manager exposed to an app.
type Registrar interface {
	RegisterBuiltin(sdk.Manifest, sdk.Factory) error
}

// Register registers every builtin application with the registry.
func Register(r Registrar) error {
	if err := r.RegisterBuiltin(echo.Manifest(), echo.Factory); err != nil {
		return fmt.Errorf("apps: register %s: %w", echo.ID, err)
	}
	if err := r.RegisterBuiltin(notes.Manifest(), notes.Factory); err != nil {
		return fmt.Errorf("apps: register %s: %w", notes.ID, err)
	}
	if err := r.RegisterBuiltin(files.Manifest(), files.Factory); err != nil {
		return fmt.Errorf("apps: register %s: %w", files.ID, err)
	}
	if err := r.RegisterBuiltin(settings.Manifest(), settings.Factory); err != nil {
		return fmt.Errorf("apps: register %s: %w", settings.ID, err)
	}
	if err := r.RegisterBuiltin(calculator.Manifest(), calculator.Factory); err != nil {
		return fmt.Errorf("apps: register %s: %w", calculator.ID, err)
	}
	if err := r.RegisterBuiltin(compendium.Manifest(), compendium.Factory); err != nil {
		return fmt.Errorf("apps: register %s: %w", compendium.ID, err)
	}
	if err := r.RegisterBuiltin(dogcalc.Manifest(), dogcalc.Factory); err != nil {
		return fmt.Errorf("apps: register %s: %w", dogcalc.ID, err)
	}
	if err := r.RegisterBuiltin(petwatch.Manifest(), petwatch.Factory); err != nil {
		return fmt.Errorf("apps: register %s: %w", petwatch.ID, err)
	}
	if err := r.RegisterBuiltin(pomodoro.Manifest(), pomodoro.Factory); err != nil {
		return fmt.Errorf("apps: register %s: %w", pomodoro.ID, err)
	}
	if err := r.RegisterBuiltin(todo.Manifest(), todo.Factory); err != nil {
		return fmt.Errorf("apps: register %s: %w", todo.ID, err)
	}
	if err := r.RegisterBuiltin(musictoy.Manifest(), musictoy.Factory); err != nil {
		return fmt.Errorf("apps: register %s: %w", musictoy.ID, err)
	}
	if err := r.RegisterBuiltin(rss.Manifest(), rss.Factory); err != nil {
		return fmt.Errorf("apps: register %s: %w", rss.ID, err)
	}
	if err := r.RegisterBuiltin(adventure.Manifest(), adventure.Factory); err != nil {
		return fmt.Errorf("apps: register %s: %w", adventure.ID, err)
	}
	if err := r.RegisterBuiltin(markview.Manifest(), markview.Factory); err != nil {
		return fmt.Errorf("apps: register %s: %w", markview.ID, err)
	}
	if err := r.RegisterBuiltin(sysmon.Manifest(), sysmon.Factory); err != nil {
		return fmt.Errorf("apps: register %s: %w", sysmon.ID, err)
	}
	if err := r.RegisterBuiltin(weather.Manifest(), weather.Factory); err != nil {
		return fmt.Errorf("apps: register %s: %w", weather.ID, err)
	}
	if err := r.RegisterBuiltin(delve.Manifest(), delve.Factory); err != nil {
		return fmt.Errorf("apps: register %s: %w", delve.ID, err)
	}
	return nil
}

// Manifests returns fresh builtin declarations, without filesystem access.
func Manifests() []sdk.Manifest {
	return []sdk.Manifest{echo.Manifest(), notes.Manifest(), files.Manifest(), settings.Manifest(), calculator.Manifest(), compendium.Manifest(), dogcalc.Manifest(), petwatch.Manifest(), pomodoro.Manifest(), todo.Manifest(), musictoy.Manifest(), rss.Manifest(), adventure.Manifest(), markview.Manifest(), sysmon.Manifest(), weather.Manifest(), delve.Manifest()}
}
