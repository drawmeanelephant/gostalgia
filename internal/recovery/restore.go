package recovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"gostalgia/internal/vfs"
)

// Inspect reads and validates a backup archive, returning its manifest descriptor.
func Inspect(r io.Reader) (*Manifest, error) {
	va, err := ParseArchive(r)
	if err != nil {
		return nil, err
	}
	return &va.Manifest, nil
}

// Preview compares an archive against the current VFS state without making any modifications.
func Preview(r io.Reader, vfsInstance vfs.FS) (*PreviewReport, error) {
	return PreviewOpt(r, vfsInstance, "")
}

// PreviewOpt is Preview with the host path of the live system preferences
// store, so the /config/system.json entry is compared against the document
// the environment actually reads.
func PreviewOpt(r io.Reader, vfsInstance vfs.FS, systemConfigPath string) (*PreviewReport, error) {
	va, err := ParseArchive(r)
	if err != nil {
		return nil, err
	}

	report := &PreviewReport{
		FormatVersion: va.Manifest.FormatVersion,
		CreatedAt:     va.Manifest.CreatedAt,
		SourceVersion: va.Manifest.SourceVersion,
		TotalFiles:    len(va.Manifest.Files),
		Profiles:      va.Manifest.Profiles,
		Create:        []string{},
		Identical:     []string{},
		Conflicts:     []ConflictEntry{},
	}

	for _, entry := range va.Manifest.Files {
		report.TotalBytes += entry.Size

		var liveData []byte
		if hostPath := systemHostPath(entry.VFSPath, systemConfigPath); hostPath != "" {
			liveData, err = os.ReadFile(hostPath)
			if errors.Is(err, fs.ErrNotExist) {
				err = fs.ErrNotExist
			}
		} else {
			liveData, err = vfsInstance.ReadFile(entry.VFSPath)
		}
		if err != nil {
			if vfs.IsNotExist(err) {
				// File does not exist live -> new file to be created
				report.Create = append(report.Create, entry.VFSPath)
				continue
			}
			// The file exists but cannot be read: its live state is unknown,
			// so a preview that claims "create" would be a lie.
			return nil, fmt.Errorf("recovery: cannot assess live file %q: %w", entry.VFSPath, err)
		}

		liveHash := ComputeSHA256(liveData)
		if liveHash == entry.SHA256 {
			// Content is identical
			report.Identical = append(report.Identical, entry.VFSPath)
		} else {
			// Content collision / differs
			report.Conflicts = append(report.Conflicts, ConflictEntry{
				VFSPath:       entry.VFSPath,
				ArchiveSize:   entry.Size,
				LiveSize:      int64(len(liveData)),
				ArchiveSHA256: entry.SHA256,
				LiveSHA256:    liveHash,
			})
		}
	}

	return report, nil
}

// journalAction captures an applied or pending transactional change.
type journalAction int

const (
	actionCreated journalAction = iota
	actionOverwritten
)

type journalEntry struct {
	vfsPath    string
	hostPath   string // non-empty: entry applies to the host system store
	action     journalAction
	backupData []byte
}

// RestoreOptions controls the behavior of a restore operation.
type RestoreOptions struct {
	Strategy      ConflictStrategy // Default is ConflictAbort
	ProfileFilter string           // Optional: only restore files matching this profile ID
	// SystemConfigPath is the host filesystem path of the live system
	// preferences store (root/config/system.json). When set, the archive's
	// /config/system.json entry is routed there instead of the VFS, where
	// nothing reads it.
	SystemConfigPath string
}

// systemHostPath maps the archive's system preferences entry onto the live
// host store path. All other paths return "".
func systemHostPath(vfsPath, systemConfigPath string) string {
	if systemConfigPath != "" && vfsPath == SystemConfigVFSPath {
		return systemConfigPath
	}
	return ""
}

