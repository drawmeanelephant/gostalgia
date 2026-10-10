package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
)

// Layer identifies a configuration layer in the precedence hierarchy.
type Layer string

const (
	LayerPreview Layer = "preview"
	LayerApp     Layer = "app"
	LayerUser    Layer = "user"
	LayerSystem  Layer = "system"
	LayerDefault Layer = "default"
)

// Precedence returns the ordered list of layers from highest to lowest precedence.
func Precedence() []Layer {
	return []Layer{LayerPreview, LayerApp, LayerUser, LayerSystem, LayerDefault}
}

// QueryOpts controls configuration lookup scope and layering.
type QueryOpts struct {
	AppID string `json:"app_id,omitempty"`
	User  string `json:"user,omitempty"`
	Layer Layer  `json:"layer,omitempty"`
}

// LayerExplanation details how an effective value was resolved across layers.
type LayerExplanation struct {
	Path      string        `json:"path"`
	Effective any           `json:"effective"`
	Winner    Layer         `json:"winner"`
	Layers    map[Layer]any `json:"layers"`
}

// ChangeEvent is published when any preference changes, is previewed, committed, or reset.
type ChangeEvent struct {
	Layer         Layer  `json:"layer"`
	Path          string `json:"path"`
	Value         any    `json:"value"`
	PreviousValue any    `json:"previous_value,omitempty"`
	IsPreview     bool   `json:"is_preview,omitempty"`
}

func (ChangeEvent) Type() string { return "config.changed" }

// LayeredStore coordinates system, user, app, and preview configuration layers.
type LayeredStore struct {
	mu            sync.RWMutex
	systemStore   *Store
	userStore     *Store
	appStores     map[string]*Store
	defaults      map[string]any
	previewData   map[string]any
	previewOwners map[string]string
	corruptLayers map[Layer]error
	listeners     []func(ChangeEvent)
}

// NewLayeredStore creates a layered configuration manager.
// If systemPath or userPath files are missing, empty stores are initialized.
// If a file exists but is corrupt, it is recorded in corruptLayers rather than overwritten.
func NewLayeredStore(systemPath, userPath string) (*LayeredStore, error) {
	ls := &LayeredStore{
		appStores:     make(map[string]*Store),
		defaults:      DefaultSettings(),
		previewData:   make(map[string]any),
		previewOwners: make(map[string]string),
		corruptLayers: make(map[Layer]error),
	}

	if systemPath != "" {
		sys, err := Load(systemPath)
		if err != nil {
			ls.corruptLayers[LayerSystem] = err
			ls.systemStore = &Store{path: systemPath, data: map[string]any{}}
		} else {
			ls.systemStore = sys
		}
	} else {
		ls.systemStore = &Store{data: map[string]any{}}
	}

	if userPath != "" {
		usr, err := Load(userPath)
		if err != nil {
			ls.corruptLayers[LayerUser] = err
			ls.userStore = &Store{path: userPath, data: map[string]any{}}
		} else {
			ls.userStore = usr
		}
	} else {
		ls.userStore = &Store{data: map[string]any{}}
	}

	return ls, nil
}

// SystemStore returns the underlying system store.
func (ls *LayeredStore) SystemStore() *Store {
	ls.mu.RLock()
	defer ls.mu.RUnlock()
	return ls.systemStore
}

// UserStore returns the underlying user store.
func (ls *LayeredStore) UserStore() *Store {
	ls.mu.RLock()
	defer ls.mu.RUnlock()
	return ls.userStore
}

// SetUserStore switches the user-layer configuration file to newUserPath.
// If the file is missing, an empty user store is initialized at that path.
// If the file is corrupt, the error is recorded in corruptLayers rather than overwritten.
func (ls *LayeredStore) SetUserStore(newUserPath string) error {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	delete(ls.corruptLayers, LayerUser)
	if newUserPath != "" {
		usr, err := Load(newUserPath)
		if err != nil {
			ls.corruptLayers[LayerUser] = err
			ls.userStore = &Store{path: newUserPath, data: map[string]any{}}
		} else {
			ls.userStore = usr
		}
	} else {
		ls.userStore = &Store{data: map[string]any{}}
	}
	return nil
}

// OnChange registers a listener for configuration change events.
func (ls *LayeredStore) OnChange(fn func(ChangeEvent)) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.listeners = append(ls.listeners, fn)
}

func (ls *LayeredStore) emit(ev ChangeEvent) {
	for _, fn := range ls.listeners {
		fn(ev)
	}
}

