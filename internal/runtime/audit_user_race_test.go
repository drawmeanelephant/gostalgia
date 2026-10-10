package runtime

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gostalgia/internal/ipc"
	"gostalgia/platform"
)

// Regression for #148: the profile/switch callback writes the runtime's
// user identity from whichever goroutine performs the switch — for an
// operator IPC client, an IPC handler goroutine. The IPC service comes up
// while later services (including the profile route) are still starting,
// so a switch can land while the boot goroutine reads the identity for the
// first session. Unsynchronized, that is a data race under -race.
//
// The test boots once to seed a second profile, then reboots while a
// client hammers profile/switch as soon as runtime.json appears.
func TestAuditRuntimeUserRaceDuringBoot(t *testing.T) {
	root := t.TempDir()

	rt, err := Boot(context.Background(), Options{Root: root, LogOutput: io.Discard})
	if err != nil {
		t.Fatalf("seed boot: %v", err)
	}
	if _, err := rt.Profiles.Create("alice", "Alice"); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	rt.Shutdown("seed done")

	bootDone := make(chan struct{})
	hammerDone := make(chan struct{})
	go func() {
		defer close(hammerDone)
		endpoint, token := waitRuntimeFile(root, bootDone)
		if endpoint == "" {
			return
		}
		var client *ipc.Client
		for client == nil {
			select {
			case <-bootDone:
				return
			default:
			}
			conn, err := platform.DialIPC(endpoint)
			if err != nil {
				time.Sleep(5 * time.Millisecond)
				continue
			}
			c, err := ipc.NewClient(conn, token)
			if err != nil {
				conn.Close()
				time.Sleep(5 * time.Millisecond)
				continue
			}
			client = c
		}
		defer client.Close()
		target := "alice"
		for {
			select {
			case <-bootDone:
				return
			default:
			}
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			// The route does not exist until the profile service starts —
			// call errors are expected and ignored; we only need the write
			// path to run concurrently with the boot-time user read.
			_ = client.Call(ctx, "profile/switch", map[string]string{"id": target}, nil)
			cancel()
			if target == "alice" {
				target = "guest"
			} else {
				target = "alice"
			}
		}
	}()

	rt2, err := Boot(context.Background(), Options{Root: root, LogOutput: io.Discard})
	close(bootDone)
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	defer rt2.Shutdown("test done")
	<-hammerDone

	// Whatever the interleaving, the identity must never be torn:
	// ID is always "u-"+Name.
	if u := rt2.CurrentUser(); u.ID != "u-"+u.Name {
		t.Fatalf("torn user identity: %+v", u)
	}
}

// The same synchronized path, exercised deterministically: concurrent
// profile switches vs. user reads. Run under -race.
func TestRuntimeUserConsistentUnderConcurrentSwitch(t *testing.T) {
	root := t.TempDir()
	rt, err := Boot(context.Background(), Options{Root: root, LogOutput: io.Discard})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	defer rt.Shutdown("test done")
	if _, err := rt.Profiles.Create("alice", "Alice"); err != nil {
		t.Fatalf("create profile: %v", err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			target := "alice"
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := rt.Profiles.Switch(target); err == nil {
					if target == "alice" {
						target = "guest"
					} else {
						target = "alice"
					}
				}
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if u := rt.CurrentUser(); u.ID != "u-"+u.Name {
					t.Errorf("torn user identity: %+v", u)
					return
				}
			}
		}()
	}
	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// waitRuntimeFile polls for the endpoint advertisement a booting
// environment writes, giving up once boot returns or after a bound.
func waitRuntimeFile(root string, bootDone <-chan struct{}) (endpoint, token string) {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-bootDone:
			return "", ""
		default:
		}
		data, err := os.ReadFile(filepath.Join(root, "runtime.json"))
		if err == nil {
			var info struct {
				Endpoint string `json:"endpoint"`
				Token    string `json:"token"`
			}
			if json.Unmarshal(data, &info) == nil && info.Endpoint != "" {
				return info.Endpoint, info.Token
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	return "", ""
}
