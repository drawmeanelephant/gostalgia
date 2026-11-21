package recovery

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"gostalgia/internal/vfs"
)

// ExportOptions controls what data is gathered during backup creation.
type ExportOptions struct {
	ProfileID      string   // If set, only exports this profile; otherwise exports all profiles
	IncludeSystem  bool     // Whether to include /config/system.json (default true)
	ExcludeSecrets []string // Secret strings (e.g. operator tokens) that must not appear in any exported file
	SourceVersion  string   // Environment version string
	Description    string   // Optional user-supplied backup note
	// SystemConfig carries the live system preferences document
	// (host config/system.json) supplied by the caller. The system store
	// lives outside the VFS, so it cannot be gathered through vfs.FS.
	// When empty, export falls back to the VFS /config/system.json path
	// for compatibility with filesystems that materialize it there.
	SystemConfig []byte
}

// SkippedFile records a path that export observed but did not include in the
// archive, along with the reason it was omitted.
type SkippedFile struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// ExportResult contains summary metrics of a completed export.
type ExportResult struct {
	TotalFiles  int           `json:"total_files"`
	TotalBytes  int64         `json:"total_bytes"`
	SHA256      string        `json:"sha256"`
	CreatedAt   time.Time     `json:"created_at"`
	Profiles    []string      `json:"profiles"`
	ArchivePath string        `json:"archive_path,omitempty"`
	Skipped     []SkippedFile `json:"skipped,omitempty"`
}

// sanitizeWorkspace removes or redacts sensitive credentials from workspace.json content.
func sanitizeWorkspace(raw []byte) []byte {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw
	}
	// Redact or wipe command history to prevent secret leakage
	if hist, ok := m["history"].([]any); ok {
		cleanHist := make([]any, 0, len(hist))
		for _, item := range hist {
			if s, ok := item.(string); ok {
				lower := strings.ToLower(s)
				if strings.HasPrefix(lower, "auth") || strings.HasPrefix(lower, "token") || strings.HasPrefix(lower, "login") {
					continue
				}
				cleanHist = append(cleanHist, s)
			}
		}
		m["history"] = cleanHist
	}
	cleaned, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return raw
	}
	return cleaned
}

