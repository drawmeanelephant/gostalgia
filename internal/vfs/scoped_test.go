package vfs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func setupTestVFS(t *testing.T) (*VFS, *HostFS) {
	t.Helper()
	root := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(root, "apps", "data"), 0o755))
	must(t, os.MkdirAll(filepath.Join(root, "shared", "workspace"), 0o755))
	must(t, os.WriteFile(filepath.Join(root, "shared", "notes.txt"), []byte("notes content"), 0o644))
	h, err := NewHost(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	v := New(h)
	return v, h
}

func setupTestMemVFS(t *testing.T) *VFS {
	t.Helper()
	m := NewMem()
	v := New(m)
	must(t, v.MkdirAll("/apps/data"))
	must(t, v.MkdirAll("/shared/workspace"))
	must(t, v.WriteFile("/shared/notes.txt", []byte("mem notes content"), 0o644))
	return v
}

func TestAppPrivateStorage_HostFS(t *testing.T) {
	v, _ := setupTestVFS(t)
	appA := v.ForApp("appA")
	appB := v.ForApp("appB")

	// AppA writes a private file
	err := appA.WriteFile("/apps/data/appA/secret.txt", []byte("secretA"), 0o644)
	if err != nil {
		t.Fatalf("appA write: %v", err)
	}

	// AppA can read its private file
	data, err := appA.ReadFile("/apps/data/appA/secret.txt")
	if err != nil || string(data) != "secretA" {
		t.Fatalf("appA read: %v, got %q", err, string(data))
	}

	// AppB CANNOT read AppA's private file
	_, err = appB.ReadFile("/apps/data/appA/secret.txt")
	if err == nil {
		t.Fatal("expected appB read of appA secret to fail")
	}
	var vErr *Error
	if !errors.As(err, &vErr) || vErr.Code != ErrPermission {
		t.Fatalf("expected ErrPermission, got %v", err)
	}

	// AppB CANNOT write into AppA's private directory
	err = appB.WriteFile("/apps/data/appA/hacked.txt", []byte("evil"), 0o644)
	if err == nil {
		t.Fatal("expected appB write to appA to fail")
	}

	// AppB CANNOT remove AppA's file
	err = appB.Remove("/apps/data/appA/secret.txt")
	if err == nil {
		t.Fatal("expected appB remove to fail")
	}

	// AppB CANNOT stat AppA's file
	_, err = appB.Stat("/apps/data/appA/secret.txt")
	if err == nil {
		t.Fatal("expected appB stat to fail")
	}

	// AppB CANNOT rename AppA's file
	err = appB.Rename("/apps/data/appA/secret.txt", "/apps/data/appB/stolen.txt")
	if err == nil {
		t.Fatal("expected appB rename to fail")
	}

	// AppB CANNOT copy AppA's file
	err = appB.Copy("/apps/data/appA/secret.txt", "/apps/data/appB/copy.txt", false)
	if err == nil {
		t.Fatal("expected appB copy to fail")
	}

	// AppB CANNOT move AppA's file
	err = appB.Move("/apps/data/appA/secret.txt", "/apps/data/appB/moved.txt", false)
	if err == nil {
		t.Fatal("expected appB move to fail")
	}

	// AppB cannot delete app private storage root
	err = appB.RemoveAll("/apps/data")
	if err == nil {
		t.Fatal("expected remove of /apps/data to fail")
	}
	err = appB.RemoveAll("/apps/data/appB")
	if err == nil {
		t.Fatal("expected remove of /apps/data/appB to fail")
	}

	// AppB listing /apps/data should only see appB, never appA
	must(t, appB.WriteFile("/apps/data/appB/own.txt", []byte("mine"), 0o644))
	entries, err := appB.ReadDir("/apps/data")
	if err != nil {
		t.Fatalf("appB ReadDir /apps/data: %v", err)
	}
	for _, e := range entries {
		if e.Name() == "appA" {
			t.Fatalf("appB should not see appA directory in /apps/data")
		}
	}
	if len(entries) != 1 || entries[0].Name() != "appB" {
		t.Fatalf("appB should see exactly [appB], got %v", entries)
	}
}

