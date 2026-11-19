package e2e

import (
	"context"
	"io"
	"testing"
	"time"

	"gostalgia/internal/app"
	"gostalgia/internal/runtime"
)

// TestAppListCarriesOptionalIconMetadata drives the real runtime over its
// authenticated socket: a built-in manifest's optional icon survives app/list
// unchanged, a manifest without one still lists, and the art is not required
// to launch or present the app.
func TestAppListCarriesOptionalIconMetadata(t *testing.T) {
	root := t.TempDir()
	rt, err := runtime.Boot(context.Background(), runtime.Options{Root: root, LogOutput: io.Discard})
	must(t, err)
	t.Cleanup(func() { rt.Shutdown("app icon e2e test") })
	client := dialRunning(t, root)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var list []app.Status
	must(t, client.Call(ctx, "app/list", nil, &list))
	icons := map[string]string{}
	for _, status := range list {
		icons[status.Manifest.ID] = status.Manifest.Icon
	}
	if got := icons["com.gostalgia.echo"]; got != ">_" {
		t.Fatalf("Echo icon over app/list = %q, want %q", got, ">_")
	}
	if got, listed := icons["com.gostalgia.pomodoro"]; !listed || got != "" {
		t.Fatalf("Pomodoro icon = %q, listed = %v; want no icon", got, listed)
	}

	// Echo is already running at boot; stop it so this single-instance launch
	// proves the icon-bearing manifest still launches.
	must(t, client.Call(ctx, "app/stop", map[string]string{"id": "com.gostalgia.echo"}, nil))
	var launched struct {
		ID  string `json:"id"`
		PID int32  `json:"pid"`
	}
	must(t, client.Call(ctx, "app/launch", map[string]string{"id": "com.gostalgia.echo"}, &launched))
	if launched.PID <= 0 {
		t.Fatalf("icon art blocked Echo's launch: %+v", launched)
	}
	must(t, client.Call(ctx, "app/stop", map[string]string{"id": "com.gostalgia.echo"}, nil))
}
