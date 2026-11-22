package services

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"io/fs"
	"time"

	"gostalgia/internal/ipc"
	"gostalgia/internal/profile"
	"gostalgia/internal/security"
	"gostalgia/internal/service"
	"gostalgia/internal/vfs"
)

// FSService exposes the virtual filesystem over IPC. It owns "fs/*".
type FSService struct {
	ctx *service.Context
}

func NewFS() *FSService { return &FSService{} }

func (s *FSService) Name() string      { return "fs" }
func (s *FSService) Depends() []string { return nil }

func (s *FSService) Init(ctx *service.Context) error {
	s.ctx = ctx
	// Trash is per-profile: point the trash tree at the active profile and
	// re-point it on every switch, mirroring how document stores rebind.
	if v, ok := ctx.VFS.(*vfs.VFS); ok && ctx.Profiles != nil {
		v.SetTrashDir(profile.TrashDir(ctx.Profiles.ActiveID()))
		ctx.Profiles.OnSwitch(func(prev, next profile.Profile) {
			v.SetTrashDir(profile.TrashDir(next.ID))
		})
	}
	return nil
}

func (s *FSService) Start(ctx context.Context) error {
	return s.ctx.Router.HandleRoutes(map[string]ipc.Route{
		// The data routes are app-scoped: capability holders get the
		// whole VFS, application principals reach the handler which
		// applies the per-path grant, private-path, and shared-host
		// policy rules a static capability cannot express.
		"fs/list":   {Handler: s.list, Policy: ipc.AppScoped(security.CapFileRead)},
		"fs/stat":   {Handler: s.stat, Policy: ipc.AppScoped(security.CapFileRead)},
		"fs/read":   {Handler: s.read, Policy: ipc.AppScoped(security.CapFileRead)},
		"fs/write":  {Handler: s.write, Policy: ipc.AppScoped(security.CapFileWrite)},
		"fs/save":   {Handler: s.save, Policy: ipc.AppScoped(security.CapFileWrite)},
		"fs/mkdir":  {Handler: s.mkdir, Policy: ipc.AppScoped(security.CapFileWrite)},
		"fs/remove": {Handler: s.remove, Policy: ipc.AppScoped(security.CapFileWrite)},
		"fs/rename": {Handler: s.rename, Policy: ipc.AppScoped(security.CapFileWrite)},
		"fs/copy":   {Handler: s.copy, Policy: ipc.AppScoped(security.CapFileWrite)},
		"fs/move":   {Handler: s.move, Policy: ipc.AppScoped(security.CapFileWrite)},
		"fs/trash":  {Handler: s.trash, Policy: ipc.AppScoped(security.CapFileWrite)},
		// Trash restore/empty still require the capability outright —
		// the per-item origin checks below additionally re-scope apps.
		"fs/restore":      {Handler: s.restore, Policy: ipc.Cap(security.CapFileWrite)},
		"fs/trash/list":   {Handler: s.trashList, Policy: ipc.Cap(security.CapFileRead)},
		"fs/trash/empty":  {Handler: s.trashEmpty, Policy: ipc.Cap(security.CapFileWrite)},
		"fs/grant":        {Handler: s.grant, Policy: ipc.Cap(security.CapAdmin)},
		"fs/grant/revoke": {Handler: s.grantRevoke, Policy: ipc.Cap(security.CapAdmin)},
		// Apps inspect their own grants; operators and admins see all.
		"fs/grant/list": {Handler: s.grantList, Policy: ipc.Policy{
			Caps:     []string{security.CapAdmin},
			Operator: true,
			App:      true,
		}},
		"hostfs/mount":   {Handler: s.hostfsMount, Policy: ipc.OperatorOr(security.CapAdmin)},
		"hostfs/unmount": {Handler: s.hostfsUnmount, Policy: ipc.OperatorOr(security.CapAdmin)},
		"hostfs/list":    {Handler: s.hostfsList, Policy: ipc.Baseline},
	})
}

