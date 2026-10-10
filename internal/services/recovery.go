package services

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"path"
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
	routes := map[string]ipc.Handler{
		"backup/export":  s.exportBackup,
		"backup/inspect": s.inspectBackup,
		"backup/preview": s.previewBackup,
		"backup/restore": s.restoreBackup,
	}

	for route, handler := range routes {
		if err := s.ctx.Router.Handle(route, handler); err != nil {
			return err
		}
	}
	return nil
}

func (s *RecoveryService) Stop(ctx context.Context) error {
	s.ctx.Router.UnhandlePrefix("backup/")
	return nil
}

func (s *RecoveryService) requireReadCap(ctx context.Context) error {
	if ipc.RequireCap(ctx, security.CapBackupRead) == nil {
		return nil
	}
	if ipc.RequireCap(ctx, security.CapAdmin) == nil {
		return nil
	}
	if ipc.CallerPrincipal(ctx).IsOperator() {
		return nil
	}
	return ipc.RequireCap(ctx, security.CapBackupRead)
}

func (s *RecoveryService) requireWriteCap(ctx context.Context) error {
	if ipc.RequireCap(ctx, security.CapBackupWrite) == nil {
		return nil
	}
	if ipc.RequireCap(ctx, security.CapAdmin) == nil {
		return nil
	}
	if ipc.CallerPrincipal(ctx).IsOperator() {
		return nil
	}
	return ipc.RequireCap(ctx, security.CapBackupWrite)
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
	if err := s.requireWriteCap(ctx); err != nil {
		return nil, err
	}

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
		destPath = fmt.Sprintf("/users/%s/downloads/backup-%d%s", targetProfile, time.Now().Unix(), recovery.FileExtension)
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
	if err := s.requireReadCap(ctx); err != nil {
		return nil, err
	}

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
	if err := s.requireReadCap(ctx); err != nil {
		return nil, err
	}

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

	report, err := recovery.Preview(bytes.NewReader(data), target)
	if err != nil {
		return nil, err
	}
	return report, nil
}

func (s *RecoveryService) restoreBackup(ctx context.Context, req ipc.Request) (any, error) {
	if err := s.requireWriteCap(ctx); err != nil {
		return nil, err
	}

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
		Strategy:      strategy,
		ProfileFilter: p.ProfileFilter,
	}

	rep, err := recovery.Restore(ctx, bytes.NewReader(data), target, opts)
	if err != nil {
		return nil, err
	}

	// If profiles registry was restored, reload profiles manager if present
	if s.ctx.Profiles != nil {
		_ = s.ctx.Profiles.Reload()
	}

	return rep, nil
}
