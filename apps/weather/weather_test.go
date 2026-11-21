package weather

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gostalgia/sdk"
)

var t0 = time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)

// fetchReply is what a stubbed net/fetch hands back: a transport/policy
// error, or an HTTP status plus body. No test touches the live network —
// the seam is the "net/fetch" IPC call itself.
type fetchReply struct {
	status int
	body   []byte
	err    error
}

// harness models the app-private VFS partition and the net/fetch route,
// with an injectable clock and a scripted fetch. No test sleeps: time
// moves via h.now.
type harness struct {
	now        time.Time
	files      map[string][]byte
	fetch      func(url string) fetchReply
	fetchCalls []string
	denyFS     bool
	readErr    error // if set, the next fs/read fails once with this error
	seq        atomic.Int64

	app      *Weather
	handlers map[string]sdk.Handler
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return b
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		now:      t0,
		files:    map[string][]byte{},
		handlers: map[string]sdk.Handler{},
	}
	geocodeBody := fixture(t, "geocode_paris.json")
	forecastBody := fixture(t, "forecast_paris.json")
	h.fetch = func(u string) fetchReply {
		switch {
		case strings.Contains(u, geocodeHost):
			return fetchReply{status: 200, body: geocodeBody}
		case strings.Contains(u, forecastHost):
			return fetchReply{status: 200, body: forecastBody}
		}
		return fetchReply{err: fmt.Errorf("unexpected fetch url %q", u)}
	}
	inst, err := Factory()
	if err != nil {
		t.Fatal(err)
	}
	w := inst.(*Weather)
	setClock(w, func() time.Time { return h.now })
	ctx := sdk.NewContext(Manifest(), slog.New(slog.NewTextHandler(io.Discard, nil)), h.call,
		func(name string, hd sdk.Handler) error { h.handlers[name] = hd; return nil })
	if err := w.Init(ctx); err != nil {
		t.Fatal(err)
	}
	h.app = w
	return h
}

func (h *harness) call(_ context.Context, method string, params, out any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	var p struct {
		Path string `json:"path"`
		Data string `json:"data_base64"`
		URL  string `json:"url"`
	}
	_ = json.Unmarshal(raw, &p)
	reply := func(v any) error {
		if out == nil {
			return nil
		}
		b, _ := json.Marshal(v)
		return json.Unmarshal(b, out)
	}
	switch method {
	case "fs/read":
		if h.readErr != nil {
			err := h.readErr
			h.readErr = nil
			return err
		}
		if h.denyFS {
			return &sdk.Error{Code: sdk.CodePermissionDenied, Message: "permission denied: missing capability \"fs.read\""}
		}
		data, ok := h.files[p.Path]
		if !ok {
			return &sdk.Error{Code: sdk.CodeNotFound, Message: fmt.Sprintf("not_found: %s", p.Path)}
		}
		return reply(map[string]any{"path": p.Path, "data_base64": base64.StdEncoding.EncodeToString(data)})
	case "fs/save":
		if h.denyFS {
			return errors.New("permission denied: missing capability \"fs.write\"")
		}
		data, err := base64.StdEncoding.DecodeString(p.Data)
		if err != nil {
			return err
		}
		h.files[p.Path] = data
		return reply(map[string]any{"path": p.Path, "saved": true})
	case "net/fetch":
		h.fetchCalls = append(h.fetchCalls, p.URL)
		res := h.fetch(p.URL)
		if res.err != nil {
			return res.err
		}
		return reply(map[string]any{
			"status":      res.status,
			"status_text": fmt.Sprintf("%d", res.status),
			"data_base64": base64.StdEncoding.EncodeToString(res.body),
			"size":        len(res.body),
		})
	}
	return fmt.Errorf("unknown method %s", method)
}

func (h *harness) advance(d time.Duration) { h.now = h.now.Add(d) }

func (h *harness) view(t *testing.T, version int) sdk.View {
	t.Helper()
	res, err := h.handlers["view"](context.Background(), json.RawMessage(fmt.Sprintf(`{"version":%d}`, version)))
	if err != nil {
		t.Fatalf("view v%d: %v", version, err)
	}
	v := res.(sdk.View)
	if err := v.Validate(); err != nil {
		t.Fatalf("view v%d does not validate: %v", version, err)
	}
	return v
}

func (h *harness) act(t *testing.T, version int, action, item string, values map[string]string) sdk.View {
	t.Helper()
	v, err := h.tryAct(version, action, item, values)
	if err != nil {
		t.Fatalf("action %s: %v", action, err)
	}
	return v
}