func (s *FSService) Stop(ctx context.Context) error {
	s.ctx.Router.UnhandlePrefix("fs/")
	s.ctx.Router.UnhandlePrefix("hostfs/")
	return nil
}

func (s *FSService) targetFS(ctx context.Context) (vfs.DocumentFS, error) {
	principal := ipc.CallerPrincipal(ctx)
	caps := ipc.Capabilities(ctx)
	if principal.IsOperator() || (caps != nil && caps.Has(security.CapAdmin)) {
		if dfs, ok := s.ctx.VFS.(vfs.DocumentFS); ok {
			return dfs, nil
		}
		return nil, fmt.Errorf("underlying filesystem does not support document operations")
	}
	if principal.IsApp() {
		if v, ok := s.ctx.VFS.(*vfs.VFS); ok {
			return v.ForApp(principal.AppID), nil
		}
		if dfs, ok := s.ctx.VFS.(vfs.DocumentFS); ok {
			return dfs, nil
		}
	}
	if dfs, ok := s.ctx.VFS.(vfs.DocumentFS); ok {
		return dfs, nil
	}
	return nil, &vfs.Error{Op: "access", Code: vfs.ErrPermission, Message: "permission denied: caller not authorized"}
}

func (s *FSService) grantStore() (*vfs.GrantStore, error) {
	if v, ok := s.ctx.VFS.(*vfs.VFS); ok {
		if gs := v.Grants(); gs != nil {
			return gs, nil
		}
	}
	return nil, fmt.Errorf("grant store is not available on this filesystem")
}

func (s *FSService) requireReadAccess(ctx context.Context, envPath string) error {
	return fsAccessChecker{s.ctx}.read(ctx, envPath)
}

func (s *FSService) requireWriteAccess(ctx context.Context, envPath string) error {
	return fsAccessChecker{s.ctx}.write(ctx, envPath)
}

// fsAccessChecker evaluates the per-path access rules shared by fs/* and
// backup/*: shared host mounts obey the operator hostfs policy (and require
// the hostfs capabilities for app callers), app-private storage is open to
// its owner, granted paths are open to their grantee, and everything else
// falls back to the filesystem capabilities.
type fsAccessChecker struct {
	sctx *service.Context
}

func (c fsAccessChecker) read(ctx context.Context, envPath string) error {
	principal := ipc.CallerPrincipal(ctx)
	if v, ok := c.sctx.VFS.(*vfs.VFS); ok {
		if isShared, hostPath, _ := v.SharedHostInfo(envPath); isShared {
			policy := security.DefaultOperatorPolicy()
			if c.sctx.Policy != nil {
				policy = c.sctx.Policy.Get()
			}
			if err := policy.CheckHostFS(hostPath, false); err != nil {
				return err
			}
			if principal.IsApp() {
				return ipc.RequireCap(ctx, security.CapHostFSRead)
			}
			return nil
		}
	}

	if principal.IsApp() {
		if vfs.IsAppPrivatePath(principal.AppID, envPath) {
			return nil
		}
		if v, ok := c.sctx.VFS.(*vfs.VFS); ok && v.Grants() != nil {
			if _, ok := v.Grants().FindMatchingGrant(principal.AppID, envPath); ok {
				return nil
			}
		}
	}
	return ipc.RequireCap(ctx, security.CapFileRead)
}

func (c fsAccessChecker) write(ctx context.Context, envPath string) error {
	principal := ipc.CallerPrincipal(ctx)
	if v, ok := c.sctx.VFS.(*vfs.VFS); ok {
		if isShared, hostPath, readOnly := v.SharedHostInfo(envPath); isShared {
			if readOnly {
				return &vfs.Error{Op: "write", Path: envPath, Code: vfs.ErrReadOnly, Message: "shared host mount is read-only"}
			}
			policy := security.DefaultOperatorPolicy()
			if c.sctx.Policy != nil {
				policy = c.sctx.Policy.Get()
			}
			if err := policy.CheckHostFS(hostPath, true); err != nil {
				return err
			}
			if principal.IsApp() {
				return ipc.RequireCap(ctx, security.CapHostFSWrite)
			}
			return nil
		}
	}

	if principal.IsApp() {
		if vfs.IsAppPrivatePath(principal.AppID, envPath) {
			return nil
		}
		if v, ok := c.sctx.VFS.(*vfs.VFS); ok && v.Grants() != nil {
			if g, ok := v.Grants().FindMatchingGrant(principal.AppID, envPath); ok && g.Access == vfs.AccessReadWrite {
				return nil
			}
		}
	}
	return ipc.RequireCap(ctx, security.CapFileWrite)
}

type fsEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
	Mode  string `json:"mode"`
}

func (s *FSService) list(ctx context.Context, req ipc.Request) (any, error) {
	var p struct {
		Path string `json:"path"`
	}
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if p.Path == "" {
		p.Path = "/"
	}
	if err := s.requireReadAccess(ctx, p.Path); err != nil {
		return nil, err
	}
	target, err := s.targetFS(ctx)
	if err != nil {
		return nil, err
	}
	entries, err := target.ReadDir(p.Path)
	if err != nil {
		return nil, err
	}
	out := make([]fsEntry, 0, len(entries))
	for _, e := range entries {
		mode := e.Type().String()
		size := int64(0)
		if !e.IsDir() {
			if info, err := e.Info(); err == nil {
				size = info.Size()
			}
		}
		out = append(out, fsEntry{Name: e.Name(), IsDir: e.IsDir(), Size: size, Mode: mode})
	}
	return map[string]any{"path": p.Path, "entries": out}, nil
}

func (s *FSService) stat(ctx context.Context, req ipc.Request) (any, error) {
	var p struct {
		Path string `json:"path"`
	}
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if p.Path == "" {
		return nil, fmt.Errorf("params.path is required")
	}
	if err := s.requireReadAccess(ctx, p.Path); err != nil {
		return nil, err
	}
	target, err := s.targetFS(ctx)
	if err != nil {
		return nil, err
	}
	info, err := target.Stat(p.Path)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"path":     p.Path,
		"name":     info.Name(),
		"is_dir":   info.IsDir(),
		"size":     info.Size(),
		"mode":     info.Mode().String(),
		"mod_time": info.ModTime().UTC().Format(time.RFC3339),
	}, nil
}

func (s *FSService) read(ctx context.Context, req ipc.Request) (any, error) {
	var p struct {
		Path   string `json:"path"`
		Offset int64  `json:"offset"`
		Limit  int64  `json:"limit"`
	}
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if p.Path == "" {
		return nil, fmt.Errorf("params.path is required")
	}
	if err := s.requireReadAccess(ctx, p.Path); err != nil {
		return nil, err
	}
	target, err := s.targetFS(ctx)
	if err != nil {
		return nil, err
	}

	info, err := target.Stat(p.Path)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, &vfs.Error{Op: "read", Path: p.Path, Code: vfs.ErrIsDir, Message: "path is a directory"}
	}
	totalSize := info.Size()

	if p.Limit <= 0 && p.Offset == 0 {
		if totalSize > vfs.MaxIPCReadLimit {
			return nil, &vfs.Error{
				Op:      "read",
				Path:    p.Path,
				Code:    vfs.ErrTooLarge,
				Message: fmt.Sprintf("file size %d exceeds default read limit of %d bytes; use bounded read with offset and limit", totalSize, vfs.MaxIPCReadLimit),
			}
		}
		data, err := target.ReadFile(p.Path)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"path":        p.Path,
			"size":        len(data),
			"total_size":  totalSize,
			"data_base64": base64.StdEncoding.EncodeToString(data),
		}, nil
	}

	// Bounded read
	limit := p.Limit
	if limit <= 0 || limit > vfs.MaxIPCReadLimit {
		limit = vfs.MaxIPCReadLimit
	}
	f, err := target.Open(p.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	if p.Offset > 0 {
		if seeker, ok := f.(io.ReadSeeker); ok {
			if _, err := seeker.Seek(p.Offset, io.SeekStart); err != nil {
				return nil, err
			}
		} else {
			if _, err := io.CopyN(io.Discard, f, p.Offset); err != nil {
				return nil, err
			}
		}
	}

	buf := make([]byte, limit)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, err
	}
	data := buf[:n]
	return map[string]any{
		"path":        p.Path,
		"offset":      p.Offset,
		"limit":       limit,
		"size":        len(data),
		"total_size":  totalSize,
		"data_base64": base64.StdEncoding.EncodeToString(data),
	}, nil
}

