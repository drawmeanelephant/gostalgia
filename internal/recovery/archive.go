package recovery

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	"time"
)

const (
	// FormatVersion is the current archive specification version.
	FormatVersion = 1

	// FileExtension is the standard file extension for Gostalgia backup archives.
	FileExtension = ".gbar"

	// Bounds protecting against zip-bombs, memory exhaustion, and runaway extraction.
	MaxArchiveBytes     = 100 << 20 // 100 MiB compressed archive ceiling
	MaxExtractBytes     = 200 << 20 // 200 MiB uncompressed total ceiling
	MaxFiles            = 5000      // Maximum number of entries in an archive
	MaxCompressionRatio = 100       // Maximum permitted compression ratio per entry
	MaxManifestBytes    = 1 << 20   // 1 MiB maximum for backup.json metadata
	MaxSingleFileBytes  = 50 << 20  // 50 MiB single file uncompressed ceiling

	// ManifestFilename is the root metadata descriptor inside the archive.
	ManifestFilename = "backup.json"

	// DataDirPrefix is the root folder inside the archive holding payload files.
	DataDirPrefix = "data/"

	// SystemConfigVFSPath is the logical path the backup format uses for
	// the system preferences document. The live store is a host file
	// outside the VFS; restores route it explicitly.
	SystemConfigVFSPath = "/config/system.json"
)

// Manifest is the backup.json header inside every valid .gbar archive.
type Manifest struct {
	FormatVersion int              `json:"format_version"`
	CreatedAt     time.Time        `json:"created_at"`
	SourceVersion string           `json:"source_version,omitempty"`
	Description   string           `json:"description,omitempty"`
	Profiles      []ProfileSummary `json:"profiles,omitempty"`
	Files         []FileEntry      `json:"files"`
}

// ProfileSummary captures high-level profile identities included in the archive.
type ProfileSummary struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// FileEntry documents a single payload file in the archive.
type FileEntry struct {
	VFSPath  string `json:"vfs_path"` // Canonical VFS path starting with "/", e.g. "/users/guest/documents/notes.txt"
	ArcName  string `json:"arc_name"` // Archive relative path, e.g. "data/users/guest/documents/notes.txt"
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
	Modified string `json:"modified,omitempty"`
}

// ConflictStrategy dictates behavior when encountering existing destination files.
type ConflictStrategy string

const (
	// ConflictAbort fails immediately if any destination file already exists with differing content.
	ConflictAbort ConflictStrategy = "abort"
	// ConflictOverwrite overwrites conflicting files with archive contents.
	ConflictOverwrite ConflictStrategy = "overwrite"
	// ConflictSkip preserves existing files and only restores new files.
	ConflictSkip ConflictStrategy = "skip"
)

// ConflictEntry documents a collision between an archive file and an existing live file.
type ConflictEntry struct {
	VFSPath       string `json:"vfs_path"`
	ArchiveSize   int64  `json:"archive_size"`
	LiveSize      int64  `json:"live_size"`
	ArchiveSHA256 string `json:"archive_sha256"`
	LiveSHA256    string `json:"live_sha256"`
}

// PreviewReport details the anticipated actions of a restore operation without applying changes.
type PreviewReport struct {
	FormatVersion int              `json:"format_version"`
	CreatedAt     time.Time        `json:"created_at"`
	SourceVersion string           `json:"source_version,omitempty"`
	TotalFiles    int              `json:"total_files"`
	TotalBytes    int64            `json:"total_bytes"`
	Profiles      []ProfileSummary `json:"profiles,omitempty"`
	Create        []string         `json:"create"`    // Files that will be newly created
	Identical     []string         `json:"identical"` // Files already matching live content
	Conflicts     []ConflictEntry  `json:"conflicts"` // Files with collisions / differing content
}

// HasConflicts returns true if any conflicting files exist.
func (p *PreviewReport) HasConflicts() bool {
	return len(p.Conflicts) > 0
}

// RestoreReport summarizes the outcome of an applied restore.
type RestoreReport struct {
	RestoredCount int      `json:"restored_count"`
	SkippedCount  int      `json:"skipped_count"`
	RestoredBytes int64    `json:"restored_bytes"`
	Profiles      []string `json:"profiles"`
	// Reloaded names the live stores refreshed after the restore; the
	// service layer populates it.
	Reloaded []string `json:"reloaded,omitempty"`
}

// PortablePath checks whether a relative archive entry name is safe and non-traversing.
func PortablePath(name string) bool {
	if !fs.ValidPath(name) || name == "." || len(name) > 1024 ||
		strings.ContainsAny(name, "\\\x00:") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if strings.TrimRight(part, ". ") != part {
			return false
		}
		for _, r := range part {
			if r < 32 || strings.ContainsRune(`<>?"*|`, r) {
				return false
			}
		}
		base := strings.ToUpper(strings.SplitN(part, ".", 2)[0])
		if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" ||
			(len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '0' && base[3] <= '9') {
			return false
		}
	}
	return true
}