// CorruptLayers returns a map of layer names to their corruption errors, if any.
func (ls *LayeredStore) CorruptLayers() map[Layer]string {
	ls.mu.RLock()
	defer ls.mu.RUnlock()
	out := make(map[Layer]string)
	for l, err := range ls.corruptLayers {
		out[l] = err.Error()
	}
	return out
}

// Value returns the resolved configuration value and winning layer.
func (ls *LayeredStore) Value(path string, opts QueryOpts) (any, Layer, bool) {
	ls.mu.RLock()
	defer ls.mu.RUnlock()
	return ls.valueLocked(path, opts)
}

func (ls *LayeredStore) valueLocked(path string, opts QueryOpts) (any, Layer, bool) {
	segs := strings.Split(path, ".")

	// If a specific layer is requested, query only that layer.
	if opts.Layer != "" {
		switch opts.Layer {
		case LayerPreview:
			if v, ok := lookup(ls.previewData, segs); ok {
				return v, LayerPreview, true
			}
			return nil, LayerPreview, false
		case LayerApp:
			if opts.AppID != "" {
				if appStore, ok := ls.appStores[opts.AppID]; ok {
					if v, ok := appStore.Value(path); ok {
						return v, LayerApp, true
					}
				}
				if v, ok := ls.userStore.Value("apps." + opts.AppID + "." + path); ok {
					return v, LayerApp, true
				}
			}
			return nil, LayerApp, false
		case LayerUser:
			if v, ok := ls.userStore.Value(path); ok {
				return v, LayerUser, true
			}
			return nil, LayerUser, false
		case LayerSystem:
			if v, ok := ls.systemStore.Value(path); ok {
				return v, LayerSystem, true
			}
			return nil, LayerSystem, false
		case LayerDefault:
			if v, ok := lookup(ls.defaults, segs); ok {
				return v, LayerDefault, true
			}
			return nil, LayerDefault, false
		default:
			return nil, "", false
		}
	}

	// 1. Preview layer (highest precedence)
	if v, ok := lookup(ls.previewData, segs); ok {
		return v, LayerPreview, true
	}

	// 2. App layer (if app context provided)
	if opts.AppID != "" {
		if appStore, ok := ls.appStores[opts.AppID]; ok {
			if v, ok := appStore.Value(path); ok {
				return v, LayerApp, true
			}
		}
		if v, ok := ls.userStore.Value("apps." + opts.AppID + "." + path); ok {
			return v, LayerApp, true
		}
	}

	// 3. User layer
	if v, ok := ls.userStore.Value(path); ok {
		return v, LayerUser, true
	}

	// 4. System layer
	if v, ok := ls.systemStore.Value(path); ok {
		return v, LayerSystem, true
	}

	// 5. Built-in defaults
	if v, ok := lookup(ls.defaults, segs); ok {
		return v, LayerDefault, true
	}

	return nil, "", false
}

// String returns the string at path or fallback.
func (ls *LayeredStore) String(path, def string, opts QueryOpts) string {
	v, _, ok := ls.Value(path, opts)
	if !ok {
		return def
	}
	if str, ok := v.(string); ok {
		return str
	}
	return fmt.Sprintf("%v", v)
}

// Int returns the int at path or fallback.
func (ls *LayeredStore) Int(path string, def int, opts QueryOpts) int {
	v, _, ok := ls.Value(path, opts)
	if !ok {
		return def
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	default:
		return def
	}
}

// Bool returns the bool at path or fallback.
func (ls *LayeredStore) Bool(path string, def bool, opts QueryOpts) bool {
	v, _, ok := ls.Value(path, opts)
	if !ok {
		return def
	}
	if b, ok := v.(bool); ok {
		return b
	}
	return def
}