func TestAppPrivateStorage_MemFS(t *testing.T) {
	v := setupTestMemVFS(t)
	appA := v.ForApp("appA")
	appB := v.ForApp("appB")

	// AppA writes a private file
	err := appA.WriteFile("/apps/data/appA/mem_secret.txt", []byte("memA"), 0o644)
	if err != nil {
		t.Fatalf("mem appA write: %v", err)
	}

	// AppB cannot read AppA's private file
	_, err = appB.ReadFile("/apps/data/appA/mem_secret.txt")
	if err == nil {
		t.Fatal("expected appB read to fail on MemFS")
	}
	var vErr *Error
	if !errors.As(err, &vErr) || vErr.Code != ErrPermission {
		t.Fatalf("expected ErrPermission on MemFS, got %v", err)
	}

	// AppB cannot write to AppA's private storage
	err = appB.WriteFile("/apps/data/appA/bad.txt", []byte("bad"), 0o644)
	if err == nil {
		t.Fatal("expected appB write to fail on MemFS")
	}

	// AppB listing /apps/data only sees appB
	must(t, appB.WriteFile("/apps/data/appB/b.txt", []byte("b"), 0o644))
	entries, err := appB.ReadDir("/apps/data")
	if err != nil {
		t.Fatalf("mem ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() == "appA" {
			t.Fatalf("mem appB should not see appA")
		}
	}
}

func TestScopedGrants_ReadOnlyAndReadWrite(t *testing.T) {
	v, _ := setupTestVFS(t)
	appA := v.ForApp("appA")
	appB := v.ForApp("appB")

	// Initially appA has no access to /shared/notes.txt
	_, err := appA.ReadFile("/shared/notes.txt")
	if err == nil {
		t.Fatal("expected appA to be denied without grant")
	}

	// Issue read-only grant for /shared/notes.txt to appA
	_, err = v.Grants().Issue("appA", "/shared/notes.txt", AccessRead, false)
	if err != nil {
		t.Fatalf("issue grant: %v", err)
	}

	// AppA can read now
	data, err := appA.ReadFile("/shared/notes.txt")
	if err != nil || string(data) != "notes content" {
		t.Fatalf("appA read after grant: %v, got %q", err, string(data))
	}

	// AppA CANNOT write or save (grant is read-only)
	err = appA.WriteFile("/shared/notes.txt", []byte("overwrite"), 0o644)
	if err == nil {
		t.Fatal("expected write to fail on read-only grant")
	}
	var vErr *Error
	if !errors.As(err, &vErr) || vErr.Code != ErrReadOnly {
		t.Fatalf("expected ErrReadOnly, got %v", err)
	}

	err = appA.SaveAtomic("/shared/notes.txt", []byte("atomic"), 0o644)
	if err == nil {
		t.Fatal("expected save to fail on read-only grant")
	}

	err = appA.Remove("/shared/notes.txt")
	if err == nil {
		t.Fatal("expected remove to fail on read-only grant")
	}

	// AppB still cannot read /shared/notes.txt
	_, err = appB.ReadFile("/shared/notes.txt")
	if err == nil {
		t.Fatal("expected appB to be denied")
	}

	// Issue read-write recursive grant for /shared/workspace to appB
	_, err = v.Grants().Issue("appB", "/shared/workspace", AccessReadWrite, true)
	if err != nil {
		t.Fatalf("issue recursive rw grant: %v", err)
	}

	// AppB can create subdirs and files inside /shared/workspace
	must(t, appB.MkdirAll("/shared/workspace/sub"))
	must(t, appB.WriteFile("/shared/workspace/sub/file.txt", []byte("hello"), 0o644))

	bData, err := appB.ReadFile("/shared/workspace/sub/file.txt")
	if err != nil || string(bData) != "hello" {
		t.Fatalf("read workspace file: %v, got %q", err, string(bData))
	}

	// AppB cannot write to /shared/workspace2 (prefix check boundary)
	err = appB.WriteFile("/shared/workspace2/evil.txt", []byte("evil"), 0o644)
	if err == nil {
		t.Fatal("expected write to /shared/workspace2 to fail")
	}

	// AppB cannot write to /shared/outside.txt
	err = appB.WriteFile("/shared/outside.txt", []byte("outside"), 0o644)
	if err == nil {
		t.Fatal("expected write to /shared/outside.txt to fail")
	}
}

