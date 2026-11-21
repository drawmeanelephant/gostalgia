package services

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gostalgia/internal/ipc"
	"gostalgia/internal/profile"
	"gostalgia/internal/security"
	"gostalgia/internal/service"
)

// ProfileService exposes profile management, workspace identity, and profile switching over IPC.
// It owns the "profile/*" method namespace.
type ProfileService struct {
	ctx *service.Context
}

// NewProfile creates a new ProfileService.
func NewProfile() *ProfileService {
	return &ProfileService{}
}

func (s *ProfileService) Name() string { return "profile" }

func (s *ProfileService) Depends() []string {
	return []string{"fs"}
}

func (s *ProfileService) Init(ctx *service.Context) error {
	s.ctx = ctx
	return nil
}

func (s *ProfileService) Start(ctx context.Context) error {
	return s.ctx.Router.HandleRoutes(map[string]ipc.Route{
		"profile/list":   {Handler: s.list, Policy: ipc.OperatorOr(security.CapProfileRead, security.CapAdmin)},
		"profile/get":    {Handler: s.get, Policy: ipc.OperatorOr(security.CapProfileRead, security.CapAdmin)},
		"profile/active": {Handler: s.active, Policy: ipc.OperatorOr(security.CapProfileRead, security.CapAdmin)},
		"profile/create": {Handler: s.create, Policy: ipc.OperatorOr(security.CapProfileWrite, security.CapAdmin)},
		"profile/update": {Handler: s.update, Policy: ipc.OperatorOr(security.CapProfileWrite, security.CapAdmin)},
		"profile/switch": {Handler: s.switchProfile, Policy: ipc.OperatorOr(security.CapProfileWrite, security.CapAdmin)},
		"profile/delete": {Handler: s.deleteProfile, Policy: ipc.OperatorOr(security.CapProfileWrite, security.CapAdmin)},
	})
}

func (s *ProfileService) Stop(ctx context.Context) error {
	s.ctx.Router.UnhandlePrefix("profile/")
	return nil
}

type ProfileListResponse struct {
	Profiles []profile.Profile `json:"profiles"`
	Active   string            `json:"active"`
}

func (s *ProfileService) list(ctx context.Context, req ipc.Request) (any, error) {
	if s.ctx.Profiles == nil {
		return ProfileListResponse{Profiles: []profile.Profile{}, Active: ""}, nil
	}
	return ProfileListResponse{
		Profiles: s.ctx.Profiles.List(),
		Active:   s.ctx.Profiles.ActiveID(),
	}, nil
}

func (s *ProfileService) get(ctx context.Context, req ipc.Request) (any, error) {
	if s.ctx.Profiles == nil {
		return nil, fmt.Errorf("profile: manager unavailable")
	}

	var params struct {
		ID string `json:"id"`
	}
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return nil, fmt.Errorf("profile: invalid parameters: %w", err)
		}
	}
	if strings.TrimSpace(params.ID) == "" {
		params.ID = s.ctx.Profiles.ActiveID()
	}

	p, ok := s.ctx.Profiles.Get(params.ID)
	if !ok {
		return nil, fmt.Errorf("profile: profile %q not found", params.ID)
	}
	return p, nil
}

func (s *ProfileService) active(ctx context.Context, req ipc.Request) (any, error) {
	if s.ctx.Profiles == nil {
		return nil, fmt.Errorf("profile: manager unavailable")
	}
	return s.ctx.Profiles.Active(), nil
}

type CreateProfileRequest struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Avatar      string         `json:"avatar,omitempty"`
	Preferences map[string]any `json:"preferences,omitempty"`
}

func (s *ProfileService) create(ctx context.Context, req ipc.Request) (any, error) {
	if s.ctx.Profiles == nil {
		return nil, fmt.Errorf("profile: manager unavailable")
	}

	var params CreateProfileRequest
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return nil, fmt.Errorf("profile: invalid parameters: %w", err)
	}

	p, err := s.ctx.Profiles.CreateDetailed(params.ID, params.Name, params.Description, params.Avatar, params.Preferences)
	if err != nil {
		return nil, err
	}
	return p, nil
}

type UpdateProfileRequest struct {
	ID          string         `json:"id"`
	Name        string         `json:"name,omitempty"`
	Description string         `json:"description,omitempty"`
	Avatar      string         `json:"avatar,omitempty"`
	Preferences map[string]any `json:"preferences,omitempty"`
}

func (s *ProfileService) update(ctx context.Context, req ipc.Request) (any, error) {
	if s.ctx.Profiles == nil {
		return nil, fmt.Errorf("profile: manager unavailable")
	}

	var params UpdateProfileRequest
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return nil, fmt.Errorf("profile: invalid parameters: %w", err)
	}

	p, err := s.ctx.Profiles.Update(params.ID, params.Name, params.Description, params.Avatar, params.Preferences)
	if err != nil {
		return nil, err
	}
	return p, nil
}

type SwitchProfileRequest struct {
	ID string `json:"id"`
}

func (s *ProfileService) switchProfile(ctx context.Context, req ipc.Request) (any, error) {
	if s.ctx.Profiles == nil {
		return nil, fmt.Errorf("profile: manager unavailable")
	}

	var params SwitchProfileRequest
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return nil, fmt.Errorf("profile: invalid parameters: %w", err)
	}

	targetID := strings.TrimSpace(params.ID)
	if targetID == "" {
		return nil, fmt.Errorf("profile: target profile id is required")
	}

	// Switch runs the profile manager's switch callback list — the single
	// owner of the "active profile changed" side effects (runtime
	// identity, user config layer rebind, app launches, operator token,
	// session ensure). The service must not repeat them here: a second
	// binding used to point the user layer at the VFS path instead of its
	// host path.
	return s.ctx.Profiles.Switch(targetID)
}

type DeleteProfileRequest struct {
	ID string `json:"id"`
}

func (s *ProfileService) deleteProfile(ctx context.Context, req ipc.Request) (any, error) {
	if s.ctx.Profiles == nil {
		return nil, fmt.Errorf("profile: manager unavailable")
	}

	var params DeleteProfileRequest
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return nil, fmt.Errorf("profile: invalid parameters: %w", err)
	}

	targetID := strings.TrimSpace(strings.ToLower(params.ID))
	if targetID == "" {
		return nil, fmt.Errorf("profile: profile id is required")
	}

	// Pre-flight the guards Profiles.Delete applies: a delete that will be
	// refused must not tear down the target's live sessions.
	if _, ok := s.ctx.Profiles.Get(targetID); !ok {
		return nil, fmt.Errorf("profile %q not found", targetID)
	}
	if targetID == s.ctx.Profiles.ActiveID() {
		return nil, fmt.Errorf("cannot delete currently active profile %q; switch profiles first", targetID)
	}

	// Close the profile's sessions and retire its workspace store before
	// the data tree comes down: a still-live session would keep writing to
	// (and recreating) the deleted /users/<id> tree, and a profile
	// re-created under the same id would inherit its workspace history.
	if s.ctx.Sessions != nil {
		s.ctx.Sessions.CloseForUser(targetID)
	}

	if err := s.ctx.Profiles.Delete(targetID); err != nil {
		return nil, err
	}

	return map[string]any{
		"success": true,
		"id":      targetID,
	}, nil
}