func (s *FSService) write(ctx context.Context, req ipc.Request) (any, error) {
	var p struct {
		Path string `json:"path"`
		Data string `json:"data_base64"`
	}
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if p.Path == "" {
		return nil, fmt.Errorf("params.path is required")
	}
	if err := s.requireWriteAccess(ctx, p.Path); err != nil {
		return nil, err
	}
	target, err := s.targetFS(ctx)
	if err != nil {
		return nil, err
	}
	var data []byte
	if p.Data != "" {
		var err error
		data, err = base64.StdEncoding.DecodeString(p.Data)
		if err != nil {
			return nil, fmt.Errorf("params.data_base64: %w", err)
		}
	}
	if len(data) > vfs.MaxIPCWriteLimit {
		return nil, &vfs.Error{
			Op:      "write",
			Path:    p.Path,
			Code:    vfs.ErrTooLarge,
			Message: fmt.Sprintf("payload size %d exceeds write limit of %d bytes", len(data), vfs.MaxIPCWriteLimit),
		}
	}
	if err := target.WriteFile(p.Path, data, 0o644); err != nil {
		return nil, err
	}
	return map[string]any{"path": p.Path, "written": len(data)}, nil
}

func (s *FSService) save(ctx context.Context, req ipc.Request) (any, error) {
	var p struct {
		Path      string `json:"path"`
		Data      string `json:"data_base64"`
		Perm      int    `json:"perm"`
		Overwrite *bool  `json:"overwrite"`
	}
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if p.Path == "" {
		return nil, fmt.Errorf("params.path is required")
	}
	if err := s.requireWriteAccess(ctx, p.Path); err != nil {
		return nil, err
	}
	target, err := s.targetFS(ctx)
	if err != nil {
		return nil, err
	}
	var data []byte
	if p.Data != "" {
		var err error
		data, err = base64.StdEncoding.DecodeString(p.Data)
		if err != nil {
			return nil, fmt.Errorf("params.data_base64: %w", err)
		}
	}
	if len(data) > vfs.MaxIPCWriteLimit {
		return nil, &vfs.Error{
			Op:      "save",
			Path:    p.Path,
			Code:    vfs.ErrTooLarge,
			Message: fmt.Sprintf("payload size %d exceeds write limit of %d bytes", len(data), vfs.MaxIPCWriteLimit),
		}
	}
	perm := fs.FileMode(0o644)
	if p.Perm != 0 {
		perm = fs.FileMode(p.Perm)
	}

	overwrite := true
	if p.Overwrite != nil {
		overwrite = *p.Overwrite
	}

	if !overwrite {
		if _, err := target.Stat(p.Path); err == nil {
			return nil, &vfs.Error{
				Op:      "save",
				Path:    p.Path,
				Code:    vfs.ErrExist,
				Message: "destination already exists",
			}
		}
	}

	if err := target.SaveAtomic(p.Path, data, perm); err != nil {
		return nil, err
	}
	return map[string]any{"path": p.Path, "written": len(data), "saved": true}, nil
}

func (s *FSService) mkdir(ctx context.Context, req ipc.Request) (any, error) {
	var p struct {
		Path string `json:"path"`
	}
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if p.Path == "" {
		return nil, fmt.Errorf("params.path is required")
	}
	if err := s.requireWriteAccess(ctx, p.Path); err != nil {
		return nil, err
	}
	target, err := s.targetFS(ctx)
	if err != nil {
		return nil, err
	}
	if err := target.MkdirAll(p.Path); err != nil {
		return nil, err
	}
	return map[string]any{"path": p.Path, "created": true}, nil
}

