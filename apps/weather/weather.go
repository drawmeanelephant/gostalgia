// Package weather is Gostalgia's weather app: current conditions and a
// multi-day forecast for a bounded list of saved places, fetched over
// capability-gated egress from the keyless Open-Meteo API — no secrets,
// no account, no key infrastructure.
//
// Two hosts must be reachable: geocoding-api.open-meteo.com resolves
// place names and api.open-meteo.com serves forecasts. The manifest
// grants net.egress, but the operator policy must also enable network
// egress and allowlist both hosts; every denial surfaces in the status
// line with recovery guidance rather than a stack trace.
//
// Saved places and the last good forecast per place persist in the
// app-private partition (/apps/data/com.gostalgia.weather/state.json) via
// atomic fs/save. Fetches happen only inside actions — search, save,
// select, refresh — never in the passive view the shell polls, so a
// running app cannot hammer the API. A fresh cache (younger than
// cacheTTL) is served without refetching; a failed refresh keeps the
// stale snapshot and says how old it is.
package weather

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"strings"
	"sync"
	"time"

	"gostalgia/sdk"
)

// ID is the reverse-DNS identifier for Weather.
const ID = "com.gostalgia.weather"

// statePath is the single persisted document inside the app-private
// partition. fs/save stages and renames atomically, so a crash mid-write
// leaves the previous state intact.
const statePath = "/apps/data/com.gostalgia.weather/state.json"

const stateVersion = 1

// Bounds: small saved list, few search candidates, a work-week forecast,
// and a cache TTL that respects a free API. maxLabelLen bounds any single
// remote- or user-supplied string before it lands in a snapshot.
const (
	maxLocations     = 8
	maxSearchResults = 5
	maxForecastDays  = 5
	maxLabelLen      = 80
	cacheTTL         = 30 * time.Minute
)

// fetchTimeoutSecs bounds one net/fetch call; the service caps it at 60.
const fetchTimeoutSecs = 10

// Reserved item IDs that are notices or content rows, not places. Loaded
// location IDs that collide with one of these — or with the cand_/day_
// prefixes — are reassigned on sanitize.
const (
	itemCurrent = "current"
	itemEmpty   = "empty"
)

//go:embed manifest.json
var manifestJSON []byte

// Manifest returns the validated manifest declaration.
func Manifest() sdk.Manifest {
	m, err := sdk.ParseManifest(manifestJSON)
	if err != nil {
		panic(err) // invalid builtin manifest is a programming error
	}
	return m
}

// ManifestJSON returns a copy of the raw embedded JSON.
func ManifestJSON() []byte { return append([]byte(nil), manifestJSON...) }

// Location is one saved (or candidate) place. Coordinates are the query
// the forecast endpoint needs; the rest is presentation.
type Location struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Region    string    `json:"region,omitempty"`
	Country   string    `json:"country,omitempty"`
	Latitude  float64   `json:"latitude"`
	Longitude float64   `json:"longitude"`
	Timezone  string    `json:"timezone,omitempty"`
	AddedAt   time.Time `json:"added_at,omitempty"`
}

// Display renders "Paris, Île-de-France, France" — the region is dropped
// when it merely repeats the name.
func (l Location) Display() string {
	parts := []string{l.Name}
	if l.Region != "" && !strings.EqualFold(l.Region, l.Name) {
		parts = append(parts, l.Region)
	}
	if l.Country != "" {
		parts = append(parts, l.Country)
	}
	s := strings.Join(parts, ", ")
	if len(s) > 140 {
		s = strings.TrimSpace(s[:140])
	}
	return s
}

// Snapshot is the last good forecast for one location, persisted with it.
// FetchedAt is the local fetch instant — the honest staleness anchor —
// while ObservedAt is the API's own local-time reading stamp.
type Snapshot struct {
	FetchedAt  time.Time `json:"fetched_at"`
	ObservedAt string    `json:"observed_at,omitempty"`
	TempC      float64   `json:"temp_c"`
	FeelsC     float64   `json:"feels_c"`
	Humidity   float64   `json:"humidity_pct"`
	PrecipMM   float64   `json:"precip_mm"`
	Code       int       `json:"code"`
	WindKph    float64   `json:"wind_kph"`
	WindDirDeg float64   `json:"wind_dir_deg"`
	IsDay      bool      `json:"is_day"`
	Days       []Day     `json:"days,omitempty"`
}