func TestScopedGrants_Revocation(t *testing.T) {
	v, _ := setupTestVFS(t)
	appA := v.ForApp("appA")

	grant, err := v.Grants().Issue("appA", "/shared/notes.txt", AccessRead, false)
	if err != nil {
		t.Fatal(err)
	}

	// Read succeeds while grant is active
	_, err = appA.ReadFile("/shared/notes.txt")
	if err != nil {
		t.Fatalf("expected read to succeed: %v", err)
	}

	// Revoke the grant
	err = v.Grants().Revoke(grant.ID)
	if err != nil {
		t.Fatalf("revoke failed: %v", err)
	}

	// Read fails immediately
	_, err = appA.ReadFile("/shared/notes.txt")
	if err == nil {
		t.Fatal("expected read to fail immediately after revocation")
	}
	var vErr *Error
	if !errors.As(err, &vErr) || vErr.Code != ErrPermission {
		t.Fatalf("expected ErrPermission, got %v", err)
	}

	// Revoked status persists
	reloadedGrant, ok := v.Grants().Get(grant.ID)
	if !ok || !reloadedGrant.Revoked {
		t.Fatal("expected grant to remain revoked")
	}
}

// Session-bound grants (document handoffs) die with the app's run via
// RevokeAppSession; standing grants from the same app are untouched.
func TestScopedGrants_SessionBound(t *testing.T) {
	v, _ := setupTestVFS(t)
	appA := v.ForApp("appA")
	gs := v.Grants()

	session, err := gs.IssueSession("appA", "/shared/notes.txt", AccessRead, false)
	if err != nil {
		t.Fatal(err)
	}
	if !session.SessionBound {
		t.Fatal("IssueSession did not mark the grant session-bound")
	}
	stable, err := gs.Issue("appA", "/shared/workspace", AccessReadWrite, true)
	if err != nil {
		t.Fatal(err)
	}
	if stable.SessionBound {
		t.Fatal("Issue marked a standing grant session-bound")
	}
	other, err := gs.IssueSession("appB", "/shared/notes.txt", AccessRead, false)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := appA.ReadFile("/shared/notes.txt"); err != nil {
		t.Fatalf("read with session grant failed: %v", err)
	}

	gs.RevokeAppSession("appA")

	if _, err := appA.ReadFile("/shared/notes.txt"); err == nil {
		t.Fatal("session-bound grant survived RevokeAppSession")
	}
	if now, _ := gs.Get(session.ID); !now.Revoked {
		t.Fatal("session-bound grant not marked revoked")
	}
	if now, _ := gs.Get(stable.ID); now.Revoked {
		t.Fatal("standing grant revoked by RevokeAppSession")
	}
	if now, _ := gs.Get(other.ID); now.Revoked {
		t.Fatal("another app's session grant revoked by RevokeAppSession")
	}
}

func TestScopedGrants_ConfinementAndSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink tests require admin privileges on Windows")
	}
	v, h := setupTestVFS(t)
	appA := v.ForApp("appA")
	appB := v.ForApp("appB")

	// Setup private files
	must(t, appB.WriteFile("/apps/data/appB/secret.txt", []byte("topsecret"), 0o644))
	must(t, appA.WriteFile("/apps/data/appA/dummy.txt", []byte("dummy"), 0o644))

	// Create a malicious symlink on host inside appA's private directory pointing to appB's directory
	hostAppADir := filepath.Join(h.RootPath(), "apps", "data", "appA")
	hostAppBDir := filepath.Join(h.RootPath(), "apps", "data", "appB")
	symlinkPath := filepath.Join(hostAppADir, "evil_link")
	if err := os.Symlink(hostAppBDir, symlinkPath); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	// AppA attempting to read /apps/data/appA/evil_link/secret.txt must be rejected with ErrEscape
	_, err := appA.ReadFile("/apps/data/appA/evil_link/secret.txt")
	if err == nil {
		t.Fatal("expected symlink escape read to fail")
	}
	var vErr *Error
	if !errors.As(err, &vErr) || vErr.Code != ErrEscape {
		t.Fatalf("expected ErrEscape, got %v", err)
	}
}