// IsAllowedVFSPath validates whether a path is in an approved user or system data tree.
// Prevents secret leakage (runtime.json, tokens) and unsupported host mounts.
func IsAllowedVFSPath(cleanPath string) bool {
	if !strings.HasPrefix(cleanPath, "/") {
		cleanPath = "/" + cleanPath
	}
	cleanPath = path.Clean(cleanPath)

	// Explicitly forbidden paths
	lower := strings.ToLower(cleanPath)
	if lower == "/runtime.json" || strings.HasPrefix(lower, "/runtime") {
		return false
	}
	if strings.HasPrefix(lower, "/logs") || strings.HasPrefix(lower, "/tmp") {
		return false
	}
	if strings.Contains(lower, "/.trash") || strings.Contains(lower, ".tmp.") || strings.HasSuffix(lower, ".recover") {
		return false
	}
	if strings.HasPrefix(lower, "/apps") {
		// App executables or private mounts are not portable backup data
		return false
	}

	// Permitted trees:
	// /config/system.json, /config/profiles.json, etc.
	if strings.HasPrefix(cleanPath, "/config/") {
		return true
	}
	// /users/<profile>/{documents,downloads,desktop,config}/...
	if strings.HasPrefix(cleanPath, "/users/") {
		parts := strings.Split(strings.TrimPrefix(cleanPath, "/"), "/")
		if len(parts) >= 2 {
			if len(parts) >= 3 {
				sub := parts[2]
				switch sub {
				case "documents", "downloads", "desktop", "config":
					return true
				default:
					return false
				}
			}
			return true
		}
	}

	return false
}

// CheckCentralDirectory pre-validates central directory records before full extraction.
// Defense against malformed ZIPs, multi-disk tricks, and ZIP64 bombs.
func CheckCentralDirectory(data []byte) error {
	end := -1
	for i := len(data) - 22; i >= 0 && i >= len(data)-22-65535; i-- {
		if binary.LittleEndian.Uint32(data[i:i+4]) == 0x06054b50 &&
			i+22+int(binary.LittleEndian.Uint16(data[i+20:i+22])) == len(data) {
			end = i
			break
		}
	}
	if end < 0 {
		return fmt.Errorf("recovery: missing ZIP end of central directory record")
	}
	if len(data) < 4 || binary.LittleEndian.Uint32(data[:4]) != 0x04034b50 {
		return fmt.Errorf("recovery: invalid ZIP local header")
	}
	h := data[end:]
	count := int(binary.LittleEndian.Uint16(h[10:12]))
	if count > MaxFiles || count < 1 || binary.LittleEndian.Uint16(h[8:10]) != uint16(count) ||
		binary.LittleEndian.Uint32(h[4:8]) != 0 {
		return fmt.Errorf("recovery: invalid or oversized ZIP directory")
	}
	start := uint64(binary.LittleEndian.Uint32(h[16:20]))
	size := uint64(binary.LittleEndian.Uint32(h[12:16]))
	if start+size != uint64(end) {
		return fmt.Errorf("recovery: invalid ZIP directory extent")
	}
	directory := data[int(start):end]
	for n := 0; len(directory) > 0; n++ {
		if n >= count || len(directory) < 46 || binary.LittleEndian.Uint32(directory[:4]) != 0x02014b50 {
			return fmt.Errorf("recovery: corrupt ZIP directory")
		}
		if binary.LittleEndian.Uint16(directory[34:36]) != 0 ||
			binary.LittleEndian.Uint16(directory[8:10])&1 != 0 {
			return fmt.Errorf("recovery: encrypted or multi-disk entry")
		}
		length := 46 + int(binary.LittleEndian.Uint16(directory[28:30])) +
			int(binary.LittleEndian.Uint16(directory[30:32])) + int(binary.LittleEndian.Uint16(directory[32:34]))
		if length > len(directory) {
			return fmt.Errorf("recovery: truncated ZIP directory")
		}
		directory = directory[length:]
		if len(directory) == 0 && n+1 != count {
			return fmt.Errorf("recovery: ZIP entry count mismatch")
		}
	}
	return nil
}

// ComputeSHA256 returns the hex-encoded SHA-256 hash of bytes.
func ComputeSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// VerifiedArchive holds safe in-memory parsed archive contents ready for inspection or restore.
type VerifiedArchive struct {
	Manifest Manifest
	Files    map[string][]byte // ArcName -> raw contents
}