// Day is one forecast row. PrecipPct is nil when the API reports null —
// rendered "—" rather than a fabricated zero.
type Day struct {
	Date      string   `json:"date"`
	Code      int      `json:"code"`
	MaxC      float64  `json:"max_c"`
	MinC      float64  `json:"min_c"`
	PrecipPct *float64 `json:"precip_pct,omitempty"`
}

// diskState is the persisted document: places, the selection, the id
// counter, and the last-good forecast per place.
type diskState struct {
	Version   int                 `json:"version"`
	Locations []Location          `json:"locations"`
	Selected  string              `json:"selected"`
	Seq       int                 `json:"seq"`
	Cache     map[string]Snapshot `json:"cache,omitempty"`
}

// Weather is the running in-process instance. All state is guarded by mu.
type Weather struct {
	app *sdk.Context
	now func() time.Time

	mu         sync.Mutex
	loaded     bool
	locations  []Location
	selected   string // location id, "" when none
	seq        int
	cache      map[string]Snapshot // last good forecast per location id
	candidates []Location          // transient geocode results, ids cand_N
	dirty      bool                // in-memory change has not reached disk
	statusNote string              // transient feedback line; cleared on each action
	lastError  string
}

// Factory constructs a fresh, uninitialized Weather instance.
func Factory() (sdk.Instance, error) { return &Weather{now: time.Now}, nil }

// setClock injects a deterministic clock for tests.
func setClock(w *Weather, now func() time.Time) { w.now = now }

// Init registers routes and the presentation callbacks. State loads
// lazily on first use so Init stays prompt.
func (w *Weather) Init(app *sdk.Context) error {
	w.app = app
	if w.now == nil {
		w.now = time.Now
	}
	for _, route := range []struct {
		name string
		h    sdk.Handler
	}{
		{"state", w.stateRoute},
		{"search", w.searchRoute},
		{"save", w.saveRoute},
		{"select", w.selectRoute},
		{"remove", w.removeRoute},
		{"refresh", w.refreshRoute},
	} {
		if err := app.Handle(route.name, route.h); err != nil {
			return err
		}
	}
	return app.Present(w.view, w.act)
}

// Run blocks until the instance is canceled. There is no live tick and no
// background poll: fetches happen only when a user action asks for them.
func (w *Weather) Run(ctx context.Context) error {
	if w.app != nil && w.app.Log != nil {
		w.app.Log.Info("weather running")
	}
	<-ctx.Done()
	return nil
}

// Stop flushes a pending save — the retry after an earlier save failure.
// A failed flush is logged, never fatal.
func (w *Weather) Stop(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.loaded && w.dirty {
		if err := w.persistLocked(ctx); err != nil && w.app != nil && w.app.Log != nil {
			w.app.Log.Warn("weather: final save failed", "err", err)
		}
	}
	return nil
}

// ensureLoadedLocked reads and decodes state.json once. A missing file is
// simply an empty list. A corrupt file is an honest error banner over an
// empty list; the bad bytes are left alone until the next real change
// saves over them. A denied read is also reported, since nothing will
// persist without the grant. Any other read failure is NOT "no save yet":
// the app reports an honest error and stays unloaded so the next call
// retries — an empty list must never overwrite a save it could not read.
func (w *Weather) ensureLoadedLocked(ctx context.Context) error {
	if w.loaded {
		return nil
	}
	w.cache = map[string]Snapshot{}

	var out struct {
		DataBase64 string `json:"data_base64"`
	}
	err := w.app.Call(ctx, "fs/read", map[string]string{"path": statePath}, &out)
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			// no save yet: empty list
		case errors.Is(err, fs.ErrPermission):
			w.lastError = fmt.Sprintf("storage access denied (%v) — places will not be saved", err)
		default:
			return fmt.Errorf("saved weather state could not be loaded: %w", err)
		}
		w.loaded = true
		return nil
	}
	w.loaded = true
	data, err := base64.StdEncoding.DecodeString(out.DataBase64)
	if err != nil {
		w.lastError = "saved weather state is unreadable — starting fresh; the next change saves over it"
		return nil
	}
	var st diskState
	if err := json.Unmarshal(data, &st); err != nil || st.Version != stateVersion {
		w.lastError = "saved weather state is unreadable — starting fresh; the next change saves over it"
		return nil
	}
	w.locations = st.Locations
	w.selected = st.Selected
	w.seq = st.Seq
	if st.Cache != nil {
		w.cache = st.Cache
	}
	if w.sanitizeLocked() {
		// The loaded file needed repair; write the healed copy back now so
		// a crash does not reload the same damage. Best-effort: the in-memory
		// state is already sound, so a failure only shows up on the banner.
		if err := w.persistLocked(ctx); err != nil {
			w.lastError = fmt.Sprintf("save failed: %v", err)
		}
	}
	return nil
}