func (h *harness) tryAct(version int, action, item string, values map[string]string) (sdk.View, error) {
	cur, err := h.handlers["view"](context.Background(), json.RawMessage(fmt.Sprintf(`{"version":%d}`, version)))
	if err != nil {
		return sdk.View{}, err
	}
	req := sdk.ActionRequest{
		Version:   version,
		Instance:  cur.(sdk.View).Instance,
		RequestID: fmt.Sprintf("req_%d", h.seq.Add(1)),
		Action:    action,
		ItemID:    item,
		Values:    values,
	}
	raw, _ := json.Marshal(req)
	res, err := h.handlers["action"](context.Background(), raw)
	if err != nil {
		return sdk.View{}, err
	}
	v := res.(sdk.View)
	if err := v.Validate(); err != nil {
		return sdk.View{}, fmt.Errorf("action view does not validate: %w", err)
	}
	return v, nil
}

func (h *harness) route(t *testing.T, name string, params, out any) {
	t.Helper()
	hd, ok := h.handlers[name]
	if !ok {
		t.Fatalf("route %q not registered", name)
	}
	raw, _ := json.Marshal(params)
	res, err := hd(context.Background(), raw)
	if err != nil {
		t.Fatalf("route %s: %v", name, err)
	}
	if out != nil {
		b, _ := json.Marshal(res)
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("route %s decode: %v", name, err)
		}
	}
}

// failRoute invokes a route expected to error and returns the error text.
func (h *harness) failRoute(t *testing.T, name string, params any) string {
	t.Helper()
	hd, ok := h.handlers[name]
	if !ok {
		t.Fatalf("route %q not registered", name)
	}
	raw, _ := json.Marshal(params)
	_, err := hd(context.Background(), raw)
	if err == nil {
		t.Fatalf("route %s should have failed", name)
	}
	return err.Error()
}

// readState decodes the persisted document straight from the fake fs.
func (h *harness) readState(t *testing.T) diskState {
	t.Helper()
	data, ok := h.files[statePath]
	if !ok {
		t.Fatal("state file was not written")
	}
	var st diskState
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("persisted state is not valid JSON: %v", err)
	}
	return st
}

// failFetch swaps in a fetch that always errors with msg.
func (h *harness) failFetch(msg string) {
	h.fetch = func(u string) fetchReply {
		return fetchReply{err: errors.New(msg)}
	}
}

// failHost errors only fetches to the given host; other hosts still
// serve the default fixtures. The "//host/" match keeps the geocoding
// host (geocoding-api.open-meteo.com) distinct from the forecast host
// (api.open-meteo.com), which is its suffix.
func (h *harness) failHost(t *testing.T, host, msg string) {
	t.Helper()
	good := h.fetch
	h.fetch = func(u string) fetchReply {
		if strings.Contains(u, "//"+host+"/") {
			return fetchReply{err: errors.New(msg)}
		}
		return good(u)
	}
}

func findItem(v sdk.View, id string) *sdk.Item {
	for i := range v.Items {
		if v.Items[i].ID == id {
			return &v.Items[i]
		}
	}
	return nil
}

func findAction(v sdk.View, id string) *sdk.Action {
	for i := range v.Actions {
		if v.Actions[i].ID == id {
			return &v.Actions[i]
		}
	}
	return nil
}

func findMeter(v sdk.View, id string) *sdk.Meter {
	for i := range v.Meters {
		if v.Meters[i].ID == id {
			return &v.Meters[i]
		}
	}
	return nil
}

// saveFirst runs search + save of cand_0 and returns the post-save view.
func (h *harness) saveFirst(t *testing.T, version int) sdk.View {
	t.Helper()
	h.act(t, version, "search", "", map[string]string{"place": "paris"})
	return h.act(t, version, "save", "cand_0", nil)
}

// --- pure parsing tests (fixtures, no network) ---

func TestGeocodeParsesFixture(t *testing.T) {
	cands, err := parseGeocode(fixture(t, "geocode_paris.json"))
	if err != nil {
		t.Fatalf("parseGeocode: %v", err)
	}
	// Seven entries in the fixture: six valid plus one empty-name row the
	// parser must drop, then the bound caps the list at five.
	if len(cands) != maxSearchResults {
		t.Fatalf("candidates = %d, want %d", len(cands), maxSearchResults)
	}
	first := cands[0]
	if first.ID != "cand_0" || first.Name != "Paris" || first.Country != "France" {
		t.Fatalf("first candidate = %+v", first)
	}
	if first.Region != "Île-de-France" || first.Timezone != "Europe/Paris" {
		t.Errorf("first candidate region/timezone = %+v", first)
	}
	if first.Latitude != 48.8534 || first.Longitude != 2.3488 {
		t.Errorf("first candidate coords = %+v", first)
	}
	for _, c := range cands {
		if !strings.HasPrefix(c.ID, "cand_") {
			t.Errorf("candidate id %q lacks cand_ prefix", c.ID)
		}
		if c.Name == "" {
			t.Error("candidate with empty name was not dropped")
		}
	}
}