// ParseArchive securely verifies and unpacks a backup archive from reader r into memory.
func ParseArchive(r io.Reader) (*VerifiedArchive, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxArchiveBytes+1))
	if err != nil {
		return nil, fmt.Errorf("recovery: read archive: %w", err)
	}
	if len(data) > MaxArchiveBytes {
		return nil, fmt.Errorf("recovery: archive size %d exceeds budget of %d bytes", len(data), MaxArchiveBytes)
	}
	if err := CheckCentralDirectory(data); err != nil {
		return nil, err
	}

	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("recovery: malformed ZIP archive: %w", err)
	}
	if len(zr.File) > MaxFiles {
		return nil, fmt.Errorf("recovery: entry count %d exceeds maximum of %d", len(zr.File), MaxFiles)
	}

	va := &VerifiedArchive{
		Files: make(map[string][]byte),
	}

	var manifestBytes []byte
	seen := make(map[string]bool)
	var totalUncompressed uint64

	for _, f := range zr.File {
		name := strings.TrimSuffix(f.Name, "/")
		if !PortablePath(name) || seen[strings.ToLower(name)] {
			return nil, fmt.Errorf("recovery: unsafe, traversing, or duplicate archive path %q", f.Name)
		}
		seen[strings.ToLower(name)] = true

		mode := f.Mode()
		if mode.Type() == fs.ModeDir {
			continue
		}
		if !mode.IsRegular() {
			return nil, fmt.Errorf("recovery: special files and symlinks are forbidden: %q", f.Name)
		}

		size := f.UncompressedSize64
		if size > MaxSingleFileBytes {
			return nil, fmt.Errorf("recovery: entry %q size %d exceeds single file limit of %d", name, size, MaxSingleFileBytes)
		}
		if totalUncompressed+size > MaxExtractBytes {
			return nil, fmt.Errorf("recovery: total extraction size exceeds limit of %d bytes", MaxExtractBytes)
		}
		if size > 0 && (f.CompressedSize64 == 0 || size > f.CompressedSize64*MaxCompressionRatio) {
			return nil, fmt.Errorf("recovery: compression ratio exceeds maximum permitted for %q", name)
		}
		totalUncompressed += size

		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("recovery: open entry %q: %w", name, err)
		}
		content, readErr := io.ReadAll(io.LimitReader(rc, int64(size)+1))
		closeErr := rc.Close()
		if readErr != nil || closeErr != nil || uint64(len(content)) != size {
			return nil, fmt.Errorf("recovery: corrupt entry payload %q", name)
		}

		if name == ManifestFilename {
			if size > MaxManifestBytes {
				return nil, fmt.Errorf("recovery: manifest exceeds maximum size")
			}
			manifestBytes = content
		} else {
			va.Files[name] = content
		}
	}

	if manifestBytes == nil {
		return nil, fmt.Errorf("recovery: archive missing required %s descriptor", ManifestFilename)
	}

	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(manifestBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("recovery: parse manifest: %w", err)
	}
	if m.FormatVersion != FormatVersion {
		return nil, fmt.Errorf("recovery: unsupported archive format version %d (expected %d)", m.FormatVersion, FormatVersion)
	}

	// Verify entries against manifest declarations and ensure no forbidden paths exist
	entryMap := make(map[string]FileEntry, len(m.Files))
	for _, entry := range m.Files {
		if !IsAllowedVFSPath(entry.VFSPath) {
			return nil, fmt.Errorf("recovery: manifest contains forbidden VFS path %q", entry.VFSPath)
		}
		if !strings.HasPrefix(entry.ArcName, DataDirPrefix) || !PortablePath(entry.ArcName) {
			return nil, fmt.Errorf("recovery: manifest contains invalid archive name %q", entry.ArcName)
		}
		expectedContent, exists := va.Files[entry.ArcName]
		if !exists {
			return nil, fmt.Errorf("recovery: manifest entry %q missing from archive payload", entry.ArcName)
		}
		if int64(len(expectedContent)) != entry.Size {
			return nil, fmt.Errorf("recovery: entry %q size mismatch (declared %d, got %d)", entry.ArcName, entry.Size, len(expectedContent))
		}
		if actualHash := ComputeSHA256(expectedContent); actualHash != entry.SHA256 {
			return nil, fmt.Errorf("recovery: entry %q checksum mismatch (declared %s, computed %s)", entry.ArcName, entry.SHA256, actualHash)
		}
		entryMap[entry.ArcName] = entry
	}

	// Ensure no payload files exist in the archive that are not in the manifest
	for arcName := range va.Files {
		if _, declared := entryMap[arcName]; !declared {
			return nil, fmt.Errorf("recovery: unmanifested file present in archive %q", arcName)
		}
	}

	va.Manifest = m
	return va, nil
}