// sanitizeLocked repairs loaded state in place and reports whether it
// changed anything: seq pushed past every loc_N id, places with dishonest
// coordinates dropped, ids that are empty, duplicated, or collide with
// reserved item ids reassigned, text normalized, timestamps pinned, the
// cache pruned to live location ids, and the selection pinned to a real
// place (the first, when the stored pick is gone).
func (w *Weather) sanitizeLocked() bool {
	changed := false
	for _, l := range w.locations {
		var n int
		if _, err := fmt.Sscanf(l.ID, "loc_%d", &n); err == nil && n > w.seq {
			w.seq = n
		}
	}
	if len(w.locations) > maxLocations {
		w.locations = w.locations[:maxLocations]
		changed = true
	}
	now := w.now()
	seen := map[string]bool{itemCurrent: true, itemEmpty: true}
	kept := w.locations[:0]
	for _, l := range w.locations {
		if math.IsNaN(l.Latitude) || math.IsNaN(l.Longitude) ||
			l.Latitude < -90 || l.Latitude > 90 ||
			l.Longitude < -180 || l.Longitude > 180 {
			changed = true
			continue // a place without honest coordinates cannot be fetched
		}
		name := sanitizeLine(l.Name)
		if name == "" {
			name = "(unnamed place)"
		}
		if name != l.Name {
			l.Name = name
			changed = true
		}
		if s := sanitizeLine(l.Region); s != l.Region {
			l.Region = s
			changed = true
		}
		if s := sanitizeLine(l.Country); s != l.Country {
			l.Country = s
			changed = true
		}
		if s := sanitizeLine(l.Timezone); s != l.Timezone {
			l.Timezone = s
			changed = true
		}
		if l.ID == "" || seen[l.ID] ||
			strings.HasPrefix(l.ID, "cand_") || strings.HasPrefix(l.ID, "day_") {
			w.seq++
			l.ID = fmt.Sprintf("loc_%d", w.seq)
			changed = true
		}
		seen[l.ID] = true
		if l.AddedAt.IsZero() || l.AddedAt.After(now) {
			l.AddedAt = now
			changed = true
		}
		kept = append(kept, l)
	}
	w.locations = kept
	if w.cache == nil {
		w.cache = map[string]Snapshot{}
	}
	ids := make(map[string]bool, len(w.locations))
	for _, l := range w.locations {
		ids[l.ID] = true
	}
	for id, snap := range w.cache {
		if !ids[id] {
			delete(w.cache, id)
			changed = true
			continue
		}
		if snap.FetchedAt.IsZero() || snap.FetchedAt.After(now) {
			snap.FetchedAt = now
			w.cache[id] = snap
			changed = true
		}
		if len(snap.Days) > maxForecastDays {
			snap.Days = snap.Days[:maxForecastDays]
			w.cache[id] = snap
			changed = true
		}
	}
	if w.selected != "" && !ids[w.selected] {
		w.selected = ""
		changed = true
	}
	if w.selected == "" && len(w.locations) > 0 {
		w.selected = w.locations[0].ID
		changed = true
	}
	return changed
}