func TestGeocodeEmptyAndErrorShapes(t *testing.T) {
	cands, err := parseGeocode([]byte(`{}`))
	if err != nil || len(cands) != 0 {
		t.Fatalf("empty geocode = %v, %v", cands, err)
	}
	if _, err := parseGeocode([]byte(`{"error":true,"reason":"name is too short"}`)); err == nil ||
		!strings.Contains(err.Error(), "name is too short") {
		t.Fatalf("api error shape should surface reason, got %v", err)
	}
	if _, err := parseGeocode([]byte(`{not json`)); err == nil {
		t.Fatal("malformed geocode body should error")
	}
}

func TestForecastParsesFixture(t *testing.T) {
	snap, err := parseForecast(fixture(t, "forecast_paris.json"), t0)
	if err != nil {
		t.Fatalf("parseForecast: %v", err)
	}
	if snap.FetchedAt != t0 {
		t.Errorf("FetchedAt = %v, want %v", snap.FetchedAt, t0)
	}
	if snap.ObservedAt != "2026-10-08T10:00" {
		t.Errorf("ObservedAt = %q", snap.ObservedAt)
	}
	if snap.TempC != 14.2 || snap.FeelsC != 12.8 || snap.Humidity != 77 {
		t.Errorf("current = %+v", snap)
	}
	if snap.Code != 3 || snap.WindKph != 11.5 || snap.WindDirDeg != 230 || !snap.IsDay {
		t.Errorf("current code/wind/day = %+v", snap)
	}
	// Seven days in the fixture are capped at maxForecastDays.
	if len(snap.Days) != maxForecastDays {
		t.Fatalf("days = %d, want %d", len(snap.Days), maxForecastDays)
	}
	if snap.Days[0].Date != "2026-10-08" || snap.Days[0].Code != 3 {
		t.Errorf("day0 = %+v", snap.Days[0])
	}
	if snap.Days[0].PrecipPct == nil || *snap.Days[0].PrecipPct != 10 {
		t.Errorf("day0 precip = %+v, want 10", snap.Days[0].PrecipPct)
	}
	// The null precip day survives as nil — honest "—", not a fake zero.
	if snap.Days[3].PrecipPct != nil {
		t.Errorf("day3 precip = %v, want nil", *snap.Days[3].PrecipPct)
	}
	if snap.Days[4].MinC != 9.4 || snap.Days[4].MaxC != 17.0 {
		t.Errorf("day4 temps = %+v", snap.Days[4])
	}
}

func TestForecastErrorShapes(t *testing.T) {
	if _, err := parseForecast([]byte(`{"error":true,"reason":"latitude is required"}`), t0); err == nil ||
		!strings.Contains(err.Error(), "latitude is required") {
		t.Fatalf("api error shape should surface reason, got %v", err)
	}
	if _, err := parseForecast([]byte(`{}`), t0); err == nil {
		t.Fatal("forecast without current conditions should error")
	}
	if _, err := parseForecast([]byte(`{not json`), t0); err == nil {
		t.Fatal("malformed forecast body should error")
	}
}

func TestDescribeCodeAndCompass(t *testing.T) {
	if describeCode(61) != "rain" || describeCode(0) != "clear sky" || describeCode(999) != "unknown conditions" {
		t.Error("describeCode table off")
	}
	if compass(230) != "SW" || compass(0) != "N" || compass(359) != "N" {
		t.Errorf("compass(230)=%q compass(0)=%q compass(359)=%q", compass(230), compass(0), compass(359))
	}
}

// --- app-level tests through the real SDK presentation handlers ---

func TestManifestValidity(t *testing.T) {
	m := Manifest()
	if err := m.Validate(); err != nil {
		t.Fatalf("manifest validation: %v", err)
	}
	if m.ID != ID || m.Entrypoint != "weather" {
		t.Errorf("manifest = %s/%s", m.ID, m.Entrypoint)
	}
	want := map[string]bool{"ipc": true, "net.egress": true, "fs.read": true, "fs.write": true}
	if len(m.Permissions) != len(want) {
		t.Fatalf("permissions = %v", m.Permissions)
	}
	for _, c := range m.Permissions {
		if !want[c] {
			t.Errorf("unexpected permission %q", c)
		}
	}
	if !json.Valid(ManifestJSON()) {
		t.Error("ManifestJSON is not valid JSON")
	}
}