func (s *FSService) remove(ctx context.Context, req ipc.Request) (any, error) {
	var p struct {
		Path      string `json:"path"`
		Recursive bool   `json:"recursive"`
	}
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if p.Path == "" {
		return nil, fmt.Errorf("params.path is required")
	}
	if err := s.requireWriteAccess(ctx, p.Path); err != nil {
		return nil, err
	}
	target, err := s.targetFS(ctx)
	if err != nil {
		return nil, err
	}
	if p.Recursive {
		err = target.RemoveAll(p.Path)
	} else {
		err = target.Remove(p.Path)
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"path": p.Path, "removed": true}, nil
}

func (s *FSService) rename(ctx context.Context, req ipc.Request) (any, error) {
	var p struct {
		Src       string `json:"src"`
		Dst       string `json:"dst"`
		Overwrite bool   `json:"overwrite"`
	}
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if p.Src == "" || p.Dst == "" {
		return nil, fmt.Errorf("params.src and params.dst are required")
	}
	if err := s.requireWriteAccess(ctx, p.Src); err != nil {
		return nil, err
	}
	if err := s.requireWriteAccess(ctx, p.Dst); err != nil {
		return nil, err
	}
	target, err := s.targetFS(ctx)
	if err != nil {
		return nil, err
	}
	if scoped, ok := target.(*vfs.ScopedVFS); ok {
		if err := scoped.RenameOpt(p.Src, p.Dst, p.Overwrite); err != nil {
			return nil, err
		}
	} else if v, ok := target.(*vfs.VFS); ok {
		if err := v.RenameOpt(p.Src, p.Dst, p.Overwrite); err != nil {
			return nil, err
		}
	} else {
		if !p.Overwrite {
			if _, err := target.Stat(p.Dst); err == nil {
				return nil, &vfs.Error{
					Op:      "rename",
					Path:    p.Src,
					Dest:    p.Dst,
					Code:    vfs.ErrExist,
					Message: "destination already exists",
				}
			}
		}
		if err := target.Rename(p.Src, p.Dst); err != nil {
			return nil, err
		}
	}
	return map[string]any{"src": p.Src, "dst": p.Dst, "renamed": true}, nil
}

func (s *FSService) copy(ctx context.Context, req ipc.Request) (any, error) {
	var p struct {
		Src       string `json:"src"`
		Dst       string `json:"dst"`
		Overwrite bool   `json:"overwrite"`
	}
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if p.Src == "" || p.Dst == "" {
		return nil, fmt.Errorf("params.src and params.dst are required")
	}
	if err := s.requireReadAccess(ctx, p.Src); err != nil {
		return nil, err
	}
	if err := s.requireWriteAccess(ctx, p.Dst); err != nil {
		return nil, err
	}
	target, err := s.targetFS(ctx)
	if err != nil {
		return nil, err
	}
	if err := target.Copy(p.Src, p.Dst, p.Overwrite); err != nil {
		return nil, err
	}
	return map[string]any{"src": p.Src, "dst": p.Dst, "copied": true}, nil
}

func (s *FSService) move(ctx context.Context, req ipc.Request) (any, error) {
	var p struct {
		Src       string `json:"src"`
		Dst       string `json:"dst"`
		Overwrite bool   `json:"overwrite"`
	}
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if p.Src == "" || p.Dst == "" {
		return nil, fmt.Errorf("params.src and params.dst are required")
	}
	if err := s.requireWriteAccess(ctx, p.Src); err != nil {
		return nil, err
	}
	if err := s.requireWriteAccess(ctx, p.Dst); err != nil {
		return nil, err
	}
	target, err := s.targetFS(ctx)
	if err != nil {
		return nil, err
	}
	if err := target.Move(p.Src, p.Dst, p.Overwrite); err != nil {
		return nil, err
	}
	return map[string]any{"src": p.Src, "dst": p.Dst, "moved": true}, nil
}