// Export gathers personal and system state from vfs and writes a verified .gbar ZIP archive to out.
func Export(ctx context.Context, vfsInstance vfs.FS, out io.Writer, opts ExportOptions) (*ExportResult, error) {
	if vfsInstance == nil {
		return nil, errors.New("recovery: VFS is required for export")
	}

	createdAt := time.Now().UTC()
	manifest := Manifest{
		FormatVersion: FormatVersion,
		CreatedAt:     createdAt,
		SourceVersion: opts.SourceVersion,
		Description:   opts.Description,
		Profiles:      []ProfileSummary{},
		Files:         []FileEntry{},
	}

	// Buffer payload files in memory during assembly to verify bounds before writing output.
	type payloadFile struct {
		entry   FileEntry
		content []byte
	}
	var payloads []payloadFile
	var totalBytes int64
	var skipped []SkippedFile

	// ParseArchive rejects entries whose names collide under case-folding,
	// so names that differ only by case must never reach the archive.
	seenFolded := map[string]string{strings.ToLower(ManifestFilename): ManifestFilename}

	recordSkip := func(vfsPath, reason string) {
		skipped = append(skipped, SkippedFile{Path: vfsPath, Reason: reason})
	}

	addFile := func(vfsPath string, content []byte, modTime time.Time) error {
		if !IsAllowedVFSPath(vfsPath) {
			recordSkip(vfsPath, "outside the portable backup scope")
			return nil
		}

		// Security scan: ensure no live credentials or configured secrets are present in content
		for _, secret := range opts.ExcludeSecrets {
			if secret != "" && bytes.Contains(content, []byte(secret)) {
				return fmt.Errorf("recovery: refusal: file %q contains sensitive credential or secret", vfsPath)
			}
		}

		cleanVFS := path.Clean(vfsPath)
		arcName := DataDirPrefix + strings.TrimPrefix(cleanVFS, "/")
		if !PortablePath(arcName) {
			return fmt.Errorf("recovery: nonportable archive path generated for %q", vfsPath)
		}
		if prev, dup := seenFolded[strings.ToLower(arcName)]; dup {
			recordSkip(cleanVFS, fmt.Sprintf("archive name collides with %q under case-insensitive comparison", prev))
			return nil
		}
		seenFolded[strings.ToLower(arcName)] = arcName

		size := int64(len(content))
		if size > MaxSingleFileBytes {
			return fmt.Errorf("recovery: file %q exceeds single file limit of %d bytes", vfsPath, MaxSingleFileBytes)
		}
		if totalBytes+size > MaxExtractBytes {
			return fmt.Errorf("recovery: total export size exceeds limit of %d bytes", MaxExtractBytes)
		}
		totalBytes += size

		entry := FileEntry{
			VFSPath:  cleanVFS,
			ArcName:  arcName,
			Size:     size,
			SHA256:   ComputeSHA256(content),
			Modified: modTime.UTC().Format(time.RFC3339),
		}
		payloads = append(payloads, payloadFile{entry: entry, content: content})
		return nil
	}

	// 1. Export profiles metadata if present
	profilesRaw, err := vfsInstance.ReadFile("/config/profiles.json")
	if err == nil {
		if err := addFile("/config/profiles.json", profilesRaw, time.Now()); err != nil {
			return nil, err
		}
		var profRegistry struct {
			Profiles []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"profiles"`
		}
		if err := json.Unmarshal(profilesRaw, &profRegistry); err == nil {
			for _, p := range profRegistry.Profiles {
				if opts.ProfileID == "" || opts.ProfileID == p.ID {
					manifest.Profiles = append(manifest.Profiles, ProfileSummary{ID: p.ID, Name: p.Name})
				}
			}
		}
	}

	// 2. Export system configuration if requested. The live system store is
	// supplied by the caller (it lives outside the VFS); the VFS path is a
	// legacy fallback only.
	if opts.IncludeSystem {
		systemCfg := opts.SystemConfig
		if len(systemCfg) == 0 {
			if raw, err := vfsInstance.ReadFile(SystemConfigVFSPath); err == nil {
				systemCfg = raw
			}
		}
		if len(systemCfg) > 0 {
			if err := addFile(SystemConfigVFSPath, systemCfg, time.Now()); err != nil {
				return nil, err
			}
		} else {
			recordSkip(SystemConfigVFSPath, "no system preferences store present")
		}
	}

	// Determine which profile directories to scan
	targetProfiles := make(map[string]bool)
	if opts.ProfileID != "" {
		targetProfiles[opts.ProfileID] = true
	} else if len(manifest.Profiles) > 0 {
		for _, p := range manifest.Profiles {
			targetProfiles[p.ID] = true
		}
	} else {
		// Fallback: discover user directories directly under /users
		entries, err := vfsInstance.ReadDir("/users")
		if err == nil {
			for _, ent := range entries {
				if ent.IsDir() && !strings.HasPrefix(ent.Name(), ".") {
					targetProfiles[ent.Name()] = true
					manifest.Profiles = append(manifest.Profiles, ProfileSummary{ID: ent.Name(), Name: ent.Name()})
				}
			}
		}
	}

	// Helper to recursively walk an approved directory
	var walkDir func(dirPath string) error
	walkDir = func(dirPath string) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		entries, err := vfsInstance.ReadDir(dirPath)
		if err != nil {
			if vfs.IsNotExist(err) {
				return nil // directory does not exist; nothing to include
			}
			// The directory exists but cannot be listed: record the omission
			// rather than silently dropping its whole subtree.
			recordSkip(dirPath, "unreadable directory: "+err.Error())
			return nil
		}
		for _, ent := range entries {
			childPath := path.Join(dirPath, ent.Name())
			if ent.IsDir() {
				// Skip trash and atomic-save staging folders
				if ent.Name() == ".trash" || strings.HasPrefix(ent.Name(), ".tmp") {
					recordSkip(childPath, "transient or trashed directory")
					continue
				}
				if err := walkDir(childPath); err != nil {
					return err
				}
			} else {
				// Skip only artifacts the environment itself owns: atomic-save
				// staging/recovery files and .gbar archives. Everything else —
				// including user .zip files — is ordinary data and is exported.
				lowerName := strings.ToLower(ent.Name())
				switch {
				case strings.Contains(ent.Name(), ".tmp."), strings.HasSuffix(ent.Name(), ".recover"):
					recordSkip(childPath, "atomic-save artifact")
					continue
				case strings.HasSuffix(lowerName, FileExtension):
					recordSkip(childPath, "backup archives are not re-exported")
					continue
				}
				content, err := vfsInstance.ReadFile(childPath)
				if err != nil {
					recordSkip(childPath, "unreadable file: "+err.Error())
					continue
				}

				info, _ := vfsInstance.Stat(childPath)
				modTime := time.Now()
				if info != nil {
					modTime = info.ModTime()
				}

				// If it's a workspace.json file, sanitize it
				if ent.Name() == "workspace.json" {
					content = sanitizeWorkspace(content)
				}

				if err := addFile(childPath, content, modTime); err != nil {
					return err
				}
			}
		}
		return nil
	}

	// 3. Scan user directories
	for profID := range targetProfiles {
		userRoot := path.Join("/users", profID)
		subdirs := []string{"documents", "downloads", "desktop", "config"}
		for _, sub := range subdirs {
			if err := walkDir(path.Join(userRoot, sub)); err != nil {
				return nil, err
			}
		}
	}

	// 3b. User-installed themes live under /config/themes; losing them on
	// every backup/restore cycle silently resets the theme catalog.
	if opts.IncludeSystem {
		if err := walkDir("/config/themes"); err != nil {
			return nil, err
		}
	}

	// /apps/data is app-private storage behind the app isolation boundary;
	// IsAllowedVFSPath excludes it and ParseArchive refuses /apps paths on
	// restore, so portable backups deliberately do not carry it. Record the
	// exclusion so a populated app-data tree is never silently absent.
	if entries, err := vfsInstance.ReadDir("/apps/data"); err == nil {
		if len(entries) > 0 {
			recordSkip("/apps/data", "app-private storage is excluded from portable backups")
		}
	} else if !vfs.IsNotExist(err) {
		recordSkip("/apps/data", "unreadable directory: "+err.Error())
	}

	// The archive also contains backup.json, so the payload count must
	// leave room for the manifest entry within MaxFiles.
	if len(payloads)+1 > MaxFiles {
		return nil, fmt.Errorf("recovery: total file count %d exceeds ceiling of %d", len(payloads), MaxFiles-1)
	}

	// Sort payloads deterministically by ArcName
	sort.Slice(payloads, func(i, j int) bool {
		return payloads[i].entry.ArcName < payloads[j].entry.ArcName
	})
	sort.Slice(skipped, func(i, j int) bool {
		return skipped[i].Path < skipped[j].Path
	})

	for _, p := range payloads {
		manifest.Files = append(manifest.Files, p.entry)
	}

	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("recovery: marshal manifest: %w", err)
	}
	if len(manifestBytes) > MaxManifestBytes {
		return nil, fmt.Errorf("recovery: manifest size %d exceeds limit of %d bytes (%d files)", len(manifestBytes), MaxManifestBytes, len(payloads))
	}

	// Write ZIP archive
	buf := new(bytes.Buffer)
	zw := zip.NewWriter(buf)

	writeEntry := func(name string, content []byte) error {
		fh := &zip.FileHeader{
			Name:               name,
			Method:             zip.Deflate,
			Modified:           createdAt,
			CRC32:              crc32.ChecksumIEEE(content),
			UncompressedSize64: uint64(len(content)),
		}
		var body []byte
		if len(content) == 0 {
			fh.Method = zip.Store
		} else {
			cb := new(bytes.Buffer)
			fw, err := flate.NewWriter(cb, flate.DefaultCompression)
			if err != nil {
				return fmt.Errorf("recovery: compress entry %q: %w", name, err)
			}
			if _, err := fw.Write(content); err != nil {
				_ = fw.Close()
				return fmt.Errorf("recovery: compress entry %q: %w", name, err)
			}
			if err := fw.Close(); err != nil {
				return fmt.Errorf("recovery: compress entry %q: %w", name, err)
			}
			body = cb.Bytes()
			// ParseArchive refuses entries whose deflate ratio exceeds
			// MaxCompressionRatio. Every payload is already content-addressed
			// by the manifest, so storing it raw is the faithful choice.
			if uint64(len(body))*MaxCompressionRatio < uint64(len(content)) {
				fh.Method = zip.Store
				body = content
			}
		}
		fh.CompressedSize64 = uint64(len(body))
		w, err := zw.CreateRaw(fh)
		if err != nil {
			return fmt.Errorf("recovery: write entry header %q: %w", name, err)
		}
		if _, err := w.Write(body); err != nil {
			return fmt.Errorf("recovery: write entry content %q: %w", name, err)
		}
		return nil
	}

	if err := writeEntry(ManifestFilename, manifestBytes); err != nil {
		return nil, err
	}
	for _, p := range payloads {
		if err := writeEntry(p.entry.ArcName, p.content); err != nil {
			return nil, err
		}
	}

	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("recovery: close archive writer: %w", err)
	}

	archiveBytes := buf.Bytes()
	if len(archiveBytes) > MaxArchiveBytes {
		return nil, fmt.Errorf("recovery: compressed archive size %d exceeds limit of %d", len(archiveBytes), MaxArchiveBytes)
	}

	// Report success only for an archive the environment's own parser
	// accepts: export and restore must enforce the same rule set.
	if _, err := ParseArchive(bytes.NewReader(archiveBytes)); err != nil {
		return nil, fmt.Errorf("recovery: export produced an archive its own parser rejects: %w", err)
	}

	if _, err := out.Write(archiveBytes); err != nil {
		return nil, fmt.Errorf("recovery: write archive output: %w", err)
	}

	profileNames := make([]string, 0, len(manifest.Profiles))
	for _, p := range manifest.Profiles {
		profileNames = append(profileNames, p.ID)
	}

	return &ExportResult{
		TotalFiles: len(manifest.Files),
		TotalBytes: totalBytes,
		SHA256:     ComputeSHA256(archiveBytes),
		CreatedAt:  createdAt,
		Profiles:   profileNames,
		Skipped:    skipped,
	}, nil
}

// ExportToVFS exports backup data atomically to a target file path on the VFS.
func ExportToVFS(ctx context.Context, vfsInstance vfs.FS, destPath string, opts ExportOptions) (*ExportResult, error) {
	destClean := path.Clean(destPath)
	if !strings.HasPrefix(destClean, "/") {
		destClean = "/" + destClean
	}
	if !strings.HasSuffix(strings.ToLower(destClean), FileExtension) && !strings.HasSuffix(strings.ToLower(destClean), ".zip") {
		destClean += FileExtension
	}

	// Ensure parent directory exists
	parent := path.Dir(destClean)
	if err := vfsInstance.MkdirAll(parent); err != nil {
		return nil, fmt.Errorf("recovery: create destination directory %q: %w", parent, err)
	}

	buf := new(bytes.Buffer)
	res, err := Export(ctx, vfsInstance, buf, opts)
	if err != nil {
		return nil, err
	}

	// Archives are bulk data bounded by MaxArchiveBytes, not by the
	// interactive document cap.
	if err := vfs.SaveAtomicBounded(vfsInstance, destClean, buf.Bytes(), 0o644, MaxArchiveBytes); err != nil {
		return nil, fmt.Errorf("recovery: save archive: %w", err)
	}

	res.ArchivePath = destClean
	return res, nil
}