// Set stores a value in the specified layer (System, User, or App), validates it,
// verifies shortcut conflicts, persists it atomically, and emits a ChangeEvent.
func (ls *LayeredStore) Set(layer Layer, path string, val any, opts QueryOpts) error {
	if err := Validate(path, val); err != nil {
		return err
	}

	ls.mu.Lock()
	defer ls.mu.Unlock()

	// If updating a shortcut, verify there are no keybinding conflicts in the effective shortcuts.
	if strings.HasPrefix(path, "shortcuts.") {
		action := strings.TrimPrefix(path, "shortcuts.")
		newKey, ok := val.(string)
		if ok {
			effectiveShortcuts := ls.collectShortcutsLocked(QueryOpts{AppID: opts.AppID})
			effectiveShortcuts[action] = newKey
			conflicts := DetectKeybindingConflicts(effectiveShortcuts)
			if len(conflicts) > 0 {
				return &KeybindingConflictError{Conflicts: conflicts}
			}
		}
	}

	prevVal, _, _ := ls.valueLocked(path, opts)

	targetLayer := layer
	if targetLayer == "" {
		targetLayer = LayerUser
	}

	switch targetLayer {
	case LayerSystem:
		if err := ls.systemStore.Set(path, val); err != nil {
			return err
		}
		delete(ls.corruptLayers, LayerSystem)
	case LayerUser:
		if err := ls.userStore.Set(path, val); err != nil {
			return err
		}
		delete(ls.corruptLayers, LayerUser)
	case LayerApp:
		if opts.AppID == "" {
			return errors.New("config: app layer requires app_id")
		}
		appStore, ok := ls.appStores[opts.AppID]
		if !ok {
			if ls.userStore.Path() != "" {
				appPath := filepath.Join(filepath.Dir(ls.userStore.Path()), "apps", opts.AppID+".json")
				var err error
				appStore, err = Load(appPath)
				if err != nil {
					return err
				}
				ls.appStores[opts.AppID] = appStore
			} else {
				appStore = &Store{data: map[string]any{}}
				ls.appStores[opts.AppID] = appStore
			}
		}
		if err := appStore.Set(path, val); err != nil {
			return err
		}
	default:
		return fmt.Errorf("config: cannot set in layer %s", targetLayer)
	}

	newEffective, _, _ := ls.valueLocked(path, opts)
	ls.emit(ChangeEvent{
		Layer:         targetLayer,
		Path:          path,
		Value:         newEffective,
		PreviousValue: prevVal,
		IsPreview:     false,
	})
	return nil
}

// Unset removes a path from the specified layer and persists the change.
func (ls *LayeredStore) Unset(layer Layer, path string, opts QueryOpts) error {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	prevVal, _, _ := ls.valueLocked(path, opts)
	targetLayer := layer
	if targetLayer == "" {
		targetLayer = LayerUser
	}

	switch targetLayer {
	case LayerSystem:
		if _, err := ls.systemStore.Delete(path); err != nil {
			return err
		}
	case LayerUser:
		if _, err := ls.userStore.Delete(path); err != nil {
			return err
		}
	case LayerApp:
		if opts.AppID == "" {
			return errors.New("config: app layer requires app_id")
		}
		if appStore, ok := ls.appStores[opts.AppID]; ok {
			if _, err := appStore.Delete(path); err != nil {
				return err
			}
		}
		_, _ = ls.userStore.Delete("apps." + opts.AppID + "." + path)
	case LayerPreview:
		if ls.previewOwners[path] == previewScope(opts) {
			ls.dropPreviewPathLocked(path)
		}
	default:
		return fmt.Errorf("config: cannot unset from layer %s", targetLayer)
	}

	newEffective, _, _ := ls.valueLocked(path, opts)
	ls.emit(ChangeEvent{
		Layer:         targetLayer,
		Path:          path,
		Value:         newEffective,
		PreviousValue: prevVal,
		IsPreview:     false,
	})
	return nil
}

// Reset clears an entire layer or a specific path within that layer.
func (ls *LayeredStore) Reset(layer Layer, path string, opts QueryOpts) error {
	if path != "" {
		return ls.Unset(layer, path, opts)
	}

	ls.mu.Lock()
	defer ls.mu.Unlock()

	targetLayer := layer
	if targetLayer == "" {
		targetLayer = LayerUser
	}

	switch targetLayer {
	case LayerSystem:
		if err := ls.systemStore.Reset(); err != nil {
			return err
		}
		delete(ls.corruptLayers, LayerSystem)
	case LayerUser:
		if err := ls.userStore.Reset(); err != nil {
			return err
		}
		delete(ls.corruptLayers, LayerUser)
	case LayerApp:
		if opts.AppID != "" {
			if appStore, ok := ls.appStores[opts.AppID]; ok {
				if err := appStore.Reset(); err != nil {
					return err
				}
			}
		} else {
			for _, appStore := range ls.appStores {
				_ = appStore.Reset()
			}
		}
	case LayerPreview:
		for p := range ls.ownedPreviewLocked(previewScope(opts)) {
			ls.dropPreviewPathLocked(p)
		}
	default:
		return fmt.Errorf("config: cannot reset layer %s", targetLayer)
	}

	ls.emit(ChangeEvent{
		Layer: targetLayer,
		Path:  "",
		Value: nil,
	})
	return nil
}