func (s *FSService) trash(ctx context.Context, req ipc.Request) (any, error) {
	var p struct {
		Path string `json:"path"`
	}
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if p.Path == "" {
		return nil, fmt.Errorf("params.path is required")
	}
	if err := s.requireWriteAccess(ctx, p.Path); err != nil {
		return nil, err
	}
	target, err := s.targetFS(ctx)
	if err != nil {
		return nil, err
	}
	entry, err := target.Trash(p.Path)
	if err != nil {
		return nil, err
	}
	return entry, nil
}

func (s *FSService) restore(ctx context.Context, req ipc.Request) (any, error) {
	var p struct {
		ID        string `json:"id"`
		Dst       string `json:"dst"`
		Overwrite bool   `json:"overwrite"`
	}
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if p.ID == "" {
		return nil, fmt.Errorf("params.id is required")
	}
	if p.Dst != "" {
		if err := s.requireWriteAccess(ctx, p.Dst); err != nil {
			return nil, err
		}
	}
	target, err := s.targetFS(ctx)
	if err != nil {
		return nil, err
	}
	// The item's origin must pass the same policy and capability checks as a
	// direct write: grant checks never run on shared host mounts, so without
	// this the hostfs.* capabilities are bypassed for trashed items.
	entries, err := target.ListTrash()
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.ID == p.ID {
			if err := s.requireWriteAccess(ctx, e.OriginalPath); err != nil {
				return nil, err
			}
			break
		}
	}
	resPath, err := target.Restore(p.ID, p.Dst, p.Overwrite)
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": p.ID, "restored_path": resPath}, nil
}

func (s *FSService) trashList(ctx context.Context, req ipc.Request) (any, error) {
	target, err := s.targetFS(ctx)
	if err != nil {
		return nil, err
	}
	entries, err := target.ListTrash()
	if err != nil {
		return nil, err
	}
	// Re-check each item's origin: entries trashed from shared host mounts
	// are only visible to callers passing the hostfs read check.
	filtered := make([]vfs.TrashEntry, 0, len(entries))
	for _, e := range entries {
		if err := s.requireReadAccess(ctx, e.OriginalPath); err == nil {
			filtered = append(filtered, e)
		}
	}
	return map[string]any{"entries": filtered}, nil
}

func (s *FSService) trashEmpty(ctx context.Context, req ipc.Request) (any, error) {
	target, err := s.targetFS(ctx)
	if err != nil {
		return nil, err
	}
	entries, err := target.ListTrash()
	if err != nil {
		return nil, err
	}
	// Purge per item so each origin passes the write check: items trashed
	// from shared host mounts stay in place unless the caller holds
	// hostfs.write for them.
	count := 0
	for _, e := range entries {
		if err := s.requireWriteAccess(ctx, e.OriginalPath); err != nil {
			continue
		}
		if err := target.PurgeTrash(e.ID); err == nil {
			count++
		}
	}
	return map[string]any{"emptied": true, "count": count}, nil
}

func (s *FSService) grant(ctx context.Context, req ipc.Request) (any, error) {
	gs, err := s.grantStore()
	if err != nil {
		return nil, err
	}
	var p struct {
		AppID        string `json:"app_id"`
		Path         string `json:"path"`
		Access       string `json:"access"`
		Recursive    bool   `json:"recursive"`
		SessionBound bool   `json:"session_bound"`
	}
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if p.AppID == "" {
		return nil, fmt.Errorf("params.app_id is required")
	}
	if p.Path == "" {
		return nil, fmt.Errorf("params.path is required")
	}
	mode, err := vfs.NormalizeAccessMode(p.Access)
	if err != nil {
		return nil, err
	}
	var g *vfs.Grant
	if p.SessionBound {
		g, err = gs.IssueSession(p.AppID, p.Path, mode, p.Recursive)
	} else {
		g, err = gs.Issue(p.AppID, p.Path, mode, p.Recursive)
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"grant": g}, nil
}