func TestSearchSaveSelectFlow(t *testing.T) {
	h := newHarness(t)
	v := h.view(t, 1)
	if findItem(v, itemEmpty) == nil {
		t.Fatal("empty view should carry the guidance item")
	}
	if len(h.fetchCalls) != 0 {
		t.Fatalf("the passive view must not fetch; calls = %v", h.fetchCalls)
	}

	v = h.act(t, 1, "search", "", map[string]string{"place": "paris"})
	if findItem(v, "cand_0") == nil || findItem(v, "cand_4") == nil {
		t.Fatalf("search results missing: %+v", v.Items)
	}
	if !strings.Contains(v.Status, "5 places match") {
		t.Errorf("search status = %q", v.Status)
	}
	if findItem(v, "cand_5") != nil {
		t.Error("more than maxSearchResults candidates rendered")
	}
	if a := findAction(v, "save"); a == nil || a.Disabled {
		t.Fatal("save should be enabled once candidates exist")
	}

	v = h.act(t, 1, "save", "cand_0", nil)
	cur := findItem(v, itemCurrent)
	if cur == nil || !strings.Contains(cur.Label, "14°C") || !strings.Contains(cur.Label, "overcast") {
		t.Fatalf("current item = %+v", cur)
	}
	if !strings.Contains(cur.Detail, "SW") || !strings.Contains(cur.Detail, "humidity 77%") {
		t.Errorf("current detail = %q", cur.Detail)
	}
	for i := 0; i < maxForecastDays; i++ {
		if findItem(v, fmt.Sprintf("day_%d", i)) == nil {
			t.Fatalf("day_%d item missing", i)
		}
	}
	if d := findItem(v, "day_3"); !strings.Contains(d.Detail, "precip —") {
		t.Errorf("null precip day should render —, got %q", d.Detail)
	}
	loc := findItem(v, "loc_1")
	if loc == nil || !strings.HasPrefix(loc.Label, "» ") || !strings.Contains(loc.Label, "Paris, Île-de-France, France") {
		t.Fatalf("saved location item = %+v", loc)
	}
	if !strings.Contains(v.Status, "saved Paris") || !strings.Contains(v.Status, "14°C") {
		t.Errorf("save status = %q", v.Status)
	}
	if len(h.fetchCalls) != 2 {
		t.Fatalf("fetchCalls = %v, want geocode+forecast", h.fetchCalls)
	}
	if !strings.Contains(h.fetchCalls[0], geocodeHost) || !strings.Contains(h.fetchCalls[1], forecastHost) {
		t.Errorf("fetch order = %v", h.fetchCalls)
	}
	if !strings.Contains(h.fetchCalls[1], "latitude=48.8534") {
		t.Errorf("forecast url = %q", h.fetchCalls[1])
	}

	st := h.readState(t)
	if len(st.Locations) != 1 || st.Selected != "loc_1" || st.Locations[0].Name != "Paris" {
		t.Fatalf("persisted state = %+v", st)
	}
	if _, ok := st.Cache["loc_1"]; !ok {
		t.Fatal("fetched snapshot was not persisted")
	}
}

func TestSelectSwitchBetweenSavedPlaces(t *testing.T) {
	h := newHarness(t)
	h.saveFirst(t, 1)
	h.act(t, 1, "search", "", map[string]string{"place": "paris again"})
	v := h.act(t, 1, "save", "cand_1", nil) // Paris, Texas — same name, other coords
	if findItem(v, "loc_2") == nil {
		t.Fatalf("second save produced no loc_2: %+v", v.Items)
	}
	if !strings.HasPrefix(findItem(v, "loc_2").Label, "» ") {
		t.Error("newly saved place should be selected")
	}
	calls := len(h.fetchCalls)

	// Selecting the other place serves its fresh cache without refetching.
	v = h.act(t, 1, "select", "loc_1", nil)
	if len(h.fetchCalls) != calls {
		t.Fatalf("fresh cache should not refetch; calls = %v", h.fetchCalls)
	}
	if !strings.HasPrefix(findItem(v, "loc_1").Label, "» ") {
		t.Error("loc_1 should be selected")
	}
	if strings.HasPrefix(findItem(v, "loc_2").Label, "» ") {
		t.Error("loc_2 should no longer be selected")
	}
}

func TestFreshCacheSkipsFetchAndRefreshForces(t *testing.T) {
	h := newHarness(t)
	h.saveFirst(t, 1)
	calls := len(h.fetchCalls)

	// Refresh always fetches, TTL or not.
	h.act(t, 1, "refresh", "", nil)
	if len(h.fetchCalls) != calls+1 {
		t.Fatalf("refresh should fetch; calls = %v", h.fetchCalls)
	}
	// Select within the TTL does not.
	h.act(t, 1, "select", "loc_1", nil)
	if len(h.fetchCalls) != calls+1 {
		t.Fatalf("fresh select should not fetch; calls = %v", h.fetchCalls)
	}
	// Past the TTL, select refetches.
	h.advance(cacheTTL + time.Minute)
	h.act(t, 1, "select", "loc_1", nil)
	if len(h.fetchCalls) != calls+2 {
		t.Fatalf("stale select should refetch; calls = %v", h.fetchCalls)
	}
}

