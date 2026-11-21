package runtime

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"gostalgia/platform"
)

// Regression for #259: two boots racing past a lock-free check-then-act
// guard could both own one root. Ownership is now claimed atomically
// before any root mutation, so a concurrent second boot must fail and
// exactly one winner may exist.
func TestConcurrentBootsSingleWinner(t *testing.T) {
	const rounds = 5
	for i := 0; i < rounds; i++ {
		root := t.TempDir()
		gate := make(chan struct{})
		errs := make(chan error, 2)
		booted := make(chan *Runtime, 2)
		for k := 0; k < 2; k++ {
			go func() {
				<-gate
				rt, err := Boot(context.Background(), Options{Root: root, LogOutput: io.Discard})
				if err != nil {
					errs <- err
					return
				}
				booted <- rt
			}()
		}
		close(gate)

		var winners []*Runtime
		deadline := 2
		for got := 0; got < deadline; {
			select {
			case rt := <-booted:
				winners = append(winners, rt)
				got++
			case err := <-errs:
				if err == nil {
					t.Fatalf("round %d: nil error without runtime", i)
				}
				got++
			}
		}
		if len(winners) != 1 {
			for _, rt := range winners {
				rt.Shutdown("test cleanup")
			}
			t.Fatalf("round %d: %d boots succeeded on %s, want exactly one winner", i, len(winners), root)
		}
		winners[0].Shutdown("test")

		// The only owner's shutdown removes its own runtime.json; nothing
		// may delete it earlier, and nothing may leave it behind.
		if _, err := os.Stat(filepath.Join(root, "runtime.json")); !os.IsNotExist(err) {
			t.Fatalf("round %d: runtime.json survives shutdown (stat: %v)", i, err)
		}
	}
}

// A boot refused on a locked root must fail before touching the root at
// all: the guard is a claim taken before the layout is created, not a
// probe that runs after config, profiles, and manifests were seeded.
func TestRefusedBootDoesNotTouchRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "env")
	lock, err := platform.TryLockRoot(root)
	if err != nil {
		t.Fatalf("seed lock: %v", err)
	}
	defer func() { _ = lock.Close() }()

	if _, err := Boot(context.Background(), Options{Root: root, LogOutput: io.Discard}); !errors.Is(err, platform.ErrLocked) {
		t.Fatalf("boot on locked root = %v, want ErrLocked", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("refused boot created the root (stat: %v)", err)
	}
}

// A second sequential boot while the first still runs is refused, and the
// winner's shutdown releases the claim so a later boot can own the root.
func TestSecondBootRefusedAndShutdownReleases(t *testing.T) {
	root := t.TempDir()
	rt, err := Boot(context.Background(), Options{Root: root, LogOutput: io.Discard})
	if err != nil {
		t.Fatalf("first boot: %v", err)
	}
	if _, err := Boot(context.Background(), Options{Root: root, LogOutput: io.Discard}); err == nil {
		t.Fatal("second boot on a live root succeeded")
	}
	rt.Shutdown("test")

	rt2, err := Boot(context.Background(), Options{Root: root, LogOutput: io.Discard})
	if err != nil {
		t.Fatalf("boot after shutdown did not reclaim the root: %v", err)
	}
	rt2.Shutdown("test")
}