// Preview sets an in-memory override without touching disk.
func (ls *LayeredStore) Preview(path string, val any, opts QueryOpts) error {
	if err := Validate(path, val); err != nil {
		return err
	}

	ls.mu.Lock()
	defer ls.mu.Unlock()

	prevVal, _, _ := ls.valueLocked(path, opts)
	segs := strings.Split(path, ".")
	m := ls.previewData
	for _, seg := range segs[:len(segs)-1] {
		next, ok := m[seg]
		if !ok {
			next = map[string]any{}
			m[seg] = next
		}
		nm, ok := next.(map[string]any)
		if !ok {
			return fmt.Errorf("config: preview conflict at %s", seg)
		}
		m = nm
	}
	m[segs[len(segs)-1]] = val

	// Record ownership: only the staging caller's scope may commit or
	// cancel this override. Re-staging a path retires ownership records
	// for any staged descendants it replaced.
	scope := previewScope(opts)
	ls.previewOwners[path] = scope
	for p := range ls.previewOwners {
		if p != path && strings.HasPrefix(p, path+".") {
			delete(ls.previewOwners, p)
		}
	}

	ls.emit(ChangeEvent{
		Layer:         LayerPreview,
		Path:          path,
		Value:         val,
		PreviousValue: prevVal,
		IsPreview:     true,
	})
	return nil
}

// previewScope identifies the owner of a staged preview override. The
// empty scope is shared by operators and in-process callers; application
// callers stage under their own app ID.
func previewScope(opts QueryOpts) string {
	return opts.AppID
}

// ownedPreviewLocked flattens the staged overrides owned by scope into
// dotted paths, pruning stale ownership records. Caller must hold ls.mu.
func (ls *LayeredStore) ownedPreviewLocked(scope string) map[string]any {
	flat := make(map[string]any)
	for p, owner := range ls.previewOwners {
		if owner != scope {
			continue
		}
		if v, ok := lookup(ls.previewData, strings.Split(p, ".")); ok {
			flat[p] = v
		} else {
			delete(ls.previewOwners, p)
		}
	}
	return flat
}

// dropPreviewPathLocked removes a staged override and its ownership
// record. Caller must hold ls.mu.
func (ls *LayeredStore) dropPreviewPathLocked(path string) {
	deleteNested(ls.previewData, strings.Split(path, "."))
	delete(ls.previewOwners, path)
}

// deleteNested removes the leaf at segs and prunes emptied parents.
func deleteNested(m map[string]any, segs []string) {
	if len(segs) == 0 {
		return
	}
	if len(segs) == 1 {
		delete(m, segs[0])
		return
	}
	if next, ok := m[segs[0]].(map[string]any); ok {
		deleteNested(next, segs[1:])
		if len(next) == 0 {
			delete(m, segs[0])
		}
	}
}

// PreviewBatch applies multiple in-memory overrides.
func (ls *LayeredStore) PreviewBatch(settings map[string]any, opts QueryOpts) error {
	flat := make(map[string]any)
	FlattenMap("", settings, flat)

	for p, v := range flat {
		if err := ls.Preview(p, v, opts); err != nil {
			return err
		}
	}
	return nil
}

// CancelPreview discards the caller's active preview overrides without
// touching disk. Overrides staged by other callers are left in place.
func (ls *LayeredStore) CancelPreview(opts QueryOpts) {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	flat := ls.ownedPreviewLocked(previewScope(opts))
	if len(flat) == 0 {
		return
	}

	for p := range flat {
		ls.dropPreviewPathLocked(p)
	}

	for p, oldPreviewVal := range flat {
		effectiveVal, winningLayer, _ := ls.valueLocked(p, opts)
		ls.emit(ChangeEvent{
			Layer:         winningLayer,
			Path:          p,
			Value:         effectiveVal,
			PreviousValue: oldPreviewVal,
			IsPreview:     false,
		})
	}
}

// HasActivePreview reports whether any preview overrides are active.
func (ls *LayeredStore) HasActivePreview() bool {
	ls.mu.RLock()
	defer ls.mu.RUnlock()
	return len(ls.previewData) > 0
}

