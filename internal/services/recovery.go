package services

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sync"
	"time"

	"gostalgia/internal/ipc"
	"gostalgia/internal/recovery"
	"gostalgia/internal/security"
	"gostalgia/internal/service"
	"gostalgia/internal/vfs"
)

// RecoveryService exposes backup export, inspection, preview, and restore over IPC.
// It owns the "backup/*" method namespace.
type RecoveryService struct {
	ctx *service.Context

	// exportMu serializes default-path exports so the same-second name
	// collision check cannot race a concurrent export into overwriting the
	// same destination.
	exportMu sync.Mutex
}

// NewRecovery creates a new RecoveryService.
func NewRecovery() *RecoveryService {
	return &RecoveryService{}
}

func (s *RecoveryService) Name() string { return "recovery" }

func (s *RecoveryService) Depends() []string {
	return []string{"fs"}
}

func (s *RecoveryService) Init(ctx *service.Context) error {
	s.ctx = ctx
	return nil
}

func (s *RecoveryService) Start(ctx context.Context) error {
	return s.ctx.Router.HandleRoutes(map[string]ipc.Route{
		"backup/export":  {Handler: s.exportBackup, Policy: ipc.OperatorOr(security.CapBackupWrite, security.CapAdmin)},
		"backup/inspect": {Handler: s.inspectBackup, Policy: ipc.OperatorOr(security.CapBackupRead, security.CapAdmin)},
		"backup/preview": {Handler: s.previewBackup, Policy: ipc.OperatorOr(security.CapBackupRead, security.CapAdmin)},
		"backup/restore": {Handler: s.restoreBackup, Policy: ipc.OperatorOr(security.CapBackupWrite, security.CapAdmin)},
	})
}

func (s *RecoveryService) Stop(ctx context.Context) error {
	s.ctx.Router.UnhandlePrefix("backup/")
	return nil
}

// Type aliases for IPC parameter payloads
type BackupExportParams = recovery.ExportParams
type BackupPathParams = recovery.PathParams
type BackupRestoreParams = recovery.RestoreParams

// callerVFS returns the filesystem view a backup operation may use for the
// calling principal. Operators and admin-capable callers see the whole
// environment filesystem; application principals get their grant-scoped view
// wrapped in the same per-path access rules fs/* applies, so a backup can
// never read, classify, or write a path the caller could not touch through
// the filesystem service itself.
func (s *RecoveryService) callerVFS(ctx context.Context) (vfs.FS, error) {
	principal := ipc.CallerPrincipal(ctx)
	caps := ipc.Capabilities(ctx)
	if principal.IsOperator() || (caps != nil && caps.Has(security.CapAdmin)) {
		return s.ctx.VFS, nil
	}
	if principal.IsApp() {
		v, ok := s.ctx.VFS.(*vfs.VFS)
		if !ok {
			return nil, &vfs.Error{Op: "access", Code: vfs.ErrPermission, Message: "permission denied: grant-scoped filesystem unavailable"}
		}
		return &scopedBackupFS{FS: v.ForApp(principal.AppID), sctx: s.ctx, ctx: ctx}, nil
	}
	return s.ctx.VFS, nil
}

// scopedBackupFS layers the shared fsAccessChecker rules over a grant-scoped
// VFS. ScopedVFS exempts shared host mounts from grant checks, so without
// this layer a backup could reach host-mounted folders without the hostfs
// capabilities fs/* would demand. Denials here degrade to whatever the
// embedded scoped view enforces — never to broader access.
type scopedBackupFS struct {
	vfs.FS
	sctx *service.Context
	ctx  context.Context
}

func (f *scopedBackupFS) checkRead(name string) error {
	return fsAccessChecker{f.sctx}.read(f.ctx, name)
}

func (f *scopedBackupFS) checkWrite(name string) error {
	return fsAccessChecker{f.sctx}.write(f.ctx, name)
}

func (f *scopedBackupFS) Open(name string) (fs.File, error) {
	if err := f.checkRead(name); err != nil {
		return nil, err
	}
	return f.FS.Open(name)
}

func (f *scopedBackupFS) Stat(name string) (fs.FileInfo, error) {
	if err := f.checkRead(name); err != nil {
		return nil, err
	}
	return f.FS.Stat(name)
}

func (f *scopedBackupFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if err := f.checkRead(name); err != nil {
		return nil, err
	}
	return f.FS.ReadDir(name)
}