// persistLocked writes the whole state document atomically. Callers hold mu.
func (w *Weather) persistLocked(ctx context.Context) error {
	locs := w.locations
	if locs == nil {
		locs = []Location{}
	}
	data, err := json.Marshal(diskState{
		Version:   stateVersion,
		Locations: locs,
		Selected:  w.selected,
		Seq:       w.seq,
		Cache:     w.cache,
	})
	if err != nil {
		return err
	}
	err = w.app.Call(ctx, "fs/save", map[string]any{
		"path":        statePath,
		"data_base64": base64.StdEncoding.EncodeToString(data),
	}, nil)
	if err == nil {
		w.dirty = false
	}
	return err
}

// --- fetch seam: net/fetch under net.egress ---

// describeFetchError turns the egress error surface into operator-facing
// guidance. The raw policy strings come from internal/security and
// internal/services/net.go; transport errors already read plainly.
func describeFetchError(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, `missing capability "net.egress"`):
		return "this install lacks the net.egress grant"
	case strings.Contains(msg, "network egress is disabled"):
		return fmt.Sprintf("network egress is off — an operator must enable it (sys/policy/update) and allowlist %s and %s", geocodeHost, forecastHost)
	case strings.Contains(msg, "not in allowed hosts"), strings.Contains(msg, "blocked by policy"):
		return fmt.Sprintf("the weather API hosts are not allowlisted — add %s and %s to network.allowed_hosts", geocodeHost, forecastHost)
	case strings.Contains(msg, "plaintext HTTP"):
		return "operator policy requires HTTPS for egress"
	case strings.Contains(msg, "response size exceeds"):
		return "the service response was too large to read"
	default:
		return msg
	}
}

// fetchLocked issues one bounded GET through net/fetch and returns the
// decoded body. Errors are already phrased for the status line; Open-Meteo
// error bodies ({"error":true,"reason":"..."}) surface their reason.
func (w *Weather) fetchLocked(ctx context.Context, urlStr string) ([]byte, error) {
	var out struct {
		Status     int    `json:"status"`
		StatusText string `json:"status_text"`
		DataBase64 string `json:"data_base64"`
	}
	if err := w.app.Call(ctx, "net/fetch", map[string]any{
		"url":             urlStr,
		"timeout_seconds": fetchTimeoutSecs,
	}, &out); err != nil {
		return nil, errors.New(describeFetchError(err))
	}
	body, err := base64.StdEncoding.DecodeString(out.DataBase64)
	if err != nil {
		return nil, fmt.Errorf("the response body was not readable")
	}
	if out.Status != 200 {
		var fail struct {
			Reason string `json:"reason"`
		}
		if json.Unmarshal(body, &fail) == nil && fail.Reason != "" {
			return nil, fmt.Errorf("the service returned HTTP %d: %s", out.Status, sanitizeLine(fail.Reason))
		}
		return nil, fmt.Errorf("the service returned HTTP %d", out.Status)
	}
	return body, nil
}

// ensureFreshLocked makes the selected location's cache current: a hit
// younger than cacheTTL skips the fetch unless forced, a miss or stale
// entry refetches. A failed fetch keeps the stale snapshot; the error
// reaches the caller so the note can say how old the shown data is.
func (w *Weather) ensureFreshLocked(ctx context.Context, locID string, force bool) error {
	loc := w.findLocationLocked(locID)
	if loc == nil {
		return fmt.Errorf("no saved place %q", locID)
	}
	snap, cached := w.cache[locID]
	if cached && !force && w.now().Sub(snap.FetchedAt) < cacheTTL {
		return nil
	}
	data, err := w.fetchLocked(ctx, forecastURL(*loc))
	if err != nil {
		return err
	}
	parsed, err := parseForecast(data, w.now())
	if err != nil {
		return err
	}
	w.cache[locID] = parsed
	w.dirty = true
	return nil
}

// --- mutations: error-returning rejections, status-note outcomes ---

func (w *Weather) findLocationLocked(id string) *Location {
	for i := range w.locations {
		if w.locations[i].ID == id {
			return &w.locations[i]
		}
	}
	return nil
}