// hostAtomicSave writes data to a host path via a staged rename so a crash
// never leaves a torn document.
func hostAtomicSave(hostPath string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(hostPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	stage, err := os.CreateTemp(dir, ".gbar-*.tmp")
	if err != nil {
		return err
	}
	stageName := stage.Name()
	if _, err := stage.Write(data); err != nil {
		_ = stage.Close()
		_ = os.Remove(stageName)
		return err
	}
	if err := stage.Sync(); err != nil {
		_ = stage.Close()
		_ = os.Remove(stageName)
		return err
	}
	if err := stage.Close(); err != nil {
		_ = os.Remove(stageName)
		return err
	}
	if err := os.Chmod(stageName, perm); err != nil {
		_ = os.Remove(stageName)
		return err
	}
	if err := os.Rename(stageName, hostPath); err != nil {
		_ = os.Remove(stageName)
		return err
	}
	return nil
}

// Restore applies an archive to the live VFS transactionally with verified rollback on failure.
func Restore(ctx context.Context, r io.Reader, vfsInstance vfs.FS, opts RestoreOptions) (*RestoreReport, error) {
	if vfsInstance == nil {
		return nil, fmt.Errorf("recovery: VFS is required for restore")
	}

	va, err := ParseArchive(r)
	if err != nil {
		return nil, err
	}

	// Default strategy is ConflictAbort
	if opts.Strategy == "" {
		opts.Strategy = ConflictAbort
	}

	// 1. Analyze conflicts and plan operations
	type plannedFile struct {
		vfsPath  string
		hostPath string // non-empty: write to the host system store, not the VFS
		arcName  string
		content  []byte
		size     int64
		exists   bool
	}

	var planned []plannedFile
	var conflicts []ConflictEntry
	var skippedCount int
	affectedProfiles := make(map[string]bool)

	for _, entry := range va.Manifest.Files {
		// Profile filter: a profile-scoped restore must only touch the
		// profile's own tree. Anything else — including global /config/*
		// state such as package-trust.json — is skipped.
		if opts.ProfileFilter != "" {
			profilePrefix := "/users/" + opts.ProfileFilter + "/"
			if !strings.HasPrefix(entry.VFSPath, profilePrefix) {
				skippedCount++
				continue
			}
		}

		// Track affected profiles
		if strings.HasPrefix(entry.VFSPath, "/users/") {
			parts := strings.Split(strings.TrimPrefix(entry.VFSPath, "/"), "/")
			if len(parts) >= 2 {
				affectedProfiles[parts[1]] = true
			}
		}

		content := va.Files[entry.ArcName]
		hostPath := systemHostPath(entry.VFSPath, opts.SystemConfigPath)
		var liveData []byte
		if hostPath != "" {
			liveData, err = os.ReadFile(hostPath)
			if errors.Is(err, fs.ErrNotExist) {
				err = fs.ErrNotExist
			}
		} else {
			liveData, err = vfsInstance.ReadFile(entry.VFSPath)
		}
		if err != nil {
			if !vfs.IsNotExist(err) {
				// The file exists but cannot be read: we cannot tell whether
				// it conflicts, and its content could not be journaled for
				// rollback. Refuse under every strategy rather than silently
				// overwriting unknown data.
				return nil, fmt.Errorf("recovery: cannot assess live file %q: %w", entry.VFSPath, err)
			}
			// Does not exist -> planned for creation
			planned = append(planned, plannedFile{
				vfsPath:  entry.VFSPath,
				hostPath: hostPath,
				arcName:  entry.ArcName,
				content:  content,
				size:     entry.Size,
				exists:   false,
			})
			continue
		}

		liveHash := ComputeSHA256(liveData)
		if liveHash == entry.SHA256 {
			// Identical content already live -> safe no-op or rewrite
			continue
		}

		// File exists with differing content -> collision
		conflict := ConflictEntry{
			VFSPath:       entry.VFSPath,
			ArchiveSize:   entry.Size,
			LiveSize:      int64(len(liveData)),
			ArchiveSHA256: entry.SHA256,
			LiveSHA256:    liveHash,
		}
		conflicts = append(conflicts, conflict)

		switch opts.Strategy {
		case ConflictAbort:
			// Handled below after collecting all conflicts
		case ConflictSkip:
			skippedCount++
			// File is skipped, not planned
		case ConflictOverwrite:
			planned = append(planned, plannedFile{
				vfsPath:  entry.VFSPath,
				hostPath: hostPath,
				arcName:  entry.ArcName,
				content:  content,
				size:     entry.Size,
				exists:   true,
			})
		default:
			return nil, fmt.Errorf("recovery: unknown conflict strategy %q", opts.Strategy)
		}
	}

	if len(conflicts) > 0 && opts.Strategy == ConflictAbort {
		return nil, fmt.Errorf("recovery: conflict refusal: %d file(s) differ from live data; explicit conflict decision required", len(conflicts))
	}

	if len(planned) == 0 {
		return &RestoreReport{
			RestoredCount: 0,
			SkippedCount:  skippedCount,
			RestoredBytes: 0,
			Profiles:      profileKeys(affectedProfiles),
		}, nil
	}

	// 2. Transactional Staging & Rollback Journal
	var journal []journalEntry

	rollback := func(triggerErr error) error {
		var rollbackErrors []string
		for i := len(journal) - 1; i >= 0; i-- {
			j := journal[i]
			switch j.action {
			case actionCreated:
				// Remove newly created file if it exists
				if j.hostPath != "" {
					if err := os.Remove(j.hostPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
						rollbackErrors = append(rollbackErrors, fmt.Sprintf("failed to remove created file %q: %v", j.hostPath, err))
					}
				} else if _, err := vfsInstance.Stat(j.vfsPath); err == nil {
					if err := vfsInstance.Remove(j.vfsPath); err != nil {
						rollbackErrors = append(rollbackErrors, fmt.Sprintf("failed to remove created file %q: %v", j.vfsPath, err))
					}
				}
			case actionOverwritten:
				// Restore original backup copy
				if j.hostPath != "" {
					if err := hostAtomicSave(j.hostPath, j.backupData, 0o644); err != nil {
						rollbackErrors = append(rollbackErrors, fmt.Sprintf("failed to restore overwritten file %q: %v", j.hostPath, err))
					}
				} else if err := vfsInstance.WriteFile(j.vfsPath, j.backupData, 0o644); err != nil {
					rollbackErrors = append(rollbackErrors, fmt.Sprintf("failed to restore overwritten file %q: %v", j.vfsPath, err))
				}
			}
		}
		if len(rollbackErrors) > 0 {
			return fmt.Errorf("%w (rollback incomplete: %s)", triggerErr, strings.Join(rollbackErrors, "; "))
		}
		return fmt.Errorf("%w (rolled back successfully)", triggerErr)
	}

	var restoredBytes int64
	for _, p := range planned {
		select {
		case <-ctx.Done():
			return nil, rollback(ctx.Err())
		default:
		}

		if p.hostPath != "" {
			if p.exists {
				orig, err := os.ReadFile(p.hostPath)
				if err != nil {
					return nil, rollback(fmt.Errorf("recovery: read original file for backup %q: %v", p.hostPath, err))
				}
				journal = append(journal, journalEntry{
					vfsPath:    p.vfsPath,
					hostPath:   p.hostPath,
					action:     actionOverwritten,
					backupData: orig,
				})
			}
			if err := hostAtomicSave(p.hostPath, p.content, 0o644); err != nil {
				return nil, rollback(fmt.Errorf("recovery: atomic save %q: %w", p.hostPath, err))
			}
			if !p.exists {
				journal = append(journal, journalEntry{
					vfsPath:  p.vfsPath,
					hostPath: p.hostPath,
					action:   actionCreated,
				})
			}
			restoredBytes += p.size
			continue
		}

		// Prepare destination directory
		parentDir := path.Dir(p.vfsPath)
		if err := vfsInstance.MkdirAll(parentDir); err != nil {
			return nil, rollback(fmt.Errorf("recovery: create directory %q: %w", parentDir, err))
		}

		if p.exists {
			// Read current content to journal for rollback
			orig, err := vfsInstance.ReadFile(p.vfsPath)
			if err != nil {
				return nil, rollback(fmt.Errorf("recovery: read original file for backup %q: %w", p.vfsPath, err))
			}
			journal = append(journal, journalEntry{
				vfsPath:    p.vfsPath,
				action:     actionOverwritten,
				backupData: orig,
			})
		}

		// Write new content atomically. Restored payloads are bounded by
		// MaxSingleFileBytes, not the interactive document cap.
		if err := vfs.SaveAtomicBounded(vfsInstance, p.vfsPath, p.content, 0o644, MaxSingleFileBytes); err != nil {
			return nil, rollback(fmt.Errorf("recovery: atomic save %q: %w", p.vfsPath, err))
		}

		if !p.exists {
			journal = append(journal, journalEntry{
				vfsPath: p.vfsPath,
				action:  actionCreated,
			})
		}

		restoredBytes += p.size
	}

	return &RestoreReport{
		RestoredCount: len(planned),
		SkippedCount:  skippedCount,
		RestoredBytes: restoredBytes,
		Profiles:      profileKeys(affectedProfiles),
	}, nil
}

func profileKeys(m map[string]bool) []string {
	res := make([]string, 0, len(m))
	for k := range m {
		res = append(res, k)
	}
	return res
}