func (f *scopedBackupFS) ReadFile(name string) ([]byte, error) {
	if err := f.checkRead(name); err != nil {
		return nil, err
	}
	return f.FS.ReadFile(name)
}

func (f *scopedBackupFS) MkdirAll(name string) error {
	if err := f.checkWrite(name); err != nil {
		return err
	}
	return f.FS.MkdirAll(name)
}

func (f *scopedBackupFS) WriteFile(name string, data []byte, perm fs.FileMode) error {
	if err := f.checkWrite(name); err != nil {
		return err
	}
	return f.FS.WriteFile(name, data, perm)
}

func (f *scopedBackupFS) SaveAtomic(name string, data []byte, perm fs.FileMode) error {
	if err := f.checkWrite(name); err != nil {
		return err
	}
	return f.FS.SaveAtomic(name, data, perm)
}

// SaveAtomicBounded applies the same write checks as SaveAtomic, then
// performs the bounded large save recovery uses for archives and restored
// payloads above the interactive document cap.
func (f *scopedBackupFS) SaveAtomicBounded(name string, data []byte, perm fs.FileMode, limit int64) error {
	if err := f.checkWrite(name); err != nil {
		return err
	}
	return vfs.SaveAtomicBounded(f.FS, name, data, perm, limit)
}

func (f *scopedBackupFS) Remove(name string) error {
	if err := f.checkWrite(name); err != nil {
		return err
	}
	return f.FS.Remove(name)
}

func (f *scopedBackupFS) RemoveAll(name string) error {
	if err := f.checkWrite(name); err != nil {
		return err
	}
	return f.FS.RemoveAll(name)
}

func (f *scopedBackupFS) Rename(oldPath, newPath string) error {
	if err := f.checkWrite(oldPath); err != nil {
		return err
	}
	if err := f.checkWrite(newPath); err != nil {
		return err
	}
	return f.FS.Rename(oldPath, newPath)
}

func (s *RecoveryService) exportBackup(ctx context.Context, req ipc.Request) (any, error) {
	var p BackupExportParams
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}

	destPath := p.Path
	if destPath == "" {
		targetProfile := p.ProfileID
		if targetProfile == "" && s.ctx.Profiles != nil {
			targetProfile = s.ctx.Profiles.ActiveID()
		}
		if targetProfile == "" {
			targetProfile = "guest"
		}
		// Default names embed the wall-clock second. Claim a free destination
		// under the lock so a repeated or concurrent same-second export lands
		// on a -N suffix instead of silently overwriting the earlier backup.
		s.exportMu.Lock()
		defer s.exportMu.Unlock()
		base := fmt.Sprintf("/users/%s/downloads/backup-%d", targetProfile, time.Now().Unix())
		destPath = base + recovery.FileExtension
		for n := 2; ; n++ {
			if _, err := s.ctx.VFS.Stat(destPath); err != nil {
				break
			}
			destPath = fmt.Sprintf("%s-%d%s", base, n, recovery.FileExtension)
		}
	}

	includeSystem := true
	if p.IncludeSystem != nil {
		includeSystem = *p.IncludeSystem
	}

	excludeSecrets := p.ExcludeSecrets
	if s.ctx.Token != "" {
		excludeSecrets = append(excludeSecrets, s.ctx.Token)
	}

	opts := recovery.ExportOptions{
		ProfileID:      p.ProfileID,
		IncludeSystem:  includeSystem,
		ExcludeSecrets: excludeSecrets,
		SourceVersion:  s.ctx.Version,
		Description:    p.Description,
	}

	// The system preferences store is a host file outside the VFS; supply
	// its live contents so backups capture what the environment reads.
	// App principals are excluded: injecting the host document would
	// bypass the grant-scoped read checks their backup path enforces.
	if includeSystem && s.ctx.Config != nil && !ipc.CallerPrincipal(ctx).IsApp() {
		if snap := s.ctx.Config.Snapshot(); len(snap) > 0 {
			if raw, err := json.Marshal(snap); err == nil {
				opts.SystemConfig = raw
			}
		}
	}

	target, err := s.callerVFS(ctx)
	if err != nil {
		return nil, err
	}

	res, err := recovery.ExportToVFS(ctx, target, destPath, opts)
	if err != nil {
		return nil, err
	}

	return res, nil
}