func (w *Weather) findCandidateLocked(id string) *Location {
	for i := range w.candidates {
		if w.candidates[i].ID == id {
			return &w.candidates[i]
		}
	}
	return nil
}

// findSimilarLocked matches a candidate to an existing place by name and
// near-identical coordinates, so re-saving a place selects it instead of
// duplicating it.
func (w *Weather) findSimilarLocked(cand Location) *Location {
	for i := range w.locations {
		l := &w.locations[i]
		if strings.EqualFold(l.Name, cand.Name) &&
			math.Abs(l.Latitude-cand.Latitude) < 0.01 &&
			math.Abs(l.Longitude-cand.Longitude) < 0.01 {
			return l
		}
	}
	return nil
}

// finishLocked persists once, then composes the status line from the
// mutation outcome and any fetch failure. A failed save keeps the change
// in memory (dirty) and says so; a failed fetch names the staleness of
// whatever data remains.
func (w *Weather) finishLocked(ctx context.Context, okNote string, fetchErr error) {
	perr := w.persistLocked(ctx)
	switch {
	case perr != nil && fetchErr != nil:
		w.statusNote = fmt.Sprintf("%s · kept in memory, save failed: %v · %s", okNote, perr, fetchErr)
	case perr != nil:
		w.statusNote = fmt.Sprintf("%s · kept in memory, save failed: %v", okNote, perr)
	case fetchErr != nil:
		if snap, ok := w.cache[w.selected]; ok {
			w.statusNote = fmt.Sprintf("%s · %s — showing data from %s", okNote, fetchErr, ago(w.now().Sub(snap.FetchedAt)))
		} else {
			w.statusNote = fmt.Sprintf("%s · %s", okNote, fetchErr)
		}
	default:
		w.statusNote = okNote
	}
}

// searchLocked geocodes the field text into bounded candidates. The
// candidate list is transient: a new search replaces it, a save consumes it.
func (w *Weather) searchLocked(ctx context.Context, query string) error {
	query = sanitizeLine(query)
	if query == "" {
		return fmt.Errorf("type a place name to search")
	}
	data, err := w.fetchLocked(ctx, geocodeURL(query))
	if err != nil {
		return fmt.Errorf("search failed: %w", err)
	}
	cands, err := parseGeocode(data)
	if err != nil {
		return fmt.Errorf("search failed: %w", err)
	}
	w.candidates = cands
	if len(cands) == 0 {
		w.statusNote = fmt.Sprintf("no places match %q — check the spelling or try a larger nearby town", query)
		return nil
	}
	w.statusNote = fmt.Sprintf("%d place%s match %q — select one and press Save result", len(cands), plural(len(cands)), query)
	return nil
}

// saveCandidateLocked keeps the selected search result as a saved place,
// selects it, and loads its forecast. Dedupe by name+coordinates beats
// the cap check so re-saving a place is a select, not a rejection.
func (w *Weather) saveCandidateLocked(ctx context.Context, itemID string) error {
	cand := w.findCandidateLocked(itemID)
	if cand == nil {
		if w.findLocationLocked(itemID) != nil {
			return fmt.Errorf("that place is already saved")
		}
		return fmt.Errorf("select a search result to save")
	}
	if dup := w.findSimilarLocked(*cand); dup != nil {
		w.selected = dup.ID
		w.candidates = nil
		w.dirty = true
		fetchErr := w.ensureFreshLocked(ctx, dup.ID, false)
		w.finishLocked(ctx, fmt.Sprintf("%s was already saved — showing it", dup.Display()), fetchErr)
		return nil
	}
	if len(w.locations) >= maxLocations {
		return fmt.Errorf("the saved list is full (%d places) — remove one first", maxLocations)
	}
	w.seq++
	loc := *cand
	loc.ID = fmt.Sprintf("loc_%d", w.seq)
	loc.AddedAt = w.now()
	w.locations = append(w.locations, loc)
	w.selected = loc.ID
	w.candidates = nil
	w.dirty = true
	fetchErr := w.ensureFreshLocked(ctx, loc.ID, false)
	okNote := fmt.Sprintf("saved %s", loc.Display())
	if fetchErr == nil {
		snap := w.cache[loc.ID]
		okNote = fmt.Sprintf("saved %s — %s, %.0f°C", loc.Display(), describeCode(snap.Code), snap.TempC)
	}
	w.finishLocked(ctx, okNote, fetchErr)
	return nil
}