// CommitPreview persists the caller's staged preview overrides to the
// specified layer. Overrides staged by other callers remain staged.
func (ls *LayeredStore) CommitPreview(targetLayer Layer, opts QueryOpts) error {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	flat := ls.ownedPreviewLocked(previewScope(opts))
	if len(flat) == 0 {
		return nil
	}

	layer := targetLayer
	if layer == "" {
		layer = LayerUser
	}

	for p, v := range flat {
		switch layer {
		case LayerSystem:
			if err := ls.systemStore.Set(p, v); err != nil {
				return err
			}
		case LayerUser:
			if err := ls.userStore.Set(p, v); err != nil {
				return err
			}
		default:
			return fmt.Errorf("config: unsupported commit layer %s", layer)
		}
	}

	for p := range flat {
		ls.dropPreviewPathLocked(p)
	}

	for p, v := range flat {
		ls.emit(ChangeEvent{
			Layer:     layer,
			Path:      p,
			Value:     v,
			IsPreview: false,
		})
	}
	return nil
}

// Explain returns a breakdown of how a path's effective value is resolved.
func (ls *LayeredStore) Explain(path string, opts QueryOpts) LayerExplanation {
	ls.mu.RLock()
	defer ls.mu.RUnlock()

	segs := strings.Split(path, ".")
	exp := LayerExplanation{
		Path:   path,
		Layers: make(map[Layer]any),
	}

	if v, ok := lookup(ls.previewData, segs); ok {
		exp.Layers[LayerPreview] = v
	}
	if opts.AppID != "" {
		if appStore, ok := ls.appStores[opts.AppID]; ok {
			if v, ok := appStore.Value(path); ok {
				exp.Layers[LayerApp] = v
			}
		}
		if v, ok := ls.userStore.Value("apps." + opts.AppID + "." + path); ok {
			exp.Layers[LayerApp] = v
		}
	}
	if v, ok := ls.userStore.Value(path); ok {
		exp.Layers[LayerUser] = v
	}
	if v, ok := ls.systemStore.Value(path); ok {
		exp.Layers[LayerSystem] = v
	}
	if v, ok := lookup(ls.defaults, segs); ok {
		exp.Layers[LayerDefault] = v
	}

	effective, winner, _ := ls.valueLocked(path, opts)
	exp.Effective = effective
	exp.Winner = winner
	return exp
}

// Snapshot returns the merged effective configuration as a nested map.
func (ls *LayeredStore) Snapshot(opts QueryOpts) map[string]any {
	ls.mu.RLock()
	defer ls.mu.RUnlock()

	merged := cloneMap(ls.defaults)
	mergeMaps(merged, ls.systemStore.Snapshot())
	mergeMaps(merged, ls.userStore.Snapshot())
	if opts.AppID != "" {
		if appStore, ok := ls.appStores[opts.AppID]; ok {
			mergeMaps(merged, appStore.Snapshot())
		}
	}
	mergeMaps(merged, cloneMap(ls.previewData))
	return merged
}

// LayerSnapshot returns the configuration map for a single layer.
func (ls *LayeredStore) LayerSnapshot(layer Layer, opts QueryOpts) map[string]any {
	ls.mu.RLock()
	defer ls.mu.RUnlock()

	switch layer {
	case LayerPreview:
		return cloneMap(ls.previewData)
	case LayerApp:
		if opts.AppID != "" {
			if appStore, ok := ls.appStores[opts.AppID]; ok {
				return appStore.Snapshot()
			}
		}
		return map[string]any{}
	case LayerUser:
		return ls.userStore.Snapshot()
	case LayerSystem:
		return ls.systemStore.Snapshot()
	case LayerDefault:
		return cloneMap(ls.defaults)
	default:
		return map[string]any{}
	}
}

func (ls *LayeredStore) collectShortcutsLocked(opts QueryOpts) map[string]string {
	shortcuts := make(map[string]string)
	for action := range ls.defaults["shortcuts"].(map[string]any) {
		val, _, ok := ls.valueLocked("shortcuts."+action, opts)
		if ok {
			if str, ok := val.(string); ok {
				shortcuts[action] = str
			}
		}
	}
	return shortcuts
}

func mergeMaps(dest, src map[string]any) {
	for k, v := range src {
		if srcMap, ok := v.(map[string]any); ok {
			if destMap, ok := dest[k].(map[string]any); ok {
				mergeMaps(destMap, srcMap)
			} else {
				dest[k] = cloneMap(srcMap)
			}
		} else {
			dest[k] = v
		}
	}
}