func TestScopedGrants_CrossMount(t *testing.T) {
	v, _ := setupTestVFS(t)
	// Mount MemFS at /tmp
	must(t, v.Mount("/tmp", NewMem()))

	appA := v.ForApp("appA")

	// Issue rw grant on /tmp/scratch to appA
	_, err := v.Grants().Issue("appA", "/tmp/scratch", AccessReadWrite, true)
	if err != nil {
		t.Fatal(err)
	}

	// Write to MemFS
	must(t, appA.MkdirAll("/tmp/scratch"))
	must(t, appA.WriteFile("/tmp/scratch/temp.txt", []byte("transient"), 0o644))

	// Copy from MemFS to HostFS app-private storage
	err = appA.Copy("/tmp/scratch/temp.txt", "/apps/data/appA/saved.txt", false)
	if err != nil {
		t.Fatalf("cross-mount copy: %v", err)
	}

	data, err := appA.ReadFile("/apps/data/appA/saved.txt")
	if err != nil || string(data) != "transient" {
		t.Fatalf("read copied file: %v, got %q", err, string(data))
	}

	// Cross-mount rename should fail with ErrCrossMount
	err = appA.Rename("/tmp/scratch/temp.txt", "/apps/data/appA/renamed.txt")
	if err == nil {
		t.Fatal("expected cross-mount rename to fail")
	}
	var vErr *Error
	if !errors.As(err, &vErr) || vErr.Code != ErrCrossMount {
		t.Fatalf("expected ErrCrossMount, got %v", err)
	}

	// Cross-mount move should succeed
	err = appA.Move("/tmp/scratch/temp.txt", "/apps/data/appA/moved.txt", false)
	if err != nil {
		t.Fatalf("cross-mount move: %v", err)
	}

	// Original in MemFS should be gone
	_, err = appA.ReadFile("/tmp/scratch/temp.txt")
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("expected moved source to not exist, got %v", err)
	}
}

func TestScopedVFS_TrashIsolation(t *testing.T) {
	v, _ := setupTestVFS(t)
	appA := v.ForApp("appA")
	appB := v.ForApp("appB")

	must(t, appA.WriteFile("/apps/data/appA/fileA.txt", []byte("contentA"), 0o644))
	must(t, appB.WriteFile("/apps/data/appB/fileB.txt", []byte("contentB"), 0o644))

	// AppA and AppB trash their files
	entryA, err := appA.Trash("/apps/data/appA/fileA.txt")
	if err != nil {
		t.Fatalf("appA trash: %v", err)
	}
	entryB, err := appB.Trash("/apps/data/appB/fileB.txt")
	if err != nil {
		t.Fatalf("appB trash: %v", err)
	}

	// AppA list trash: only sees entryA
	listA, err := appA.ListTrash()
	if err != nil {
		t.Fatalf("appA list trash: %v", err)
	}
	if len(listA) != 1 || listA[0].ID != entryA.ID {
		t.Fatalf("appA expected [entryA], got %v", listA)
	}

	// AppB list trash: only sees entryB
	listB, err := appB.ListTrash()
	if err != nil {
		t.Fatalf("appB list trash: %v", err)
	}
	if len(listB) != 1 || listB[0].ID != entryB.ID {
		t.Fatalf("appB expected [entryB], got %v", listB)
	}

	// AppA cannot restore AppB's trash item
	_, err = appA.Restore(entryB.ID, "", false)
	if err == nil {
		t.Fatal("expected appA restore of appB item to fail")
	}

	// AppA emptying trash only removes entryA
	count, err := appA.EmptyTrash()
	if err != nil || count != 1 {
		t.Fatalf("appA empty trash: count=%d, err=%v", count, err)
	}

	// AppB's trash item remains intact
	listBAfter, err := appB.ListTrash()
	if err != nil || len(listBAfter) != 1 || listBAfter[0].ID != entryB.ID {
		t.Fatalf("appB item should still exist after appA emptied trash, got %v", listBAfter)
	}
}

