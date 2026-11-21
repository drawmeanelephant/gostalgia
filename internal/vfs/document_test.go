package vfs

import (
	"errors"
	"testing"
)

func TestHostFSDocumentOps(t *testing.T) {
	h := newTestHostFS(t)
	defer func() { _ = h.Close() }()

	// 1. SaveAtomic new file
	err := h.SaveAtomic("doc.txt", []byte("hello world"), 0o644)
	if err != nil {
		t.Fatalf("SaveAtomic failed: %v", err)
	}
	data, err := h.ReadFile("doc.txt")
	if err != nil || string(data) != "hello world" {
		t.Fatalf("ReadFile after SaveAtomic = %q, %v", string(data), err)
	}

	// 2. SaveAtomic replace existing file
	err = h.SaveAtomic("doc.txt", []byte("updated content"), 0o644)
	if err != nil {
		t.Fatalf("SaveAtomic update failed: %v", err)
	}
	data, err = h.ReadFile("doc.txt")
	if err != nil || string(data) != "updated content" {
		t.Fatalf("ReadFile after update = %q, %v", string(data), err)
	}

	// 3. Rename file
	err = h.Rename("doc.txt", "doc_renamed.txt")
	if err != nil {
		t.Fatalf("Rename failed: %v", err)
	}
	if _, err := h.Stat("doc.txt"); err == nil {
		t.Fatal("expected old file to be gone after rename")
	}
	data, err = h.ReadFile("doc_renamed.txt")
	if err != nil || string(data) != "updated content" {
		t.Fatalf("ReadFile after rename = %q, %v", string(data), err)
	}

	// 4. Directory operations and RemoveAll
	err = h.MkdirAll("nested/sub/dir")
	if err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}
	err = h.SaveAtomic("nested/sub/dir/child.txt", []byte("child"), 0o644)
	if err != nil {
		t.Fatalf("SaveAtomic in subdir failed: %v", err)
	}
	err = h.Rename("nested/sub", "nested/sub_moved")
	if err != nil {
		t.Fatalf("Rename dir failed: %v", err)
	}
	data, err = h.ReadFile("nested/sub_moved/dir/child.txt")
	if err != nil || string(data) != "child" {
		t.Fatalf("ReadFile in moved dir = %q, %v", string(data), err)
	}

	err = h.RemoveAll("nested")
	if err != nil {
		t.Fatalf("RemoveAll failed: %v", err)
	}
	if _, err := h.Stat("nested"); err == nil {
		t.Fatal("nested dir still exists after RemoveAll")
	}

	// 5. Failure injection: stage hook error (original content preserved, no leak)
	err = h.SaveAtomic("safe.txt", []byte("safe data"), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	h.SetSaveHooks(func(stageName string) error {
		return errors.New("simulated disk full during staging")
	}, nil)

	err = h.SaveAtomic("safe.txt", []byte("corrupted?"), 0o644)
	if err == nil {
		t.Fatal("expected error from stage hook")
	}
	data, err = h.ReadFile("safe.txt")
	if err != nil || string(data) != "safe data" {
		t.Fatalf("original content corrupted after failed stage: %q", string(data))
	}

	// 6. Failure injection: rename hook error (produces recoverable artifact)
	h.SetSaveHooks(nil, func(stageName, targetName string) error {
		return errors.New("simulated crash during atomic rename")
	})

	err = h.SaveAtomic("safe.txt", []byte("recovery data"), 0o644)
	if err == nil {
		t.Fatal("expected error from rename hook")
	}
	var vfsErr *Error
	if !errors.As(err, &vfsErr) {
		t.Fatalf("expected *vfs.Error, got %T: %v", err, err)
	}
	if vfsErr.RecoverPath == "" {
		t.Fatal("expected RecoverPath in error")
	}

	// Original safe.txt should still have old content
	data, err = h.ReadFile("safe.txt")
	if err != nil || string(data) != "safe data" {
		t.Fatalf("original file altered after rename failure: %q", string(data))
	}
	// Recover file should contain new content
	recoverRel, err := Normalize(vfsErr.RecoverPath)
	if err != nil {
		t.Fatalf("Normalize(RecoverPath) = %v", err)
	}
	recData, err := h.ReadFile(recoverRel)
	if err != nil || string(recData) != "recovery data" {
		t.Fatalf("recover artifact missing or incorrect: %q, %v", string(recData), err)
	}

	// Clear hooks
	h.SetSaveHooks(nil, nil)
}

