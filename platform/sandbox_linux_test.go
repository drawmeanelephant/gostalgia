//go:build linux

package platform

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Regression for #181: /proc/self/mounts parsing feeds the read-only remount
// walk; foreign-userns mounts (WSL 9p/drvfs) must reach the mask fallback,
// not be skipped or mis-parsed.
func TestLinuxParseMounts(t *testing.T) {
	data := "sysfs /sys sysfs rw,nosuid,nodev,noexec,relatime 0 0\n" +
		"/dev/sda / ext4 rw,relatime 0 0\n" +
		"drvfs /mnt/c 9p rw,relatime,aname=drvfs 0 0\n" +
		"none /mnt/space\\040dir ext4 rw 0 0\n" +
		"garbage line\n"
	mounts := linuxParseMounts(data)
	if len(mounts) != 4 {
		t.Fatalf("parsed %d mounts, want 4: %+v", len(mounts), mounts)
	}
	if mounts[1].point != "/" || mounts[1].fstype != "ext4" {
		t.Fatalf("mount[1] = %+v", mounts[1])
	}
	if mounts[2].fstype != "9p" {
		t.Fatalf("drvfs fstype = %q", mounts[2].fstype)
	}
	if mounts[3].point != "/mnt/space dir" {
		t.Fatalf("escaped space = %q", mounts[3].point)
	}
	if !linuxPseudoFSTypes["sysfs"] || linuxPseudoFSTypes["9p"] || linuxPseudoFSTypes["ext4"] {
		t.Fatal("pseudo fstype classification wrong")
	}
}

// TestLinuxConfigureSandboxFlags checks that a sandboxed policy configures
// user+mount namespaces and stages the init payload for the re-exec hook.
func TestLinuxConfigureSandboxFlags(t *testing.T) {
	if !GetHostSecurityCapabilities().Supported {
		t.Skip("sandbox unsupported on this host")
	}
	cmd := exec.Command("/bin/echo", "hi")
	hook, err := ConfigureSandbox(cmd, ExecutionPolicy{
		Isolation:   IsolationSandbox,
		DenyNetwork: true,
	})
	if err != nil {
		t.Fatalf("ConfigureSandbox: %v", err)
	}
	if hook == nil {
		t.Fatal("expected post-start hook for resource limits")
	}
	attr := cmd.SysProcAttr
	if attr == nil {
		t.Fatal("SysProcAttr not set")
	}
	want := uintptr(syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS | syscall.CLONE_NEWNET)
	if attr.Unshareflags&want != want {
		t.Fatalf("Unshareflags = %#x, want NEWUSER|NEWNS|NEWNET", attr.Unshareflags)
	}
	if cmd.Path != "/proc/self/exe" {
		t.Fatalf("cmd.Path = %q, want /proc/self/exe re-exec", cmd.Path)
	}
	if len(attr.UidMappings) != 1 || attr.UidMappings[0].HostID != os.Getuid() {
		t.Fatalf("uid mapping incorrect: %+v", attr.UidMappings)
	}
	var payload string
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, sandboxInitEnv+"=") {
			payload = strings.TrimPrefix(kv, sandboxInitEnv+"=")
		}
	}
	if payload == "" {
		t.Fatal("sandbox init payload missing from env")
	}
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("payload not base64: %v", err)
	}
	var cfg sandboxInitConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("payload not json: %v", err)
	}
	if !strings.HasSuffix(cfg.Path, "/echo") {
		t.Fatalf("payload path = %q, want the real binary", cfg.Path)
	}
}

// TestLinuxTrustedNoNamespace ensures trusted children are untouched.
func TestLinuxTrustedNoNamespace(t *testing.T) {
	cmd := exec.Command("/bin/echo", "hi")
	hook, err := ConfigureSandbox(cmd, ExecutionPolicy{Isolation: IsolationTrusted})
	if err != nil || hook != nil {
		t.Fatalf("trusted: hook=%v err=%v", hook, err)
	}
	if cmd.SysProcAttr != nil && cmd.SysProcAttr.Unshareflags != 0 {
		t.Fatal("trusted child got namespace flags")
	}
	if cmd.Path == "/proc/self/exe" {
		t.Fatal("trusted child was wrapped in re-exec")
	}
}

// procStatAlive reads /proc/<pid>/stat and reports (ppid, pgid, alive).
// Zombies (state Z/X) still appear in /proc with their ppid/pgid intact but
// are already dead, so they must not count.
func procStatAlive(pid int) (ppid, pgid int, alive bool) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, 0, false
	}
	s := string(data)
	i := strings.LastIndexByte(s, ')')
	if i < 0 || i+2 >= len(s) {
		return 0, 0, false
	}
	fields := strings.Fields(s[i+1:])
	if len(fields) < 3 {
		return 0, 0, false
	}
	p, err1 := strconv.Atoi(fields[1])
	g, err2 := strconv.Atoi(fields[2])
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	alive = fields[0] != "Z" && fields[0] != "X"
	return p, g, alive
}

// TestLinuxKillDescendantsProcessGroup verifies the pgid sweep kills group
// members even after they reparent (the classic fork+exit race).
func TestLinuxKillDescendantsProcessGroup(t *testing.T) {
	// Leader execs sleep (so it cannot reap its children); the two background
	// sleeps share its process group.
	cmd := exec.Command("sh", "-c", "sleep 60 & sleep 60 & exec sleep 60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	defer func() { cmd.Process.Kill(); cmd.Wait() }()

	// Give the children a moment to spawn.
	time.Sleep(150 * time.Millisecond)

	KillDescendants(pid)

	// Every non-leader member of the child's process group must be dead
	// (zombies already reaped-or-not do not count).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		alive := 0
		entries, _ := os.ReadDir("/proc")
		for _, e := range entries {
			p, err := strconv.Atoi(e.Name())
			if err != nil || p <= 0 || p == pid {
				continue
			}
			_, pgid, ok := procStatAlive(p)
			if ok && pgid == pid {
				alive++
			}
		}
		if alive == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("descendant process-group members still alive after KillDescendants")
}

// TestLinuxKillDescendantsOrphaned verifies the ppid-tree sweep catches a
// descendant that escaped the process group but not the ancestry tree.
func TestLinuxKillDescendantsOrphaned(t *testing.T) {
	// setsid detaches from the process group but the ppid tree still links
	// the grandchild back through the (still living) intermediate child.
	cmd := exec.Command("sh", "-c", "setsid sleep 60 & exec sleep 60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn test tree: %v", err)
	}
	pid := cmd.Process.Pid
	defer func() { cmd.Process.Kill(); cmd.Wait() }()

	time.Sleep(150 * time.Millisecond)
	KillDescendants(pid)

	// The setsid'd grandchild should be dead within a poll.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		found := false
		entries, _ := os.ReadDir("/proc")
		for _, e := range entries {
			p, err := strconv.Atoi(e.Name())
			if err != nil || p <= 0 {
				continue
			}
			ppid, _, ok := procStatAlive(p)
			if ok && ppid == pid {
				found = true
			}
		}
		if !found {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("ppid-tree descendant still alive after KillDescendants")
}

// TestLinuxCapabilityInvariant: on a host that advertises filesystem
// sandboxing, a sandboxed policy must not silently degrade — the mount
// program must have been staged.
func TestLinuxCapabilityInvariant(t *testing.T) {
	caps := GetHostSecurityCapabilities()
	if !caps.Supported {
		t.Skipf("sandbox unsupported: %s", caps.Reason)
	}
	if !caps.FilesystemSandbox {
		t.Fatal("Supported without FilesystemSandbox: capability drift")
	}
}