// A non-recursive grant on a directory authorizes the directory entry only.
// Bulk operations that read or mutate the entire subtree — copy, move,
// rename, remove, removeall, trash, restore — must fail unless a recursive
// grant (or app-private/shared coverage) authorizes every descendant.
func TestScopedGrants_BulkOpsRequireRecursiveCoverage(t *testing.T) {
	v, _ := setupTestVFS(t)
	must(t, v.MkdirAll("/shared/tree/sub"))
	must(t, v.WriteFile("/shared/tree/child.txt", []byte("child"), 0o644))
	must(t, v.WriteFile("/shared/tree/sub/inner.txt", []byte("inner"), 0o644))
	must(t, v.MkdirAll("/shared/dst"))
	must(t, v.MkdirAll("/shared/dst2"))

	appRO := v.ForApp("appRO")
	appRW := v.ForApp("appRW")
	appRW2 := v.ForApp("appRW2")

	// Read-only, non-recursive grant on the directory itself.
	if _, err := v.Grants().Issue("appRO", "/shared/tree", AccessRead, false); err != nil {
		t.Fatal(err)
	}
	// Read-write, non-recursive grant for the mutating ops, plus a
	// non-recursive destination dir and a recursive one.
	if _, err := v.Grants().Issue("appRW", "/shared/tree", AccessReadWrite, false); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Grants().Issue("appRW", "/shared/dst", AccessReadWrite, false); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Grants().Issue("appRW", "/shared/dst2", AccessReadWrite, true); err != nil {
		t.Fatal(err)
	}
	// appRW2 holds the recursive source coverage for destination checks.
	if _, err := v.Grants().Issue("appRW2", "/shared/tree", AccessReadWrite, true); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Grants().Issue("appRW2", "/shared/dst", AccessReadWrite, false); err != nil {
		t.Fatal(err)
	}

	// The granted directory itself is listable ...
	if _, err := appRO.ReadDir("/shared/tree"); err != nil {
		t.Fatalf("readdir on granted directory: %v", err)
	}
	// ... but descendants are not covered by the grant.
	if _, err := appRO.ReadFile("/shared/tree/child.txt"); err == nil {
		t.Fatal("read of uncovered descendant succeeded under non-recursive grant")
	}

	// Copy would exfiltrate the whole subtree into private storage.
	err := appRO.Copy("/shared/tree", "/apps/data/appRO/tree-copy", true)
	if err == nil {
		t.Fatal("copy of subtree under non-recursive grant succeeded")
	}
	var vErr *Error
	if !errors.As(err, &vErr) || vErr.Code != ErrPermission {
		t.Fatalf("copy subtree: expected ErrPermission, got %v", err)
	}

	// Mutating bulk ops on the subtree must all be denied.
	if err := appRW.Move("/shared/tree", "/apps/data/appRW/tree-moved", true); err == nil {
		t.Fatal("move of subtree under non-recursive grant succeeded")
	}
	if err := appRW.Remove("/shared/tree"); err == nil {
		t.Fatal("remove of subtree under non-recursive grant succeeded")
	}
	if err := appRW.RemoveAll("/shared/tree"); err == nil {
		t.Fatal("removeall of subtree under non-recursive grant succeeded")
	}
	if _, err := appRW.Trash("/shared/tree"); err == nil {
		t.Fatal("trash of subtree under non-recursive grant succeeded")
	}

	// A directory rename under only a non-recursive grant must be denied
	// even when the destination is covered by a recursive grant.
	if err := appRW.Rename("/shared/tree", "/shared/dst2/tree"); err == nil {
		t.Fatal("rename of subtree under non-recursive grant succeeded")
	}

	// A directory destination must be covered recursively too: bulk ops
	// create descendants beneath it, and a non-recursive grant on the
	// destination authorizes none of them — even with a recursively
	// granted source.
	if err := appRW2.Rename("/shared/tree", "/shared/dst"); err == nil {
		t.Fatal("rename into non-recursively-granted directory succeeded")
	}
	if err := appRW2.Copy("/shared/tree", "/shared/dst", true); err == nil {
		t.Fatal("copy into non-recursively-granted directory succeeded")
	}
	if err := appRW2.Move("/shared/tree", "/shared/dst", true); err == nil {
		t.Fatal("move into non-recursively-granted directory succeeded")
	}

	// The subtree must be untouched.
	if _, err := v.Stat("/shared/tree/sub/inner.txt"); err != nil {
		t.Fatalf("subtree was modified by denied operations: %v", err)
	}
	if _, err := v.Stat("/shared/tree"); err != nil {
		t.Fatalf("granted directory was removed by denied operations: %v", err)
	}

	// Recursive coverage on the source makes the same operations lawful.
	if err := appRW2.Copy("/shared/tree", "/apps/data/appRW2/tree-copy", true); err != nil {
		t.Fatalf("copy under recursive grant: %v", err)
	}
	data, err := appRW2.ReadFile("/apps/data/appRW2/tree-copy/sub/inner.txt")
	if err != nil || string(data) != "inner" {
		t.Fatalf("copied subtree unreadable: %v, %q", err, data)
	}
	if _, err := appRW.Trash("/shared/dst2"); err != nil {
		t.Fatalf("trash under recursive grant: %v", err)
	}
	if err := appRW2.RemoveAll("/shared/tree"); err != nil {
		t.Fatalf("removeall under recursive grant: %v", err)
	}
	if _, err := v.Stat("/shared/tree"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("recursive removeall left the directory behind")
	}

	// Single-file operands still work under non-recursive grants: a file
	// has no descendants, so the exact-path check is the whole story.
	must(t, v.WriteFile("/shared/flat.txt", []byte("flat"), 0o644))
	if _, err := v.Grants().Issue("appRO", "/shared/flat.txt", AccessRead, false); err != nil {
		t.Fatal(err)
	}
	if err := appRO.Copy("/shared/flat.txt", "/apps/data/appRO/flat.txt", false); err != nil {
		t.Fatalf("file copy under non-recursive grant: %v", err)
	}
}