func TestMemFSDocumentOps(t *testing.T) {
	m := NewMem()

	// 1. SaveAtomic
	err := m.SaveAtomic("doc.txt", []byte("mem content"), 0o644)
	if err != nil {
		t.Fatalf("SaveAtomic failed: %v", err)
	}
	data, err := m.ReadFile("doc.txt")
	if err != nil || string(data) != "mem content" {
		t.Fatalf("ReadFile = %q, %v", string(data), err)
	}

	// 2. SaveAtomic overwrite
	err = m.SaveAtomic("doc.txt", []byte("updated mem"), 0o644)
	if err != nil {
		t.Fatalf("SaveAtomic overwrite failed: %v", err)
	}
	data, err = m.ReadFile("doc.txt")
	if err != nil || string(data) != "updated mem" {
		t.Fatalf("ReadFile after overwrite = %q, %v", string(data), err)
	}

	// 3. Rename file
	err = m.Rename("doc.txt", "doc_renamed.txt")
	if err != nil {
		t.Fatalf("Rename failed: %v", err)
	}
	if _, err := m.Stat("doc.txt"); err == nil {
		t.Fatal("expected old doc.txt to be gone")
	}
	data, err = m.ReadFile("doc_renamed.txt")
	if err != nil || string(data) != "updated mem" {
		t.Fatalf("ReadFile after rename = %q, %v", string(data), err)
	}

	// 4. Directory rename with children
	err = m.MkdirAll("dir1/dir2")
	if err != nil {
		t.Fatal(err)
	}
	err = m.SaveAtomic("dir1/dir2/file.txt", []byte("deep file"), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	err = m.Rename("dir1", "moved_dir")
	if err != nil {
		t.Fatalf("Rename dir failed: %v", err)
	}
	data, err = m.ReadFile("moved_dir/dir2/file.txt")
	if err != nil || string(data) != "deep file" {
		t.Fatalf("ReadFile after dir rename = %q, %v", string(data), err)
	}
	if _, err := m.Stat("dir1"); err == nil {
		t.Fatal("expected dir1 to be gone")
	}

	// 5. RemoveAll
	err = m.RemoveAll("moved_dir")
	if err != nil {
		t.Fatalf("RemoveAll failed: %v", err)
	}
	if _, err := m.Stat("moved_dir"); err == nil {
		t.Fatal("expected moved_dir to be removed")
	}

	// 6. Hooks: stage hook failure
	err = m.SaveAtomic("safe.txt", []byte("safe mem"), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	m.SetSaveHooks(func(stageName string) error {
		return errors.New("simulated mem stage error")
	}, nil)

	err = m.SaveAtomic("safe.txt", []byte("bad mem"), 0o644)
	if err == nil {
		t.Fatal("expected stage hook error")
	}
	data, err = m.ReadFile("safe.txt")
	if err != nil || string(data) != "safe mem" {
		t.Fatalf("safe.txt modified on stage failure: %q", string(data))
	}

	// 7. Hooks: rename hook failure produces recover artifact
	m.SetSaveHooks(nil, func(stageName, targetName string) error {
		return errors.New("simulated mem rename error")
	})
	err = m.SaveAtomic("safe.txt", []byte("recovery mem"), 0o644)
	if err == nil {
		t.Fatal("expected rename hook error")
	}
	var vfsErr *Error
	if !errors.As(err, &vfsErr) {
		t.Fatalf("expected *vfs.Error, got %v", err)
	}
	if vfsErr.RecoverPath == "" {
		t.Fatal("expected RecoverPath")
	}
	recRel, err := Normalize(vfsErr.RecoverPath)
	if err != nil {
		t.Fatalf("Normalize(RecoverPath) = %v", err)
	}
	recData, err := m.ReadFile(recRel)
	if err != nil || string(recData) != "recovery mem" {
		t.Fatalf("recover artifact data = %q, %v", string(recData), err)
	}
}

func TestVFSDocumentOps(t *testing.T) {
	hostFS := newTestHostFS(t)
	defer func() { _ = hostFS.Close() }()

	v := New(hostFS)
	mem := NewMem()
	must(t, v.Mount("/tmp", mem))

	// Setup directories
	must(t, v.MkdirAll("/users/guest/docs"))

	// 1. SaveAtomic & SaveAtomicOpt
	must(t, v.SaveAtomic("/users/guest/docs/readme.txt", []byte("readme v1"), 0o644))
	err := v.SaveAtomicOpt("/users/guest/docs/readme.txt", []byte("readme v2"), 0o644, false)
	if err == nil {
		t.Fatal("expected error saving with overwrite=false on existing file")
	}
	if !IsExist(err) {
		t.Fatalf("expected ErrExist, got %v", err)
	}
	must(t, v.SaveAtomicOpt("/users/guest/docs/readme.txt", []byte("readme v2"), 0o644, true))
	data, err := v.ReadFile("/users/guest/docs/readme.txt")
	if err != nil || string(data) != "readme v2" {
		t.Fatalf("unexpected content: %q", string(data))
	}

	// 2. Copy file (same mount)
	must(t, v.Copy("/users/guest/docs/readme.txt", "/users/guest/docs/readme_copy.txt", false))
	dataCopy, err := v.ReadFile("/users/guest/docs/readme_copy.txt")
	if err != nil || string(dataCopy) != "readme v2" {
		t.Fatalf("unexpected copy content: %q", string(dataCopy))
	}

	// Copy overwrite=false failure
	err = v.Copy("/users/guest/docs/readme.txt", "/users/guest/docs/readme_copy.txt", false)
	if err == nil || !IsExist(err) {
		t.Fatalf("expected ErrExist on copy collision, got: %v", err)
	}

	// 3. Copy cross-mount (root -> /tmp)
	must(t, v.Copy("/users/guest/docs/readme.txt", "/tmp/readme_tmp.txt", false))
	tmpData, err := v.ReadFile("/tmp/readme_tmp.txt")
	if err != nil || string(tmpData) != "readme v2" {
		t.Fatalf("cross-mount copy failed: %q, %v", string(tmpData), err)
	}

	// 4. Copy directory recursive
	must(t, v.MkdirAll("/users/guest/folder/sub"))
	must(t, v.SaveAtomic("/users/guest/folder/sub/item.txt", []byte("item"), 0o644))
	must(t, v.Copy("/users/guest/folder", "/users/guest/folder_copy", false))
	itemData, err := v.ReadFile("/users/guest/folder_copy/sub/item.txt")
	if err != nil || string(itemData) != "item" {
		t.Fatalf("copied folder item missing: %v", err)
	}

	// Copy directory into self error
	err = v.Copy("/users/guest/folder", "/users/guest/folder/sub/nested", false)
	if err == nil {
		t.Fatal("expected error copying directory into self")
	}

	// 5. Move same-mount
	must(t, v.Move("/users/guest/docs/readme_copy.txt", "/users/guest/docs/readme_moved.txt", false))
	if _, err := v.Stat("/users/guest/docs/readme_copy.txt"); err == nil {
		t.Fatal("source still exists after Move")
	}
	movedData, err := v.ReadFile("/users/guest/docs/readme_moved.txt")
	if err != nil || string(movedData) != "readme v2" {
		t.Fatalf("moved file invalid: %q", string(movedData))
	}

	// Move cross-mount (/tmp -> root)
	must(t, v.Move("/tmp/readme_tmp.txt", "/users/guest/docs/from_tmp.txt", false))
	if _, err := v.Stat("/tmp/readme_tmp.txt"); err == nil {
		t.Fatal("tmp source still exists after cross-mount Move")
	}
	fromTmpData, err := v.ReadFile("/users/guest/docs/from_tmp.txt")
	if err != nil || string(fromTmpData) != "readme v2" {
		t.Fatalf("cross-mount moved file invalid: %q", string(fromTmpData))
	}

	// 6. Rename (same mount vs cross-mount)
	must(t, v.RenameOpt("/users/guest/docs/from_tmp.txt", "/users/guest/docs/from_tmp_renamed.txt", false))
	// Cross mount rename must fail with ErrCrossMount
	err = v.Rename("/users/guest/docs/from_tmp_renamed.txt", "/tmp/invalid_cross_rename.txt")
	if err == nil {
		t.Fatal("expected cross-mount rename to fail")
	}
	var vErr *Error
	if !errors.As(err, &vErr) || vErr.Code != ErrCrossMount {
		t.Fatalf("expected ErrCrossMount, got %v", err)
	}

	// Rename cannot rename mount point
	err = v.Rename("/tmp", "/tmp2")
	if err == nil {
		t.Fatal("expected error renaming mount point")
	}

	// 7. Trash & Restore lifecycle
	entry, err := v.Trash("/users/guest/docs/from_tmp_renamed.txt")
	if err != nil {
		t.Fatalf("Trash failed: %v", err)
	}
	if entry.Name != "from_tmp_renamed.txt" {
		t.Fatalf("unexpected trash entry name: %q", entry.Name)
	}
	if _, err := v.Stat("/users/guest/docs/from_tmp_renamed.txt"); err == nil {
		t.Fatal("original file still exists after Trash")
	}

	// List trash
	list, err := v.ListTrash()
	if err != nil {
		t.Fatalf("ListTrash failed: %v", err)
	}
	if len(list) == 0 {
		t.Fatal("expected trash list to contain item")
	}

	// Restore original
	restoredPath, err := v.Restore(entry.ID, "", false)
	if err != nil {
		t.Fatalf("Restore failed: %v", err)
	}
	if restoredPath != "/users/guest/docs/from_tmp_renamed.txt" {
		t.Fatalf("unexpected restored path: %q", restoredPath)
	}
	restoredData, err := v.ReadFile(restoredPath)
	if err != nil || string(restoredData) != "readme v2" {
		t.Fatalf("restored data mismatch: %q", string(restoredData))
	}

	// Trash directory
	dirEntry, err := v.Trash("/users/guest/folder_copy")
	if err != nil {
		t.Fatalf("Trash directory failed: %v", err)
	}
	if !dirEntry.IsDir {
		t.Fatal("expected trashed directory entry to have IsDir=true")
	}
	// Restore directory to new destination
	destDir := "/users/guest/folder_restored"
	_, err = v.Restore(dirEntry.ID, destDir, false)
	if err != nil {
		t.Fatalf("Restore directory failed: %v", err)
	}
	restoredChildData, err := v.ReadFile(destDir + "/sub/item.txt")
	if err != nil || string(restoredChildData) != "item" {
		t.Fatalf("restored directory content missing: %v", err)
	}

	// EmptyTrash & PurgeTrash
	e1, err := v.Trash(destDir)
	if err != nil {
		t.Fatal(err)
	}
	// Purge single
	err = v.PurgeTrash(e1.ID)
	if err != nil {
		t.Fatalf("PurgeTrash failed: %v", err)
	}
	// Add another and empty
	must(t, v.SaveAtomic("/users/guest/trash_me.txt", []byte("trash"), 0o644))
	_, err = v.Trash("/users/guest/trash_me.txt")
	if err != nil {
		t.Fatal(err)
	}
	count, err := v.EmptyTrash()
	if err != nil || count != 1 {
		t.Fatalf("EmptyTrash count = %d, err = %v", count, err)
	}
	remaining, err := v.ListTrash()
	if err != nil || len(remaining) != 0 {
		t.Fatalf("remaining trash after empty: %v", remaining)
	}

	// Trash rejects root and mount points
	if _, err := v.Trash("/"); err == nil {
		t.Fatal("expected Trash(/) to fail")
	}
	if _, err := v.Trash("/tmp"); err == nil {
		t.Fatal("expected Trash(/tmp) to fail")
	}
}

func TestConfinementAndPathEscapes(t *testing.T) {
	h := newTestHostFS(t)
	defer h.Close()
	v := New(h)

	escapePaths := []string{
		"../outside",
		"/../../outside",
		"a/../../b/../../../etc",
		"..\\windows",
		"/users/guest/../../../escaped",
	}

	for _, p := range escapePaths {
		if err := v.SaveAtomic(p, []byte("data"), 0o644); err == nil {
			t.Errorf("SaveAtomic(%q) should fail confinement", p)
		}
		if err := v.Rename(p, "/valid.txt"); err == nil {
			t.Errorf("Rename(%q) should fail confinement", p)
		}
		if err := v.Rename("/valid.txt", p); err == nil {
			t.Errorf("Rename destination (%q) should fail confinement", p)
		}
		if err := v.Copy(p, "/valid.txt", true); err == nil {
			t.Errorf("Copy source (%q) should fail confinement", p)
		}
		if err := v.Copy("/valid.txt", p, true); err == nil {
			t.Errorf("Copy dest (%q) should fail confinement", p)
		}
		if err := v.Move(p, "/valid.txt", true); err == nil {
			t.Errorf("Move source (%q) should fail confinement", p)
		}
		if err := v.Move("/valid.txt", p, true); err == nil {
			t.Errorf("Move dest (%q) should fail confinement", p)
		}
		if err := v.RemoveAll(p); err == nil {
			t.Errorf("RemoveAll(%q) should fail confinement", p)
		}
		if _, err := v.Trash(p); err == nil {
			t.Errorf("Trash(%q) should fail confinement", p)
		}
	}
}
