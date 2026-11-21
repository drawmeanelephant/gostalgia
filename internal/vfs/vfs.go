// Package vfs implements the environment's virtual filesystem. Paths are
// environment paths ("/users/guest/documents"); host paths never leak
// through the API. The root is backed by a host directory (confined with
// os.Root, so symlink and ".." escapes fail closed), and a mount table
// lets subtrees (memory filesystems, future remote filesystems) shadow it.
package vfs

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"sync/atomic"
	"time"
)

// FS is the environment filesystem interface: io/fs read semantics plus
// the writes the environment needs. Path arguments accept both
// environment form ("/users/guest") and io/fs form ("users/guest").
type FS interface {
	fs.FS
	fs.StatFS
	fs.ReadDirFS
	fs.ReadFileFS

	// MkdirAll creates the directory and any missing parents.
	MkdirAll(path string) error
	// WriteFile creates or truncates path, creating parents as needed.
	WriteFile(path string, data []byte, perm fs.FileMode) error
	// Remove deletes a file or an empty directory.
	Remove(path string) error
	// Rename renames oldName to newName within the same filesystem.
	Rename(oldName, newName string) error
	// RemoveAll deletes path and any children it contains.
	RemoveAll(path string) error
	// SaveAtomic writes data to path safely and atomically, ensuring that
	// interrupted or failed saves leave either the prior valid document or
	// a recoverable artifact, never silent corruption or truncated files.
	SaveAtomic(path string, data []byte, perm fs.FileMode) error
}

// DocumentFS defines higher-level document and trash operations supported by
// the virtual filesystem.
type DocumentFS interface {
	FS

	// Copy copies src to dst (file or directory).
	Copy(src, dst string, overwrite bool) error
	// Move moves src to dst, atomically within the same mount or safely across mounts.
	Move(src, dst string, overwrite bool) error
	// Trash moves path into the environment trash with metadata.
	Trash(path string) (*TrashEntry, error)
	// Restore restores an item from trash to its original path or dst.
	Restore(trashID, dst string, overwrite bool) (string, error)
	// ListTrash lists all items currently in the trash.
	ListTrash() ([]TrashEntry, error)
	// EmptyTrash deletes all items in the trash.
	EmptyTrash() (int, error)
	// PurgeTrash permanently removes a single item from trash.
	PurgeTrash(trashID string) error
}

// MaxDocumentSize bounds single-document reads/writes in the VFS layer (16 MiB).
const MaxDocumentSize = 16 << 20

// BoundedSaver is implemented by filesystems that can stage an atomic save
// above MaxDocumentSize up to an explicit per-call limit. Bulk data paths
// such as backup/restore are bounded by their own documented ceilings, not
// by the interactive document cap.
type BoundedSaver interface {
	SaveAtomicBounded(name string, data []byte, perm fs.FileMode, limit int64) error
}

// SaveAtomicBounded saves data atomically, enforcing limit instead of the
// interactive MaxDocumentSize cap. Backends that implement BoundedSaver keep
// their full stage/fsync/rename behavior; other filesystems keep the
// atomicity contract through a stage-and-rename sequence built from the
// portable FS primitives.
func SaveAtomicBounded(fsys FS, name string, data []byte, perm fs.FileMode, limit int64) error {
	if int64(len(data)) > limit {
		return &Error{
			Op:      "save",
			Path:    name,
			Code:    ErrTooLarge,
			Message: fmt.Sprintf("document size %d exceeds limit of %d bytes", len(data), limit),
		}
	}
	if bs, ok := fsys.(BoundedSaver); ok {
		return bs.SaveAtomicBounded(name, data, perm, limit)
	}
	if int64(len(data)) <= MaxDocumentSize {
		return fsys.SaveAtomic(name, data, perm)
	}
	// Generic fallback for filesystems without native large-save support:
	// stage beside the target, then rename over it.
	stage := fmt.Sprintf("%s.tmp.%d.%d", name, time.Now().UnixNano(), boundedSaveSeq.Add(1))
	if err := fsys.WriteFile(stage, data, perm); err != nil {
		return err
	}
	var renameErr error
	if ro, ok := fsys.(interface {
		RenameOpt(oldPath, newPath string, overwrite bool) error
	}); ok {
		renameErr = ro.RenameOpt(stage, name, true)
	} else {
		renameErr = fsys.Rename(stage, name)
	}
	if renameErr != nil {
		recoverName := name + ".recover"
		if ro, ok := fsys.(interface {
			RenameOpt(oldPath, newPath string, overwrite bool) error
		}); ok {
			if rErr := ro.RenameOpt(stage, recoverName, true); rErr != nil {
				recoverName = stage
			}
		} else if rErr := fsys.Rename(stage, recoverName); rErr != nil {
			recoverName = stage
		}
		return &Error{
			Op:          "save",
			Path:        name,
			RecoverPath: recoverName,
			Code:        ErrIO,
			Err:         renameErr,
			Message:     fmt.Sprintf("atomic replacement failed: %v", renameErr),
		}
	}
	return nil
}