// #268: re-issuing a session grant for the same (app, path, mode, scope)
// reuses the live grant instead of accumulating a duplicate per handoff.
// Revocation reaps grants out of the live set into a bounded tombstone
// set, so neither map grows with usage.
func TestScopedGrants_SessionGrantDedupAndReaping(t *testing.T) {
	gs := NewGrantStore()

	g1, err := gs.IssueSession("appA", "/shared/notes.txt", AccessRead, false)
	if err != nil {
		t.Fatal(err)
	}
	g2, err := gs.IssueSession("appA", "/shared/notes.txt", AccessRead, false)
	if err != nil {
		t.Fatal(err)
	}
	if g2.ID != g1.ID {
		t.Fatalf("re-issue minted duplicate grant %q, want reuse of %q", g2.ID, g1.ID)
	}
	if got := len(gs.List("appA")); got != 1 {
		t.Fatalf("expected 1 live grant after re-issue, got %d", got)
	}

	// A different mode or scope is a different grant.
	g3, err := gs.IssueSession("appA", "/shared/notes.txt", AccessReadWrite, false)
	if err != nil {
		t.Fatal(err)
	}
	if g3.ID == g1.ID {
		t.Fatal("read-write re-issue reused the read grant")
	}
	g4, err := gs.IssueSession("appA", "/shared/notes.txt", AccessRead, true)
	if err != nil {
		t.Fatal(err)
	}
	if g4.ID == g1.ID {
		t.Fatal("recursive re-issue reused the non-recursive grant")
	}

	// IssueSessionOnce reports whether the grant was newly minted.
	g5, created, err := gs.IssueSessionOnce("appA", "/shared/notes.txt", AccessRead, false)
	if err != nil {
		t.Fatal(err)
	}
	if created || g5.ID != g1.ID {
		t.Fatalf("IssueSessionOnce = (%v, %v), want reuse of %q", g5.ID, created, g1.ID)
	}

	// Session teardown reaps every session grant out of the live set.
	gs.RevokeAppSession("appA")
	if n := len(gs.grants); n != 0 {
		t.Fatalf("live grant set holds %d reaped session grants", n)
	}
	if _, ok := gs.Get(g1.ID); !ok {
		t.Fatal("reaped session grant has no tombstone")
	}
	if err := gs.CheckAccess("appA", "/shared/notes.txt", AccessRead); err == nil {
		t.Fatal("reaped session grant still authorizes access")
	}

	// A post-session handoff mints a fresh grant; tombstones never dedup.
	g6, err := gs.IssueSession("appA", "/shared/notes.txt", AccessRead, false)
	if err != nil {
		t.Fatal(err)
	}
	if g6.ID == g1.ID {
		t.Fatal("post-session issue reused a reaped grant")
	}

	// The tombstone set is bounded: revoking more than the limit keeps the
	// store small no matter how many grants churned through it.
	for i := 0; i < revokedGrantLimit+50; i++ {
		g, err := gs.Issue("appB", fmt.Sprintf("/shared/doc-%d.txt", i), AccessRead, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := gs.Revoke(g.ID); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(gs.revoked); n > revokedGrantLimit {
		t.Fatalf("tombstone set holds %d grants, want <= %d", n, revokedGrantLimit)
	}
	if n := len(gs.List("appB")); n > revokedGrantLimit {
		t.Fatalf("grant list holds %d entries, want <= %d", n, revokedGrantLimit)
	}
}