func TestEgressDisabledSurfacesHonestly(t *testing.T) {
	h := newHarness(t)
	h.failHost(t, geocodeHost, "policy_denied: integration is disabled by operator policy: network egress is disabled")
	v := h.act(t, 1, "search", "", map[string]string{"place": "paris"})
	if !strings.Contains(v.Status, "network egress is off") ||
		!strings.Contains(v.Status, geocodeHost) || !strings.Contains(v.Status, forecastHost) {
		t.Fatalf("egress-disabled status should guide the operator, got %q", v.Status)
	}

	// A save whose forecast is denied still persists the place — honestly.
	h2 := newHarness(t)
	h2.failHost(t, forecastHost, "policy_denied: integration is disabled by operator policy: network egress is disabled")
	v = h2.saveFirst(t, 1)
	if findItem(v, "loc_1") == nil {
		t.Fatal("place should still be saved when the forecast fetch is denied")
	}
	if !strings.Contains(v.Status, "saved") || !strings.Contains(v.Status, "network egress is off") {
		t.Errorf("save+denied status = %q", v.Status)
	}
	st := h2.readState(t)
	if len(st.Locations) != 1 || len(st.Cache) != 0 {
		t.Fatalf("persisted state = %+v, want the place saved with no cache", st)
	}
	cur := findItem(v, itemCurrent)
	if !strings.Contains(cur.Label, "No data") {
		t.Errorf("no-cache current item = %+v", cur)
	}
}

func TestUnallowlistedHostGuidance(t *testing.T) {
	h := newHarness(t)
	h.failHost(t, forecastHost,
		`policy_denied: network egress denied by operator policy: host "api.open-meteo.com" is not in allowed hosts whitelist`)
	v := h.saveFirst(t, 1)
	if !strings.Contains(v.Status, "allowlist") ||
		!strings.Contains(v.Status, geocodeHost) || !strings.Contains(v.Status, forecastHost) {
		t.Fatalf("unallowlisted status should name both hosts, got %q", v.Status)
	}
}

func TestMissingGrantSurfacesHonestly(t *testing.T) {
	h := newHarness(t)
	h.failFetch(`permission denied: missing capability "net.egress"`)
	v := h.act(t, 1, "search", "", map[string]string{"place": "paris"})
	if !strings.Contains(v.Status, "net.egress") {
		t.Fatalf("missing-grant status = %q", v.Status)
	}
}

func TestTransportFailureKeepsStaleCache(t *testing.T) {
	h := newHarness(t)
	h.saveFirst(t, 1)
	h.advance(2 * time.Hour)
	h.failHost(t, forecastHost, "net: request failed: dial tcp: connection refused")

	v := h.act(t, 1, "refresh", "", nil)
	if !strings.Contains(v.Status, "couldn't refresh") || !strings.Contains(v.Status, "connection refused") {
		t.Fatalf("refresh failure status = %q", v.Status)
	}
	if !strings.Contains(v.Status, "2h ago") {
		t.Errorf("status should name the staleness: %q", v.Status)
	}
	cur := findItem(v, itemCurrent)
	if cur == nil || !strings.Contains(cur.Label, "14°C") {
		t.Fatal("stale snapshot should keep rendering after a failed refresh")
	}
}

func TestFetchFailureWithNoCache(t *testing.T) {
	h := newHarness(t)
	h.failHost(t, forecastHost, "net: request failed: no such host")
	v := h.saveFirst(t, 1)
	if !strings.Contains(v.Status, "saved") || !strings.Contains(v.Status, "no such host") {
		t.Errorf("no-cache failure status = %q", v.Status)
	}
	cur := findItem(v, itemCurrent)
	if !strings.Contains(cur.Detail, "Refresh") {
		t.Errorf("empty-cache item should point at Refresh: %q", cur.Detail)
	}
}

func TestHTTPErrorSurfacesServiceReason(t *testing.T) {
	h := newHarness(t)
	h.fetch = func(u string) fetchReply {
		return fetchReply{status: 503, body: []byte(`{"error":true,"reason":"Service temporarily unavailable"}`)}
	}
	v := h.act(t, 1, "search", "", map[string]string{"place": "paris"})
	if !strings.Contains(v.Status, "HTTP 503") || !strings.Contains(v.Status, "Service temporarily unavailable") {
		t.Fatalf("HTTP error status = %q", v.Status)
	}
}

func TestLocationNotFound(t *testing.T) {
	h := newHarness(t)
	h.fetch = func(u string) fetchReply {
		if strings.Contains(u, geocodeHost) {
			return fetchReply{status: 200, body: []byte(`{"generationtime_ms":0.1}`)}
		}
		return fetchReply{status: 200, body: fixture(t, "forecast_paris.json")}
	}
	v := h.act(t, 1, "search", "", map[string]string{"place": "xyzzy"})
	if !strings.Contains(v.Status, "no places match") {
		t.Fatalf("not-found status = %q", v.Status)
	}
	if a := findAction(v, "save"); a == nil || !a.Disabled {
		t.Error("save should be disabled with no candidates")
	}
}

func TestEmptySearchRejected(t *testing.T) {
	h := newHarness(t)
	v := h.act(t, 1, "search", "", map[string]string{"place": "   "})
	if !strings.Contains(v.Status, "place name") {
		t.Errorf("empty search status = %q", v.Status)
	}
	if len(h.fetchCalls) != 0 {
		t.Error("empty search must not fetch")
	}
}