// selectLocationLocked switches the shown place and refreshes it if the
// cache went stale while another place was selected.
func (w *Weather) selectLocationLocked(ctx context.Context, itemID string) error {
	if w.findLocationLocked(itemID) == nil {
		if w.findCandidateLocked(itemID) != nil {
			return fmt.Errorf("that's a search result — press Save result to keep it")
		}
		return fmt.Errorf("select a saved place first")
	}
	if w.selected != itemID {
		w.selected = itemID
		w.dirty = true
	}
	loc := w.findLocationLocked(itemID)
	fetchErr := w.ensureFreshLocked(ctx, itemID, false)
	w.finishLocked(ctx, fmt.Sprintf("showing %s", loc.Display()), fetchErr)
	return nil
}

// removeLocationLocked drops a saved place and its cached forecast. A
// removed selection falls back to the first remaining place.
func (w *Weather) removeLocationLocked(ctx context.Context, itemID string) error {
	loc := w.findLocationLocked(itemID)
	if loc == nil {
		if w.findCandidateLocked(itemID) != nil {
			return fmt.Errorf("search results aren't saved yet — nothing to remove")
		}
		return fmt.Errorf("select a saved place first")
	}
	name := loc.Display()
	kept := w.locations[:0]
	for _, l := range w.locations {
		if l.ID != itemID {
			kept = append(kept, l)
		}
	}
	w.locations = kept
	delete(w.cache, itemID)
	if w.selected == itemID {
		w.selected = ""
		if len(w.locations) > 0 {
			w.selected = w.locations[0].ID
		}
	}
	w.dirty = true
	w.finishLocked(ctx, fmt.Sprintf("removed %s", name), nil)
	return nil
}

// refreshLocked forces a refetch of the selected place, TTL or not.
func (w *Weather) refreshLocked(ctx context.Context) error {
	loc := w.findLocationLocked(w.selected)
	if loc == nil {
		return fmt.Errorf("save a place first — search below")
	}
	fetchErr := w.ensureFreshLocked(ctx, loc.ID, true)
	note := fmt.Sprintf("updated %s just now", loc.Display())
	if fetchErr != nil {
		note = fmt.Sprintf("couldn't refresh %s", loc.Display())
	}
	w.finishLocked(ctx, note, fetchErr)
	return nil
}

// --- presentation ---

func (w *Weather) view(ctx context.Context) (sdk.View, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.ensureLoadedLocked(ctx); err != nil {
		return sdk.View{Title: "Weather", State: sdk.ViewError, Error: err.Error()}, nil
	}
	return w.viewLocked(sdk.PresentationRequestVersion(ctx)), nil
}

func (w *Weather) act(ctx context.Context, req sdk.ActionRequest) (sdk.View, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.ensureLoadedLocked(ctx); err != nil {
		return sdk.View{Title: "Weather", State: sdk.ViewError, Error: err.Error()}, nil
	}
	w.lastError = ""
	w.statusNote = ""

	var err error
	switch req.Action {
	case "search":
		err = w.searchLocked(ctx, req.Values["place"])
	case "save":
		err = w.saveCandidateLocked(ctx, req.ItemID)
	case "select":
		err = w.selectLocationLocked(ctx, req.ItemID)
	case "remove":
		err = w.removeLocationLocked(ctx, req.ItemID)
	case "refresh":
		err = w.refreshLocked(ctx)
	default:
		return sdk.View{}, fmt.Errorf("unknown action %q", req.Action)
	}
	if err != nil {
		// Domain rejections land in the status line, not the error banner:
		// the saved list and the cache are still fully usable.
		w.statusNote = err.Error()
	}
	return w.viewLocked(sdk.PresentationRequestVersion(ctx)), nil
}