var boundedSaveSeq atomic.Uint64

// MaxIPCReadLimit bounds default read sizes over IPC (4 MiB).
const MaxIPCReadLimit = 4 << 20

// MaxIPCWriteLimit bounds maximum payload sizes over IPC (4 MiB).
const MaxIPCWriteLimit = 4 << 20

// ErrorCode categorizes filesystem errors for dialogs and caller handling.
type ErrorCode string

const (
	ErrNotFound   ErrorCode = "not_found"
	ErrExist      ErrorCode = "already_exists"
	ErrIsDir      ErrorCode = "is_directory"
	ErrNotDir     ErrorCode = "not_directory"
	ErrCrossMount ErrorCode = "cross_mount"
	ErrEscape     ErrorCode = "path_escape"
	ErrPermission ErrorCode = "permission_denied"
	ErrReadOnly   ErrorCode = "read_only"
	ErrTooLarge   ErrorCode = "too_large"
	ErrInvalid    ErrorCode = "invalid"
	ErrIO         ErrorCode = "io"
)

// Error represents a structured filesystem error suitable for dialogs.
type Error struct {
	Op          string    `json:"op"`
	Path        string    `json:"path"`
	Dest        string    `json:"dest,omitempty"`
	RecoverPath string    `json:"recover_path,omitempty"`
	Code        ErrorCode `json:"code"`
	Err         error     `json:"-"`
	Message     string    `json:"message"`
}

func (e *Error) Error() string {
	var target string
	if e.Dest != "" {
		target = fmt.Sprintf(" -> %s", e.Dest)
	}
	var rec string
	if e.RecoverPath != "" {
		rec = fmt.Sprintf(" (recoverable artifact preserved at %s)", e.RecoverPath)
	}
	return fmt.Sprintf("vfs: %s %s%s: %s%s", e.Op, e.Path, target, e.Message, rec)
}

func (e *Error) Unwrap() error {
	if e.Err != nil {
		return e.Err
	}
	switch e.Code {
	case ErrNotFound:
		return fs.ErrNotExist
	case ErrExist:
		return fs.ErrExist
	case ErrPermission, ErrReadOnly:
		return fs.ErrPermission
	case ErrInvalid, ErrEscape, ErrCrossMount, ErrIsDir, ErrNotDir:
		return fs.ErrInvalid
	default:
		return nil
	}
}

// TrashEntry records an item in the environment trash and its restore metadata.
type TrashEntry struct {
	ID           string    `json:"id"`
	OriginalPath string    `json:"original_path"`
	TrashPath    string    `json:"trash_path"`
	Name         string    `json:"name"`
	IsDir        bool      `json:"is_dir"`
	Size         int64     `json:"size"`
	TrashedAt    time.Time `json:"trashed_at"`
}

// Normalize converts an environment path ("/users/guest") to io/fs form
// ("users/guest"). Environment paths may carry a leading slash, but dot
// segments, backslashes, and interior empty elements are rejected outright
// — a path may never leave or obfuscate its position in the tree.
func Normalize(path string) (string, error) {
	if strings.ContainsRune(path, '\\') {
		return "", fmt.Errorf("vfs: path %q contains a backslash; use '/' separators", path)
	}
	if path == "." {
		return ".", nil // the root, already in fs form
	}
	segs := strings.Split(path, "/")
	out := make([]string, 0, len(segs))
	for i, seg := range segs {
		switch {
		case seg == "":
			if i == 0 || i == len(segs)-1 {
				continue // leading or trailing slash
			}
			return "", fmt.Errorf("vfs: invalid path %q", path)
		case seg == "." || seg == "..":
			return "", fmt.Errorf("vfs: invalid path %q: dot segments are not allowed", path)
		default:
			if strings.ContainsRune(seg, 0) {
				return "", fmt.Errorf("vfs: path %q contains a NUL byte", path)
			}
			out = append(out, seg)
		}
	}
	if len(out) == 0 {
		return ".", nil
	}
	return strings.Join(out, "/"), nil
}

// IsNotExist reports whether err is a not-exist error from any FS.
func IsNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }

// IsExist reports whether err is an already-exists error from any FS.
func IsExist(err error) bool { return errors.Is(err, fs.ErrExist) }