func TestRemovePlace(t *testing.T) {
	h := newHarness(t)
	h.saveFirst(t, 1)
	h.act(t, 1, "search", "", map[string]string{"place": "paris"})
	h.act(t, 1, "save", "cand_1", nil) // loc_2 selected

	v := h.act(t, 1, "remove", "loc_2", nil)
	if findItem(v, "loc_2") != nil {
		t.Fatal("removed place still listed")
	}
	if !strings.HasPrefix(findItem(v, "loc_1").Label, "» ") {
		t.Error("removing the selection should fall back to the first place")
	}
	if !strings.Contains(v.Status, "removed") {
		t.Errorf("remove status = %q", v.Status)
	}
	st := h.readState(t)
	if len(st.Locations) != 1 || st.Selected != "loc_1" {
		t.Fatalf("persisted state after remove = %+v", st)
	}
	if _, ok := st.Cache["loc_2"]; ok {
		t.Error("removed place's cache should be dropped")
	}

	// Removing the last place leaves a clean empty state.
	v = h.act(t, 1, "remove", "loc_1", nil)
	if findItem(v, itemEmpty) == nil {
		t.Error("removing the last place should show the empty state")
	}
	if h.app.selected != "" {
		t.Errorf("selected = %q, want empty", h.app.selected)
	}
}

func TestRemoveRejectsNonLocations(t *testing.T) {
	h := newHarness(t)
	h.saveFirst(t, 1) // one saved place, so remove is enabled
	h.act(t, 1, "search", "", map[string]string{"place": "paris"})
	v := h.act(t, 1, "remove", "cand_0", nil)
	if !strings.Contains(v.Status, "aren't saved") {
		t.Errorf("remove on a candidate status = %q", v.Status)
	}
	if len(h.app.locations) != 1 {
		t.Fatal("candidate remove must not touch the saved list")
	}
	v = h.act(t, 1, "remove", "day_0", nil)
	if !strings.Contains(v.Status, "saved place") {
		t.Errorf("remove on a forecast row status = %q", v.Status)
	}
}

func TestDedupeOnSave(t *testing.T) {
	h := newHarness(t)
	h.saveFirst(t, 1)
	// Search again and save the same place: dedupe selects, never duplicates.
	h.act(t, 1, "search", "", map[string]string{"place": "paris"})
	v := h.act(t, 1, "save", "cand_0", nil)
	if len(h.app.locations) != 1 {
		t.Fatalf("dedupe should not duplicate; locations = %+v", h.app.locations)
	}
	if !strings.Contains(v.Status, "already saved") {
		t.Errorf("dedupe status = %q", v.Status)
	}
}

func TestSavedListBound(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < maxLocations; i++ {
		h.app.locations = append(h.app.locations, Location{
			ID: fmt.Sprintf("loc_%d", i+1), Name: fmt.Sprintf("Place %d", i+1),
			Latitude: float64(i), Longitude: float64(i), AddedAt: h.now,
		})
	}
	h.app.seq = maxLocations
	h.act(t, 1, "search", "", map[string]string{"place": "paris"})
	v := h.act(t, 1, "save", "cand_0", nil)
	if !strings.Contains(v.Status, "full") {
		t.Errorf("cap rejection status = %q", v.Status)
	}
	if len(h.app.locations) != maxLocations {
		t.Fatal("cap should not grow the list")
	}
}

func TestUnknownActionIsIPCError(t *testing.T) {
	h := newHarness(t)
	if _, err := h.tryAct(1, "launch_missiles", "", nil); err == nil {
		t.Error("unknown action should be an IPC error")
	}
}

func TestSelectRequiresSavedPlace(t *testing.T) {
	h := newHarness(t)
	h.saveFirst(t, 1)
	v := h.act(t, 1, "select", "day_0", nil)
	if !strings.Contains(v.Status, "saved place") {
		t.Errorf("select on a forecast row status = %q", v.Status)
	}
	// Selecting a candidate explains it isn't saved yet.
	h.act(t, 1, "search", "", map[string]string{"place": "paris"})
	v = h.act(t, 1, "select", "cand_0", nil)
	if !strings.Contains(v.Status, "search result") {
		t.Errorf("select on a candidate status = %q", v.Status)
	}
}

func TestPersistenceAcrossRelaunch(t *testing.T) {
	h := newHarness(t)
	h.saveFirst(t, 1)
	if err := h.app.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Two hours pass with the app off; the new instance shares the fs.
	h2 := newHarness(t)
	h2.files = h.files
	h2.now = t0.Add(2 * time.Hour)

	v := h2.view(t, 1)
	if loc := findItem(v, "loc_1"); loc == nil || !strings.HasPrefix(loc.Label, "» ") {
		t.Fatalf("relaunched view lost the selection: %+v", v.Items)
	}
	cur := findItem(v, itemCurrent)
	if cur == nil || !strings.Contains(cur.Label, "14°C") {
		t.Fatal("relaunched view should render the persisted snapshot")
	}
	if !strings.Contains(v.Status, "stale") {
		t.Errorf("a two-hour-old cache should read stale: %q", v.Status)
	}
	if len(h2.fetchCalls) != 0 {
		t.Fatalf("the passive view must not fetch even for stale data; calls = %v", h2.fetchCalls)
	}
}