func (w *Weather) viewLocked(version int) sdk.View {
	now := w.now()
	loc := w.findLocationLocked(w.selected)
	snap, hasSnap := w.cache[w.selected]

	items := make([]sdk.Item, 0, len(w.locations)+len(w.candidates)+maxForecastDays+1)
	if loc != nil && hasSnap {
		items = append(items, sdk.Item{
			ID:    itemCurrent,
			Label: fmt.Sprintf("Now · %.0f°C · %s", snap.TempC, describeCode(snap.Code)),
			Detail: fmt.Sprintf("feels %.0f°C · humidity %.0f%% · wind %.0f km/h %s · precip %.1f mm · observed %s",
				snap.FeelsC, snap.Humidity, snap.WindKph, compass(snap.WindDirDeg), snap.PrecipMM, observedShort(snap.ObservedAt)),
		})
		for i, d := range snap.Days {
			items = append(items, sdk.Item{
				ID:     fmt.Sprintf("day_%d", i),
				Label:  fmt.Sprintf("%s · %s", shortDate(d.Date), describeCode(d.Code)),
				Detail: fmt.Sprintf("low %.0f° · high %.0f° · precip %s", d.MinC, d.MaxC, precipText(d.PrecipPct)),
			})
		}
	} else if loc != nil {
		items = append(items, sdk.Item{
			ID:     itemCurrent,
			Label:  "No data for " + loc.Display(),
			Detail: "press Refresh to load conditions",
		})
	}
	for _, l := range w.locations {
		label := l.Display()
		if l.ID == w.selected {
			label = "» " + label
		}
		detail := "saved " + ago(now.Sub(l.AddedAt))
		if s, ok := w.cache[l.ID]; ok {
			detail += " · updated " + ago(now.Sub(s.FetchedAt))
		} else {
			detail += " · no data yet"
		}
		items = append(items, sdk.Item{ID: l.ID, Label: label, Detail: detail})
	}
	for _, c := range w.candidates {
		items = append(items, sdk.Item{
			ID:     c.ID,
			Label:  c.Display(),
			Detail: fmt.Sprintf("%.4f, %.4f — select and press Save result", c.Latitude, c.Longitude),
		})
	}
	if len(items) == 0 {
		items = append(items, sdk.Item{
			ID:     itemEmpty,
			Label:  "No saved places",
			Detail: "type a place name below and press Search",
		})
	}

	title := "Weather"
	if loc != nil && hasSnap {
		title = fmt.Sprintf("Weather — %s %.0f°C", loc.Display(), snap.TempC)
	} else if loc != nil {
		title = "Weather — " + loc.Display()
	}

	v := sdk.View{
		Title:  title,
		State:  sdk.ViewReady,
		Items:  items,
		Fields: []sdk.Field{{ID: "place", Label: "Place name"}},
		Actions: []sdk.Action{
			{ID: "search", Label: "Search"},
			{ID: "save", Label: "Save result", Disabled: len(w.candidates) == 0},
			{ID: "select", Label: "Select place", Disabled: len(w.locations) == 0},
			{ID: "remove", Label: "Remove place", Disabled: len(w.locations) == 0},
			{ID: "refresh", Label: "Refresh", Disabled: loc == nil},
		},
		Status: w.statusLocked(now),
		Error:  w.lastError,
	}
	if version >= 2 && loc != nil && hasSnap {
		v.Blocks = []sdk.Block{{
			ID:    "weather_art",
			Label: fmt.Sprintf("%s — %s", loc.Display(), describeCode(snap.Code)),
			Text:  glyphFor(snap.Code, snap.IsDay),
		}}
		meters := []sdk.Meter{{ID: "m_humidity", Label: "Humidity", Value: snap.Humidity / 100}}
		if len(snap.Days) > 0 && snap.Days[0].PrecipPct != nil {
			meters = append(meters, sdk.Meter{ID: "m_precip", Label: "Precip today", Value: *snap.Days[0].PrecipPct / 100})
		}
		v.Meters = meters
	}
	return v
}