func (s *RecoveryService) inspectBackup(ctx context.Context, req ipc.Request) (any, error) {
	var p BackupPathParams
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if p.Path == "" {
		return nil, fmt.Errorf("recovery: path parameter is required")
	}

	target, err := s.callerVFS(ctx)
	if err != nil {
		return nil, err
	}

	cleanPath := path.Clean(p.Path)
	data, err := target.ReadFile(cleanPath)
	if err != nil {
		return nil, fmt.Errorf("recovery: read archive %q: %w", cleanPath, err)
	}

	manifest, err := recovery.Inspect(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	return manifest, nil
}

func (s *RecoveryService) previewBackup(ctx context.Context, req ipc.Request) (any, error) {
	var p BackupPathParams
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if p.Path == "" {
		return nil, fmt.Errorf("recovery: path parameter is required")
	}

	target, err := s.callerVFS(ctx)
	if err != nil {
		return nil, err
	}

	cleanPath := path.Clean(p.Path)
	data, err := target.ReadFile(cleanPath)
	if err != nil {
		return nil, fmt.Errorf("recovery: read archive %q: %w", cleanPath, err)
	}

	report, err := recovery.PreviewOpt(bytes.NewReader(data), target, s.systemConfigPath(ctx))
	if err != nil {
		return nil, err
	}
	return report, nil
}

// systemConfigPath returns the host path of the live system preferences
// store, or "" when none is configured. It returns "" for application
// principals so a grant-scoped restore cannot write a host file outside
// its scoped filesystem checks.
func (s *RecoveryService) systemConfigPath(ctx context.Context) string {
	if ipc.CallerPrincipal(ctx).IsApp() || s.ctx.Config == nil {
		return ""
	}
	return s.ctx.Config.Path()
}

func (s *RecoveryService) restoreBackup(ctx context.Context, req ipc.Request) (any, error) {
	var p BackupRestoreParams
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if p.Path == "" {
		return nil, fmt.Errorf("recovery: path parameter is required")
	}

	strategy := p.Strategy
	if strategy == "" {
		strategy = recovery.ConflictAbort
	}
	switch strategy {
	case recovery.ConflictAbort, recovery.ConflictOverwrite, recovery.ConflictSkip:
	default:
		return nil, fmt.Errorf("recovery: invalid conflict strategy %q (allowed: abort, overwrite, skip)", strategy)
	}

	target, err := s.callerVFS(ctx)
	if err != nil {
		return nil, err
	}

	cleanPath := path.Clean(p.Path)
	data, err := target.ReadFile(cleanPath)
	if err != nil {
		return nil, fmt.Errorf("recovery: read archive %q: %w", cleanPath, err)
	}

	opts := recovery.RestoreOptions{
		Strategy:         strategy,
		ProfileFilter:    p.ProfileFilter,
		SystemConfigPath: s.systemConfigPath(ctx),
	}

	rep, err := recovery.Restore(ctx, bytes.NewReader(data), target, opts)
	if err != nil {
		return nil, err
	}

	// Restored files replace live documents on disk; every service that
	// caches whole-document state must re-read it, or the restore only
	// half-landed and the next write would clobber the restored data.
	rep.Reloaded = s.reloadLiveStores(ctx)

	return rep, nil
}

// reloadLiveStores refreshes every cached store the restore may have
// rewritten. Profiles reload first: a changed active profile fires the
// switch hooks that rebind the user config path and runtime identity, and
// the later reloads then read the newly bound documents.
func (s *RecoveryService) reloadLiveStores(ctx context.Context) []string {
	var reloaded []string
	if s.ctx.Profiles != nil {
		if err := s.ctx.Profiles.Reload(); err == nil {
			reloaded = append(reloaded, "profiles")
		}
	}
	if s.ctx.Layered != nil {
		if err := s.ctx.Layered.Reload(); err == nil {
			reloaded = append(reloaded, "config")
		}
	}
	if s.ctx.Sessions != nil {
		s.ctx.Sessions.ReloadWorkspaces()
		reloaded = append(reloaded, "workspaces")
	}
	if s.ctx.Services != nil {
		if svc, ok := s.ctx.Services.Get("doc"); ok {
			if ds, ok := svc.(*DocumentService); ok && ds.Store() != nil {
				_ = ds.Store().Reload(ctx)
				reloaded = append(reloaded, "documents")
			}
		}
	}
	return reloaded
}