func TestCorruptStateStartsFresh(t *testing.T) {
	h := newHarness(t)
	h.files[statePath] = []byte("{not json")
	v := h.view(t, 1)
	if !strings.Contains(v.Error, "unreadable") {
		t.Errorf("corrupt state error = %q", v.Error)
	}
	if findItem(v, itemEmpty) == nil {
		t.Error("corrupt state should start empty")
	}
	// A real change saves over the corrupt file.
	h.saveFirst(t, 1)
	st := h.readState(t)
	if len(st.Locations) != 1 {
		t.Fatal("save after corruption should persist cleanly")
	}
}

func TestSanitizeHealsLoadedState(t *testing.T) {
	h := newHarness(t)
	bad := diskState{
		Version: stateVersion,
		Locations: []Location{
			{ID: "loc_1", Name: "  Paris\n\n", Latitude: 48.85, Longitude: 2.35, AddedAt: t0.Add(-time.Hour)},
			{ID: "loc_1", Name: "Dupe", Latitude: 1, Longitude: 1, AddedAt: t0},
			{ID: "day_0", Name: "Reserved", Latitude: 2, Longitude: 2, AddedAt: t0},
			{ID: "loc_9", Name: "NoCoords", Latitude: 999, Longitude: 0, AddedAt: t0},
			{ID: "", Name: "NoID", Latitude: 3, Longitude: 3, AddedAt: t0},
		},
		Selected: "loc_999",
		Seq:      9,
		Cache: map[string]Snapshot{
			"ghost":  {FetchedAt: t0, TempC: 1},
			"loc_1":  {FetchedAt: t0.Add(time.Hour), TempC: 20, Days: make([]Day, 9)},
			"loc_99": {FetchedAt: t0, TempC: 5},
		},
	}
	raw, _ := json.Marshal(bad)
	h.files[statePath] = raw

	v := h.view(t, 1)
	if v.Error != "" {
		t.Fatalf("healable state should not banner: %q", v.Error)
	}
	if len(h.app.locations) != 4 { // NoCoords dropped
		t.Fatalf("locations after heal = %+v", h.app.locations)
	}
	ids := map[string]bool{}
	for _, l := range h.app.locations {
		if ids[l.ID] || l.ID == "" || strings.HasPrefix(l.ID, "day_") {
			t.Fatalf("bad id survived: %+v", h.app.locations)
		}
		ids[l.ID] = true
	}
	if h.app.locations[0].Name != "Paris" {
		t.Errorf("normalized name = %q", h.app.locations[0].Name)
	}
	if h.app.selected == "" || !ids[h.app.selected] {
		t.Errorf("selection should be pinned to a real place, got %q", h.app.selected)
	}
	for id := range h.app.cache {
		if !ids[id] {
			t.Errorf("cache entry %q survived its place", id)
		}
	}
	if snap := h.app.cache["loc_1"]; snap.FetchedAt.After(h.now) {
		t.Error("future fetch timestamp should be clamped")
	} else if len(snap.Days) != maxForecastDays {
		t.Errorf("persisted days should be bounded to %d, got %d", maxForecastDays, len(snap.Days))
	}
	// The healed state is written back so a crash cannot reload the damage.
	st := h.readState(t)
	if len(st.Locations) != 4 || st.Selected == "loc_999" {
		t.Fatalf("healed state was not persisted: %+v", st)
	}
}

func TestFSDeniedStillWorksInMemory(t *testing.T) {
	h := newHarness(t)
	h.denyFS = true
	v := h.view(t, 1)
	if !strings.Contains(v.Error, "denied") {
		t.Errorf("denied storage should banner: %q", v.Error)
	}
	v = h.act(t, 1, "search", "", map[string]string{"place": "paris"})
	v = h.act(t, 1, "save", "cand_0", nil)
	if findItem(v, "loc_1") == nil {
		t.Fatal("in-memory save should still work without fs")
	}
	if !strings.Contains(v.Status, "save failed") {
		t.Errorf("denied save should say so: %q", v.Status)
	}
}

func TestV1ViewHasNoBlocksMetersOrGrid(t *testing.T) {
	h := newHarness(t)
	v := h.saveFirst(t, 1)
	if len(v.Blocks) != 0 || len(v.Meters) != 0 || v.Grid != nil {
		t.Fatal("v1 view must not carry blocks, meters, or grid")
	}
	if findItem(v, itemCurrent) == nil {
		t.Error("v1 view should still carry conditions as items")
	}
}