func (s *FSService) grantRevoke(ctx context.Context, req ipc.Request) (any, error) {
	gs, err := s.grantStore()
	if err != nil {
		return nil, err
	}
	var p struct {
		ID string `json:"id"`
	}
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if p.ID == "" {
		return nil, fmt.Errorf("params.id is required")
	}
	if err := gs.Revoke(p.ID); err != nil {
		return nil, err
	}
	return map[string]any{"revoked": true, "id": p.ID}, nil
}

func (s *FSService) grantList(ctx context.Context, req ipc.Request) (any, error) {
	gs, err := s.grantStore()
	if err != nil {
		return nil, err
	}
	var p struct {
		AppID string `json:"app_id"`
	}
	_ = ipc.DecodeParams(req.Params, &p)

	principal := ipc.CallerPrincipal(ctx)
	caps := ipc.Capabilities(ctx)
	if principal.IsApp() {
		if p.AppID != "" && p.AppID != principal.AppID {
			return nil, &vfs.Error{Op: "grant/list", Code: vfs.ErrPermission, Message: "permission denied: cannot inspect another application's grants"}
		}
		p.AppID = principal.AppID
	} else if !principal.IsOperator() && (caps == nil || !caps.Has(security.CapAdmin)) {
		return nil, &vfs.Error{Op: "grant/list", Code: vfs.ErrPermission, Message: "permission denied: requires admin capability"}
	}
	grants := gs.List(p.AppID)
	if grants == nil {
		grants = []vfs.Grant{}
	}
	return map[string]any{"grants": grants}, nil
}

func (s *FSService) hostfsMount(ctx context.Context, req ipc.Request) (any, error) {
	var p struct {
		HostPath  string `json:"host_path"`
		MountPath string `json:"mount_path"`
		ReadOnly  bool   `json:"read_only"`
	}
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if p.HostPath == "" || p.MountPath == "" {
		return nil, fmt.Errorf("params.host_path and params.mount_path are required")
	}

	policy := security.DefaultOperatorPolicy()
	if s.ctx.Policy != nil {
		policy = s.ctx.Policy.Get()
	}
	if err := policy.CheckHostFS(p.HostPath, !p.ReadOnly); err != nil {
		return nil, err
	}

	v, ok := s.ctx.VFS.(*vfs.VFS)
	if !ok {
		return nil, fmt.Errorf("vfs does not support mounting")
	}

	sharedFS, err := vfs.NewSharedHost(p.HostPath, p.ReadOnly)
	if err != nil {
		return nil, err
	}

	if err := v.Mount(p.MountPath, sharedFS); err != nil {
		_ = sharedFS.Close()
		return nil, err
	}

	return map[string]any{
		"mounted":    true,
		"mount_path": p.MountPath,
		"host_path":  p.HostPath,
		"read_only":  p.ReadOnly,
	}, nil
}

func (s *FSService) hostfsUnmount(ctx context.Context, req ipc.Request) (any, error) {
	var p struct {
		MountPath string `json:"mount_path"`
	}
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if p.MountPath == "" {
		return nil, fmt.Errorf("params.mount_path is required")
	}

	v, ok := s.ctx.VFS.(*vfs.VFS)
	if !ok {
		return nil, fmt.Errorf("vfs does not support mounting")
	}

	if err := v.Unmount(p.MountPath); err != nil {
		return nil, err
	}

	return map[string]any{
		"unmounted":  true,
		"mount_path": p.MountPath,
	}, nil
}

func (s *FSService) hostfsList(ctx context.Context, req ipc.Request) (any, error) {
	v, ok := s.ctx.VFS.(*vfs.VFS)
	if !ok {
		return map[string]any{"mounts": []any{}}, nil
	}

	return map[string]any{
		"mounts": v.SharedHostMounts(),
	}, nil
}
