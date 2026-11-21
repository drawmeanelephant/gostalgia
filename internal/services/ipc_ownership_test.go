package services

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Regression for #259: when two instances once co-owned a root, the first
// to exit deleted the survivor's runtime.json, leaving it unreachable by
// gctl and unprotected by the guard. Stop may only remove a file whose
// pid and token still match this instance.
func TestIPCStopLeavesForeignRuntimeFile(t *testing.T) {
	sctx := bootTestContext(t)
	svc := NewIPC()
	if err := svc.Init(sctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	// A different instance's record lands where ours was — a survivor of
	// a double-boot, or simply a newer boot on the same root.
	foreign := runtimeFile{
		PID:      os.Getpid() + 1,
		Version:  sctx.Version,
		Endpoint: "unix:///other-instance.sock",
		Token:    "foreign-token",
	}
	data, err := json.Marshal(foreign)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sctx.Root, "runtime.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := svc.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Stop removed a runtime.json owned by another instance: %v", err)
	}
}

// The matching case: Stop does remove the file this instance wrote.
func TestIPCStopRemovesOwnRuntimeFile(t *testing.T) {
	sctx := bootTestContext(t)
	svc := NewIPC()
	if err := svc.Init(sctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := svc.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(sctx.Root, "runtime.json")); !os.IsNotExist(err) {
		t.Fatalf("Stop left runtime.json behind (stat: %v)", err)
	}
}