func TestV2ViewHasArtAndMeters(t *testing.T) {
	h := newHarness(t)
	v := h.saveFirst(t, 2)
	if len(v.Blocks) != 1 || v.Blocks[0].ID != "weather_art" {
		t.Fatalf("blocks = %+v, want one weather_art block", v.Blocks)
	}
	if m := findMeter(v, "m_humidity"); m == nil || m.Value != 0.77 {
		t.Errorf("humidity meter = %+v, want 0.77", m)
	}
	if m := findMeter(v, "m_precip"); m == nil || m.Value != 0.10 {
		t.Errorf("precip meter = %+v, want 0.10", m)
	}
	// Same session still serves v1.
	v1 := h.view(t, 1)
	if len(v1.Blocks) != 0 {
		t.Error("v1 after v2 must not carry blocks")
	}
}

func TestNightGlyphForClearSky(t *testing.T) {
	if glyphFor(0, true) != artClearDay {
		t.Error("clear day should pick the sun glyph")
	}
	if glyphFor(0, false) != artClearNight {
		t.Error("clear night should pick the night glyph")
	}
	if glyphFor(61, false) != artRain {
		t.Error("rain glyph ignores is_day")
	}
}

func TestRoutesRoundTrip(t *testing.T) {
	h := newHarness(t)
	var searchOut struct {
		Count int `json:"count"`
	}
	h.route(t, "search", map[string]string{"query": "paris"}, &searchOut)
	if searchOut.Count != maxSearchResults {
		t.Fatalf("search count = %d", searchOut.Count)
	}
	var saveOut struct {
		Selected string `json:"selected"`
	}
	h.route(t, "save", map[string]string{"id": "cand_1"}, &saveOut)
	if saveOut.Selected != "loc_1" {
		t.Fatalf("save selected = %q", saveOut.Selected)
	}
	var st struct {
		Locations []Location `json:"locations"`
		Selected  string     `json:"selected"`
		Hosts     []string   `json:"egress_hosts"`
	}
	h.route(t, "state", nil, &st)
	if len(st.Locations) != 1 || st.Selected != "loc_1" {
		t.Fatalf("state route = %+v", st)
	}
	if len(st.Hosts) != 2 || st.Hosts[0] != geocodeHost || st.Hosts[1] != forecastHost {
		t.Errorf("state egress_hosts = %v", st.Hosts)
	}
	var refreshOut struct {
		Selected string `json:"selected"`
	}
	h.route(t, "refresh", nil, &refreshOut)
	if refreshOut.Selected != "loc_1" {
		t.Fatalf("refresh = %+v", refreshOut)
	}
	// select via route with a saved id works; a bogus id is an honest error.
	h.route(t, "select", map[string]string{"id": "loc_1"}, nil)
	if msg := h.failRoute(t, "select", map[string]string{"id": "loc_999"}); !strings.Contains(msg, "saved place") {
		t.Errorf("select bogus = %q", msg)
	}
	if msg := h.failRoute(t, "save", map[string]string{"id": "cand_99"}); !strings.Contains(msg, "search result") {
		t.Errorf("save bogus = %q", msg)
	}
	if msg := h.failRoute(t, "remove", map[string]string{"id": "loc_999"}); !strings.Contains(msg, "saved place") {
		t.Errorf("remove bogus = %q", msg)
	}
	var remOut struct {
		Removed  bool   `json:"removed"`
		Selected string `json:"selected"`
	}
	h.route(t, "remove", map[string]string{"id": "loc_1"}, &remOut)
	if !remOut.Removed || remOut.Selected != "" {
		t.Errorf("remove = %+v", remOut)
	}
	if msg := h.failRoute(t, "refresh", nil); !strings.Contains(msg, "save a place") {
		t.Errorf("refresh with no places = %q", msg)
	}
}

// A transient fs/read failure (a canceled call racing the first view, a
// host I/O error, the read-size bound) is not "no save yet": the app must
// say so honestly, retry the load, and never let the next change overwrite
// the saved places.
func TestTransientReadErrorNeverOverwritesPlaces(t *testing.T) {
	h := newHarness(t)
	h.saveFirst(t, 1) // searches Paris and saves it as loc_1

	h2 := newHarness(t)
	h2.files = h.files
	h2.readErr = context.Canceled // first fs/read fails once, transiently

	v := h2.view(t, 1)
	if v.State != sdk.ViewError || v.Error == "" {
		t.Fatalf("transient read failure view = %+v, want an honest error", v)
	}

	// The retry loads the real file; saving another place then extends
	// the list, not replaces it.
	h2.act(t, 1, "search", "", map[string]string{"place": "paris again"})
	v = h2.act(t, 1, "save", "cand_1", nil) // Paris, Texas — other coords
	if findItem(v, "loc_2") == nil {
		t.Fatalf("second save after transient failure produced no loc_2: %+v", v.Items)
	}
	st := h2.readState(t)
	if len(st.Locations) != 2 {
		t.Fatalf("%d place(s) on disk after a transient read error and one save, want 2", len(st.Locations))
	}
}
