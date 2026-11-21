package services

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"gostalgia/internal/ipc"
	"gostalgia/internal/pkg"
	"gostalgia/internal/security"
	"gostalgia/internal/service"
)

// PackageService owns pkg/* and the rooted installed package store.
type PackageService struct {
	ctx     *service.Context
	manager *pkg.Manager
}

func NewPackage() *PackageService           { return &PackageService{} }
func (s *PackageService) Name() string      { return "package" }
func (s *PackageService) Depends() []string { return nil }

func (s *PackageService) Init(ctx *service.Context) error {
	s.ctx = ctx
	trust, err := loadPackageTrust(filepath.Join(ctx.Root, "config", "package-trust.json"))
	if err != nil {
		return err
	}
	s.manager, err = pkg.NewManager(filepath.Join(ctx.Root, "vfs", "apps"), ctx.Apps, trust)
	return err
}

func loadPackageTrust(name string) (pkg.TrustStore, error) {
	f, err := os.Open(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return nil, fmt.Errorf("package: invalid trust store")
	}
	raw, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	if err != nil || len(raw) > 64<<10 {
		return nil, fmt.Errorf("package: trust store exceeds budget")
	}
	var encoded map[string]string
	if err := json.Unmarshal(raw, &encoded); err != nil {
		return nil, err
	}
	trust := make(pkg.TrustStore)
	for id, value := range encoded {
		key, err := base64.StdEncoding.DecodeString(value)
		if err != nil || id == "" || len(key) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("package: invalid trusted publisher key")
		}
		trust[id] = ed25519.PublicKey(key)
	}
	return trust, nil
}

func (s *PackageService) Start(ctx context.Context) error {
	routes := map[string]ipc.Route{
		"pkg/install":   {Handler: s.handle, Policy: ipc.Cap(security.CapPackageWrite)},
		"pkg/update":    {Handler: s.handle, Policy: ipc.Cap(security.CapPackageWrite)},
		"pkg/uninstall": {Handler: s.handle, Policy: ipc.Cap(security.CapPackageWrite)},
		"pkg/rollback":  {Handler: s.handle, Policy: ipc.Cap(security.CapPackageWrite)},
		"pkg/list":      {Handler: s.handle, Policy: ipc.Cap(security.CapPackageRead)},
		"pkg/inspect":   {Handler: s.handle, Policy: ipc.Cap(security.CapPackageRead)},
	}
	if err := s.ctx.Router.HandleRoutes(routes); err != nil {
		_ = s.manager.Close()
		return err
	}
	return nil
}

func (s *PackageService) Stop(ctx context.Context) error {
	s.ctx.Router.UnhandlePrefix("pkg/")
	return s.manager.Close()
}

func (s *PackageService) archive(ctx context.Context, name string) (fs.File, error) {
	// Reuse the FS service's path-grant and shared-host policy checks, including
	// scoped reads. A package capability is not arbitrary host read permission.
	reader := &FSService{ctx: s.ctx}
	if err := reader.requireReadAccess(ctx, name); err != nil {
		return nil, err
	}
	target, err := reader.targetFS(ctx)
	if err != nil {
		return nil, err
	}
	info, err := target.Stat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > pkg.MaxArchiveBytes {
		return nil, fmt.Errorf("package: archive must be a bounded regular file")
	}
	return target.Open(name)
}

func (s *PackageService) handle(ctx context.Context, req ipc.Request) (any, error) {
	var p pkg.Params
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if p.ConfirmPermissions && !ipc.CallerPrincipal(ctx).IsOperator() &&
		(ipc.Capabilities(ctx) == nil || !ipc.Capabilities(ctx).Has(security.CapAdmin)) {
		return nil, fmt.Errorf("package: only the operator may confirm permission expansion")
	}
	switch req.Method {
	case "pkg/list":
		if p.ID != "" || p.Path != "" || p.ConfirmPermissions {
			return nil, fmt.Errorf("package: list takes no parameters")
		}
		return s.manager.List(), nil
	case "pkg/install", "pkg/update":
		if p.Path == "" || p.ID != "" {
			return nil, fmt.Errorf("package: params.path is required (VFS path)")
		}
		f, err := s.archive(ctx, p.Path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return s.manager.Install(ctx, f, req.Method == "pkg/update", p.ConfirmPermissions)
	case "pkg/inspect":
		if p.ConfirmPermissions || (p.ID == "") == (p.Path == "") {
			return nil, fmt.Errorf("package: inspect requires exactly one of id or path")
		}
		if p.ID != "" {
			return s.manager.Inspect(p.ID)
		}
		f, err := s.archive(ctx, p.Path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return s.manager.InspectArchive(f)
	case "pkg/rollback":
		if p.ID == "" || p.Path != "" {
			return nil, fmt.Errorf("package: params.id is required")
		}
		return s.manager.Rollback(ctx, p.ID, p.ConfirmPermissions)
	case "pkg/uninstall":
		if p.ID == "" || p.Path != "" || p.ConfirmPermissions {
			return nil, fmt.Errorf("package: params.id is required")
		}
		if err := s.manager.Uninstall(ctx, p.ID); err != nil {
			return nil, err
		}
		return map[string]any{"id": p.ID, "uninstalled": true}, nil
	}
	return nil, fmt.Errorf("package: unknown route")
}
