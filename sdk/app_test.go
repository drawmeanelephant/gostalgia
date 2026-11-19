package sdk

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestManifestOptionalIconMetadata(t *testing.T) {
	// Manifests written before icon metadata existed still decode, and they
	// simply carry no art.
	legacy, err := ParseManifest([]byte(`{"id":"com.test.app","name":"App","version":"1.0.0","entrypoint":"app"}`))
	if err != nil {
		t.Fatalf("legacy manifest: %v", err)
	}
	if legacy.Icon != "" {
		t.Fatalf("legacy manifest icon = %q, want none", legacy.Icon)
	}

	// Optional art survives strict decoding verbatim; display sanitization is
	// the experience layer's job.
	const art = ">_\n>_"
	withIcon, err := ParseManifest([]byte(`{"id":"com.test.app","name":"App","version":"1.0.0","entrypoint":"app","icon":">_\n>_"}`))
	if err != nil {
		t.Fatalf("manifest with icon: %v", err)
	}
	if withIcon.Icon != art {
		t.Fatalf("icon = %q, want %q", withIcon.Icon, art)
	}

	// Hostile terminal controls are transport metadata, not a manifest error.
	hostile, err := ParseManifest([]byte(`{"id":"com.test.app","name":"App","version":"1.0.0","entrypoint":"app","icon":"\u001b[31mX\u001b[0m\nY"}`))
	if err != nil {
		t.Fatalf("manifest with hostile icon: %v", err)
	}
	if !strings.Contains(hostile.Icon, "\x1b") {
		t.Fatalf("hostile icon changed at the SDK boundary: %q", hostile.Icon)
	}

	// A list transport round trip carries the optional field unchanged.
	type listed struct {
		Manifest Manifest `json:"manifest"`
	}
	data, err := json.Marshal(listed{Manifest: withIcon})
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip listed
	if err := json.Unmarshal(data, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if roundTrip.Manifest.Icon != art {
		t.Fatalf("round-trip icon = %q, want %q", roundTrip.Manifest.Icon, art)
	}

	// Storage is bounded at registration; the exact limit still passes.
	atLimit := strings.Repeat("x", MaxIconBytes)
	if _, err := ParseManifest([]byte(`{"id":"com.test.app","name":"App","version":"1.0.0","entrypoint":"app","icon":"` + atLimit + `"}`)); err != nil {
		t.Fatalf("icon at the storage limit: %v", err)
	}
	over := strings.Repeat("x", MaxIconBytes+1)
	if _, err := ParseManifest([]byte(`{"id":"com.test.app","name":"App","version":"1.0.0","entrypoint":"app","icon":"` + over + `"}`)); err == nil || !strings.Contains(err.Error(), "icon art") {
		t.Fatalf("oversized icon error = %v", err)
	}
}

func TestManifestContract(t *testing.T) {
	good := `{"id":"com.test.app","name":"App","version":"1.2.3","entrypoint":"app","permissions":["ipc","fs.read"]}`
	if _, err := ParseManifest([]byte(good)); err != nil {
		t.Fatal(err)
	}
	goodExt := `{"id":"com.test.ext","name":"External App","version":"1.0.0","mode":"external","executable":"/bin/ext","args":["--flag"],"protocol_version":1,"permissions":["ipc"]}`
	if _, err := ParseManifest([]byte(goodExt)); err != nil {
		t.Fatal(err)
	}
	goodSandbox := `{"id":"com.test.sandbox","name":"Sandbox App","version":"1.0.0","mode":"external","executable":"/bin/ext","isolation":"sandbox","permissions":["ipc"]}`
	if m, err := ParseManifest([]byte(goodSandbox)); err != nil || m.EffectiveIsolation() != IsolationSandbox {
		t.Fatalf("unexpected parse sandbox manifest: %v, isolation=%s", err, m.EffectiveIsolation())
	}
	for _, bad := range []string{
		`{}`, `null`, good + ` {}`, good + ` garbage`,
		`{"id":"com.test.app","name":"App","version":"1","entrypoint":"app"}`,
		`{"id":"com.test.app","name":"App","version":"1.0.0","entrypoint":"app","permissions":["admin"]}`,
		`{"id":"com.test.app","name":"App","version":"1.0.0","entrypoint":"app","permissions":["ipc","ipc"]}`,
		`{"id":"com.test.app","name":"App","version":"1.0.0","entrypoint":"app","permissions":["typo"]}`,
		`{"id":"com.test.app","name":"App","version":"1.0.0","entrypoint":"app","unknown":true}`,
		`{"id":"com.test.app","name":"App","version":"1.0.0","mode":"invalid","executable":"foo"}`,
		`{"id":"com.test.app","name":"App","version":"1.0.0","mode":"inproc","executable":"foo","entrypoint":"bar"}`,
		`{"id":"com.test.app","name":"App","version":"1.0.0","mode":"external","executable":""}`,
		`{"id":"com.test.app","name":"App","version":"1.0.0","mode":"external","executable":"foo","protocol_version":999}`,
		`{"id":"com.test.app","name":"App","version":"1.0.0","entrypoint":"app","isolation":"sandbox"}`,
		`{"id":"com.test.ext","name":"External App","version":"1.0.0","mode":"external","executable":"/bin/ext","isolation":"bogus"}`,
	} {
		if _, err := ParseManifest([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}