// statusLocked picks the line under the list: transient notes win, then
// the honest summary of the selection and the cache's age.
func (w *Weather) statusLocked(now time.Time) string {
	if w.statusNote != "" {
		return w.statusNote
	}
	loc := w.findLocationLocked(w.selected)
	if loc == nil {
		if len(w.locations) == 0 {
			return "no saved places — search for one below"
		}
		return "select a saved place"
	}
	snap, ok := w.cache[w.selected]
	if !ok {
		return fmt.Sprintf("no data for %s yet — press Refresh", loc.Display())
	}
	age := now.Sub(snap.FetchedAt)
	if age >= cacheTTL || age < 0 {
		return fmt.Sprintf("%s · data is %s old (stale — press Refresh)", loc.Display(), ago(age))
	}
	return fmt.Sprintf("%s · updated %s", loc.Display(), ago(age))
}

// observedShort renders the API's local-time stamp as just its clock part.
func observedShort(observed string) string {
	if i := strings.Index(observed, "T"); i >= 0 && i+1 < len(observed) {
		return observed[i+1:]
	}
	if observed == "" {
		return "unknown"
	}
	return observed
}

func precipText(pct *float64) string {
	if pct == nil {
		return "—"
	}
	return fmt.Sprintf("%.0f%%", *pct)
}

// ago renders a duration compactly: "just now", "34m ago", "6h ago", "2d ago".
func ago(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours())/24)
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// --- programmatic IPC routes ---

func (w *Weather) stateRoute(ctx context.Context, _ json.RawMessage) (any, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.ensureLoadedLocked(ctx); err != nil {
		return nil, err
	}
	cacheAge := map[string]string{}
	for id, snap := range w.cache {
		cacheAge[id] = ago(w.now().Sub(snap.FetchedAt))
	}
	return map[string]any{
		"locations":  w.locations,
		"selected":   w.selected,
		"candidates": w.candidates,
		"cache_age":  cacheAge,
		"state_path": statePath,
		"egress_hosts": []string{
			geocodeHost, forecastHost,
		},
	}, nil
}

func (w *Weather) searchRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var params struct {
		Query string `json:"query"`
	}
	if err := sdk.DecodeParams(raw, &params); err != nil {
		return nil, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.ensureLoadedLocked(ctx); err != nil {
		return nil, err
	}
	if err := w.searchLocked(ctx, params.Query); err != nil {
		return nil, err
	}
	return map[string]any{"candidates": w.candidates, "count": len(w.candidates)}, nil
}

func (w *Weather) saveRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var params struct {
		ID string `json:"id"`
	}
	if err := sdk.DecodeParams(raw, &params); err != nil {
		return nil, err
	}
	if params.ID == "" {
		return nil, fmt.Errorf("params.id is required")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.ensureLoadedLocked(ctx); err != nil {
		return nil, err
	}
	if err := w.saveCandidateLocked(ctx, params.ID); err != nil {
		return nil, err
	}
	return map[string]any{"selected": w.selected, "note": w.statusNote}, nil
}

func (w *Weather) selectRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var params struct {
		ID string `json:"id"`
	}
	if err := sdk.DecodeParams(raw, &params); err != nil {
		return nil, err
	}
	if params.ID == "" {
		return nil, fmt.Errorf("params.id is required")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.ensureLoadedLocked(ctx); err != nil {
		return nil, err
	}
	if err := w.selectLocationLocked(ctx, params.ID); err != nil {
		return nil, err
	}
	return map[string]any{"selected": w.selected, "note": w.statusNote}, nil
}

func (w *Weather) removeRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var params struct {
		ID string `json:"id"`
	}
	if err := sdk.DecodeParams(raw, &params); err != nil {
		return nil, err
	}
	if params.ID == "" {
		return nil, fmt.Errorf("params.id is required")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.ensureLoadedLocked(ctx); err != nil {
		return nil, err
	}
	if err := w.removeLocationLocked(ctx, params.ID); err != nil {
		return nil, err
	}
	return map[string]any{"id": params.ID, "removed": true, "selected": w.selected}, nil
}

func (w *Weather) refreshRoute(ctx context.Context, _ json.RawMessage) (any, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.ensureLoadedLocked(ctx); err != nil {
		return nil, err
	}
	if err := w.refreshLocked(ctx); err != nil {
		return nil, err
	}
	return map[string]any{"selected": w.selected, "note": w.statusNote}, nil
}
