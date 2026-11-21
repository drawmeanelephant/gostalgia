package rss

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gostalgia/sdk"
)

var t0 = time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)

// --- feed fixtures: no test touches the network; the harness serves
// these bytes through its net/fetch seam ---

const rssFixture = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>Example News</title>
    <link>https://news.example.com/</link>
    <item>
      <title>First Post</title>
      <link>https://news.example.com/first</link>
      <guid>https://news.example.com/first</guid>
      <pubDate>Thu, 08 Oct 2026 07:00:00 +0000</pubDate>
    </item>
    <item>
      <title>Second Post</title>
      <link>https://news.example.com/second</link>
      <guid>https://news.example.com/second</guid>
      <pubDate>Wed, 07 Oct 2026 07:00:00 +0000</pubDate>
    </item>
    <item>
      <title>Undated Post</title>
      <link>https://news.example.com/undated</link>
      <guid>https://news.example.com/undated</guid>
    </item>
  </channel>
</rss>`

const atomFixture = `<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <title>Example Atom</title>
  <link href="https://atom.example.com/"/>
  <updated>2026-10-08T07:30:00Z</updated>
  <entry>
    <title>Atom Entry One</title>
    <link rel="alternate" href="https://atom.example.com/one"/>
    <id>urn:uuid:entry-1</id>
    <published>2026-10-08T06:00:00Z</published>
    <updated>2026-10-08T07:00:00Z</updated>
  </entry>
  <entry>
    <title>Second Entry</title>
    <link rel="self" href="https://atom.example.com/feed.atom"/>
    <link rel="alternate" href="https://atom.example.com/two"/>
    <id>urn:uuid:entry-2</id>
    <updated>2026-10-07T06:00:00Z</updated>
  </entry>
</feed>`

// makeRSS builds an RSS 2.0 document with n items so bound tests do not
// need hand-written fixtures.
func makeRSS(title string, n int, since time.Time) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><rss version="2.0"><channel>`)
	fmt.Fprintf(&b, `<title>%s</title>`, title)
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, `<item><title>Story %d</title><link>https://x.example.com/s%d</link><guid>g%d</guid><pubDate>%s</pubDate></item>`,
			i, i, i, since.Add(-time.Duration(i)*time.Hour).Format(time.RFC1123Z))
	}
	b.WriteString(`</channel></rss>`)
	return b.String()
}

// notification is one captured notify/post call.
type notification struct {
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Body     string `json:"body"`
}

// harness models the app-private VFS partition, the notify route, and
// the net/fetch egress route, with an injectable clock. Feed bytes live
// in feeds[url]; fetchErr[url] forces an egress-level error;
// fetchStatus[url] forces an HTTP status. No test sleeps on fake time:
// only the Run ticker test waits on real milliseconds.
type harness struct {
	now        time.Time
	files      map[string][]byte
	notifies   []notification
	denyNotify bool
	denyFS     bool
	readErr    error // if set, the next fs/read fails once with this error

	fmu         sync.Mutex // guards the four fetch fields for Run tests
	feeds       map[string][]byte
	fetchErr    map[string]error
	fetchStatus map[string]int
	fetches     []string

	seq      atomic.Int64
	app      *RSS
	handlers map[string]sdk.Handler
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		now:         t0,
		files:       map[string][]byte{},
		feeds:       map[string][]byte{},
		fetchErr:    map[string]error{},
		fetchStatus: map[string]int{},
		handlers:    map[string]sdk.Handler{},
	}
	inst, err := Factory()
	if err != nil {
		t.Fatal(err)
	}
	r := inst.(*RSS)
	setClock(r, func() time.Time { return h.now })
	ctx := sdk.NewContext(Manifest(), slog.New(slog.NewTextHandler(io.Discard, nil)), h.call,
		func(name string, hd sdk.Handler) error { h.handlers[name] = hd; return nil })
	if err := r.Init(ctx); err != nil {
		t.Fatal(err)
	}
	h.app = r
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
			return &sdk.Error{Code: sdk.CodePermissionDenied, Message: `permission denied: missing capability "fs.read"`}
		}
		data, ok := h.files[p.Path]
		if !ok {
			return &sdk.Error{Code: sdk.CodeNotFound, Message: fmt.Sprintf("not_found: %s", p.Path)}
		}
		return reply(map[string]any{"path": p.Path, "data_base64": base64.StdEncoding.EncodeToString(data)})
	case "fs/save":
		if h.denyFS {
			return errors.New(`permission denied: missing capability "fs.write"`)
		}
		data, err := base64.StdEncoding.DecodeString(p.Data)
		if err != nil {
			return err
		}
		h.files[p.Path] = data
		return reply(map[string]any{"path": p.Path, "saved": true})
	case "net/fetch":
		var np struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(raw, &np)
		h.fmu.Lock()
		defer h.fmu.Unlock()
		h.fetches = append(h.fetches, np.URL)
		if err, ok := h.fetchErr[np.URL]; ok {
			return err
		}
		body, ok := h.feeds[np.URL]
		if !ok {
			return errors.New("net: request failed: dial tcp: lookup no-such-host: no such host")
		}
		status := h.fetchStatus[np.URL]
		if status == 0 {
			status = 200
		}
		return reply(map[string]any{
			"status":      status,
			"status_text": fmt.Sprintf("%d status", status),
			"data_base64": base64.StdEncoding.EncodeToString(body),
			"size":        len(body),
		})
	case "notify/post":
		if h.denyNotify {
			return errors.New(`permission denied: missing capability "notify"`)
		}
		var n notification
		if err := json.Unmarshal(raw, &n); err != nil {
			return err
		}
		h.notifies = append(h.notifies, n)
		return reply(map[string]any{"posted": true})
	}
	return fmt.Errorf("unknown method %s", method)
}

func (h *harness) advance(d time.Duration) { h.now = h.now.Add(d) }

// fetchCount reports how many net/fetch calls hit the harness.
func (h *harness) fetchCount() int {
	h.fmu.Lock()
	defer h.fmu.Unlock()
	return len(h.fetches)
}

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

func findAction(v sdk.View, id string) *sdk.Action {
	for i := range v.Actions {
		if v.Actions[i].ID == id {
			return &v.Actions[i]
		}
	}
	return nil
}

func findItem(v sdk.View, id string) *sdk.Item {
	for i := range v.Items {
		if v.Items[i].ID == id {
			return &v.Items[i]
		}
	}
	return nil
}

func feedItem(v sdk.View, id string) *sdk.Item { return findItem(v, id) }

// --- parser tests ---

func TestParseRSSFixture(t *testing.T) {
	f, err := parseFeed([]byte(rssFixture))
	if err != nil {
		t.Fatalf("parseFeed RSS: %v", err)
	}
	if f.Title != "Example News" {
		t.Errorf("feed title = %q", f.Title)
	}
	if len(f.Items) != 3 {
		t.Fatalf("items = %d, want 3", len(f.Items))
	}
	first := f.Items[0]
	if first.Title != "First Post" || first.Link != "https://news.example.com/first" || first.GUID == "" {
		t.Errorf("first item = %+v", first)
	}
	wantPub := time.Date(2026, 10, 8, 7, 0, 0, 0, time.UTC)
	if !first.Published.Equal(wantPub) {
		t.Errorf("pubDate parsed = %v, want %v", first.Published, wantPub)
	}
	if !f.Items[2].Published.IsZero() {
		t.Errorf("undated item published = %v, want zero", f.Items[2].Published)
	}
}

func TestParseAtomFixture(t *testing.T) {
	f, err := parseFeed([]byte(atomFixture))
	if err != nil {
		t.Fatalf("parseFeed Atom: %v", err)
	}
	if f.Title != "Example Atom" {
		t.Errorf("feed title = %q", f.Title)
	}
	if len(f.Items) != 2 {
		t.Fatalf("entries = %d, want 2", len(f.Items))
	}
	one := f.Items[0]
	if one.Title != "Atom Entry One" || one.Link != "https://atom.example.com/one" || one.GUID != "urn:uuid:entry-1" {
		t.Errorf("entry one = %+v", one)
	}
	wantPub := time.Date(2026, 10, 8, 6, 0, 0, 0, time.UTC)
	if !one.Published.Equal(wantPub) {
		t.Errorf("published parsed = %v, want %v", one.Published, wantPub)
	}
	// The self link must not win over the alternate link.
	if got := f.Items[1].Link; got != "https://atom.example.com/two" {
		t.Errorf("entry two link = %q, want the alternate link", got)
	}
	wantUpd := time.Date(2026, 10, 7, 6, 0, 0, 0, time.UTC)
	if !f.Items[1].Published.Equal(wantUpd) {
		t.Errorf("entry two falls back to updated: %v, want %v", f.Items[1].Published, wantUpd)
	}
}

func TestParseRejectsNonFeedDocuments(t *testing.T) {
	for name, doc := range map[string]string{
		"garbage":   "this is not xml at all",
		"truncated": `<?xml version="1.0"?><rss version="2.0"><channel><title>x`,
		"html":      `<html><body>not a feed</body></html>`,
		"rdf":       `<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#"><channel/></rdf:RDF>`,
		"empty_rss": `<rss version="2.0"></rss>`,
	} {
		if _, err := parseFeed([]byte(doc)); err == nil {
			t.Errorf("%s: expected parse rejection, got nil", name)
		} else if !strings.Contains(err.Error(), "unreadable feed") {
			t.Errorf("%s: error %q does not say 'unreadable feed'", name, err)
		}
	}
}

func TestParseDateLayouts(t *testing.T) {
	cases := map[string]time.Time{
		"Thu, 08 Oct 2026 07:00:00 +0000": time.Date(2026, 10, 8, 7, 0, 0, 0, time.UTC),
		"08 Oct 2026 07:00:00 GMT":        time.Date(2026, 10, 8, 7, 0, 0, 0, time.UTC),
		"2026-10-08T07:00:00Z":            time.Date(2026, 10, 8, 7, 0, 0, 0, time.UTC),
		"2026-10-08":                      time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC),
	}
	for raw, want := range cases {
		if got := parseDate(raw); !got.Equal(want) {
			t.Errorf("parseDate(%q) = %v, want %v", raw, got, want)
		}
	}
	if got := parseDate("not a date"); !got.IsZero() {
		t.Errorf("unparseable date = %v, want zero", got)
	}
}

// --- app-level tests through the real SDK presentation handlers ---

func TestManifestValidity(t *testing.T) {
	m := Manifest()
	if err := m.Validate(); err != nil {
		t.Fatalf("manifest validation: %v", err)
	}
	if m.ID != ID || m.Entrypoint != "rss" {
		t.Errorf("manifest = %s/%s", m.ID, m.Entrypoint)
	}
	want := map[string]bool{"ipc": true, "net.egress": true, "fs.read": true, "fs.write": true, "notify": true}
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

func TestEmptyViewHasHonestNotice(t *testing.T) {
	h := newHarness(t)
	v := h.view(t, 2)
	it := findItem(v, itemEmpty)
	if it == nil || !strings.Contains(it.Label, "No feeds yet") {
		t.Fatalf("empty view = %+v", v.Items)
	}
	if !strings.Contains(v.Status, "no feeds yet") {
		t.Errorf("status = %q", v.Status)
	}
	for _, a := range v.Actions {
		if a.ID != "subscribe" && !a.Disabled {
			t.Errorf("action %s should be disabled with no feeds", a.ID)
		}
	}
}

func TestSubscribeFetchesHeadlines(t *testing.T) {
	h := newHarness(t)
	url := "https://news.example.com/feed.xml"
	h.feeds[url] = []byte(rssFixture)

	v := h.act(t, 2, "subscribe", "", map[string]string{"feed_url": url})
	feed := feedItem(v, "feed_1")
	if feed == nil {
		t.Fatalf("feed row missing: %+v", v.Items)
	}
	if feed.Label != "Example News" {
		t.Errorf("feed label = %q, want the feed's own title", feed.Label)
	}
	if !strings.Contains(feed.Detail, "3 unread of 3") {
		t.Errorf("feed detail = %q", feed.Detail)
	}
	if findItem(v, "item_1") == nil || findItem(v, "item_3") == nil {
		t.Fatalf("headline rows missing: %+v", v.Items)
	}
	if !strings.Contains(v.Status, "subscribed to Example News — 3 headlines") {
		t.Errorf("status = %q", v.Status)
	}
	if h.fetchCount() != 1 {
		t.Errorf("fetches = %d, want 1", h.fetchCount())
	}
	// Newest-first ordering: the undated item sinks to the tail.
	var labels []string
	for _, it := range v.Items {
		if strings.HasPrefix(it.ID, "item_") {
			labels = append(labels, it.Label)
		}
	}
	if !strings.Contains(labels[0], "First Post") || !strings.Contains(labels[2], "Undated Post") {
		t.Errorf("headline order = %v, want newest first, undated last", labels)
	}
}

func TestAtomSubscribeRendersHeadlines(t *testing.T) {
	h := newHarness(t)
	url := "https://atom.example.com/feed.atom"
	h.feeds[url] = []byte(atomFixture)
	v := h.act(t, 2, "subscribe", "", map[string]string{"feed_url": url})
	if !strings.Contains(v.Status, "subscribed to Example Atom — 2 headlines") {
		t.Errorf("status = %q", v.Status)
	}
	var atomItem *sdk.Item
	for i := range v.Items {
		if strings.HasPrefix(v.Items[i].ID, "item_") && strings.Contains(v.Items[i].Label, "Atom Entry One") {
			atomItem = &v.Items[i]
		}
	}
	if atomItem == nil || !strings.Contains(atomItem.Detail, "atom.example.com/one") {
		t.Fatalf("atom headline row missing link detail: %+v", v.Items)
	}
}

func TestSubscribeRejections(t *testing.T) {
	h := newHarness(t)
	url := "https://news.example.com/feed.xml"
	h.feeds[url] = []byte(rssFixture)
	h.act(t, 2, "subscribe", "", map[string]string{"feed_url": url})

	for _, tc := range []struct {
		name, value, want string
	}{
		{"empty", "   ", "paste a feed URL"},
		{"junk", "not a url", "not a valid URL"},
		{"nohost", "https://", "not a valid URL"},
		{"scheme", "ftp://news.example.com/feed", "only http(s)"},
		{"duplicate", url, "already subscribed"},
	} {
		v := h.act(t, 2, "subscribe", "", map[string]string{"feed_url": tc.value})
		if !strings.Contains(v.Status, tc.want) {
			t.Errorf("%s: status = %q, want %q", tc.name, v.Status, tc.want)
		}
	}
	if len(h.app.feeds) != 1 {
		t.Errorf("feeds after rejections = %d, want 1", len(h.app.feeds))
	}
}

func TestEgressDeniedDegradesToStatus(t *testing.T) {
	h := newHarness(t)
	url := "https://news.example.com/feed.xml"
	h.fetchErr[url] = errors.New(`permission denied: missing capability "net.egress"`)

	v := h.act(t, 2, "subscribe", "", map[string]string{"feed_url": url})
	feed := feedItem(v, "feed_1")
	if feed == nil {
		t.Fatalf("denied feed should still be listed: %+v", v.Items)
	}
	if !strings.Contains(feed.Detail, "egress permission denied") {
		t.Errorf("feed detail = %q, want the honest denial", feed.Detail)
	}
	if v.State != sdk.ViewReady {
		t.Errorf("view state = %q after denial", v.State)
	}
	if !strings.Contains(v.Status, "first fetch failed") {
		t.Errorf("status = %q", v.Status)
	}
	if len(h.app.feeds) != 1 {
		t.Error("denied subscribe corrupted the feed list")
	}
}

func TestEgressDisabledAndUnallowlistedDegrade(t *testing.T) {
	h := newHarness(t)
	disabled := "https://a.example.com/feed"
	unlisted := "https://b.example.com/feed"
	h.fetchErr[disabled] = errors.New("policy_denied: network egress is disabled")
	h.fetchErr[unlisted] = errors.New(`policy_denied: host "b.example.com" is not in allowed hosts whitelist`)

	v := h.act(t, 2, "subscribe", "", map[string]string{"feed_url": disabled})
	if d := feedItem(v, "feed_1").Detail; !strings.Contains(d, "network egress is disabled") {
		t.Errorf("egress-disabled detail = %q", d)
	}
	v = h.act(t, 2, "subscribe", "", map[string]string{"feed_url": unlisted})
	if d := feedItem(v, "feed_2").Detail; !strings.Contains(d, "not on the operator allowlist") {
		t.Errorf("unallowlisted detail = %q", d)
	}
	if !strings.Contains(v.Status, "first fetch failed") {
		t.Errorf("status = %q", v.Status)
	}
}

func TestNetworkErrorAndRecover(t *testing.T) {
	h := newHarness(t)
	url := "https://news.example.com/feed.xml"
	h.fetchErr[url] = errors.New("net: request failed: dial tcp: i/o timeout")
	v := h.act(t, 2, "subscribe", "", map[string]string{"feed_url": url})
	if d := feedItem(v, "feed_1").Detail; !strings.Contains(d, "fetch failed") {
		t.Fatalf("network-error detail = %q", d)
	}

	// The network comes back: the same feed fetches honestly.
	h.fetchErr = map[string]error{}
	h.feeds[url] = []byte(rssFixture)
	v = h.act(t, 2, "refresh", "feed_1", nil)
	feed := feedItem(v, "feed_1")
	if !strings.Contains(feed.Detail, "3 unread of 3") {
		t.Errorf("recovered feed detail = %q", feed.Detail)
	}
	if findItem(v, "item_1") == nil {
		t.Error("headlines did not arrive after recovery")
	}
}

func TestHTTPErrorAndMalformedFeedDegrade(t *testing.T) {
	h := newHarness(t)
	bad := "https://http404.example.com/feed"
	mal := "https://malformed.example.com/feed"
	h.feeds[bad] = []byte("not found")
	h.fetchStatus[bad] = 404
	h.feeds[mal] = []byte("<html>definitely not a feed</html>")

	v := h.act(t, 2, "subscribe", "", map[string]string{"feed_url": bad})
	if d := feedItem(v, "feed_1").Detail; !strings.Contains(d, "HTTP 404") {
		t.Errorf("404 detail = %q", d)
	}
	v = h.act(t, 2, "subscribe", "", map[string]string{"feed_url": mal})
	if d := feedItem(v, "feed_2").Detail; !strings.Contains(d, "unreadable feed") {
		t.Errorf("malformed detail = %q", d)
	}
	if len(h.app.feeds) != 2 || len(h.app.feeds[0].Items) != 0 || len(h.app.feeds[1].Items) != 0 {
		t.Error("failed fetches corrupted stored state")
	}
}

func TestMalformedThenGoodFeedKeepsItems(t *testing.T) {
	h := newHarness(t)
	url := "https://news.example.com/feed.xml"
	h.feeds[url] = []byte(rssFixture)
	h.act(t, 2, "subscribe", "", map[string]string{"feed_url": url})
	if len(h.app.feeds[0].Items) != 3 {
		t.Fatal("setup: expected 3 headlines")
	}

	// The feed goes malformed, then recovers; stored headlines survive.
	h.feeds[url] = []byte("{not xml")
	v := h.act(t, 2, "refresh", "feed_1", nil)
	feed := feedItem(v, "feed_1")
	if !strings.Contains(feed.Detail, "unreadable feed") || !strings.Contains(feed.Detail, "3 headlines") {
		t.Errorf("malformed-refresh detail = %q", feed.Detail)
	}
	if len(h.app.feeds[0].Items) != 3 {
		t.Error("malformed refresh emptied stored headlines")
	}

	h.feeds[url] = []byte(rssFixture)
	v = h.act(t, 2, "refresh", "feed_1", nil)
	if d := feedItem(v, "feed_1").Detail; !strings.Contains(d, "3 unread of 3") {
		t.Errorf("post-recovery detail = %q", d)
	}
}

func TestMarkReadUnread(t *testing.T) {
	h := newHarness(t)
	url := "https://news.example.com/feed.xml"
	h.feeds[url] = []byte(rssFixture)
	h.act(t, 2, "subscribe", "", map[string]string{"feed_url": url})

	v := h.act(t, 2, "mark_read", "item_1", nil)
	if !strings.Contains(findItem(v, "item_1").Label, "[x]") {
		t.Errorf("item_1 not marked read: %+v", findItem(v, "item_1"))
	}
	if !strings.Contains(feedItem(v, "feed_1").Detail, "2 unread of 3") {
		t.Errorf("unread count = %q", feedItem(v, "feed_1").Detail)
	}

	// Already-read and notice-row selections are honest rejections.
	v = h.act(t, 2, "mark_read", "item_1", nil)
	if !strings.Contains(v.Status, "already read") {
		t.Errorf("re-mark status = %q", v.Status)
	}
	v = h.act(t, 2, "mark_unread", "item_1", nil)
	if !strings.Contains(findItem(v, "item_1").Label, "[ ]") {
		t.Error("item_1 not marked unread")
	}

	// A feed row marks all of its headlines read at once (item_1 went
	// back to unread, so all three flip).
	v = h.act(t, 2, "mark_read", "feed_1", nil)
	if !strings.Contains(feedItem(v, "feed_1").Detail, "0 unread of 3") {
		t.Errorf("feed-marked detail = %q", feedItem(v, "feed_1").Detail)
	}
	if !strings.Contains(v.Status, "marked 3 headlines read") {
		t.Errorf("feed-marked status = %q", v.Status)
	}
	// Feed rows cannot go unread.
	v = h.act(t, 2, "mark_unread", "feed_1", nil)
	if !strings.Contains(v.Status, "only a headline") {
		t.Errorf("feed-unread status = %q", v.Status)
	}
}

func TestUnsubscribeRemovesFeedAndItems(t *testing.T) {
	h := newHarness(t)
	url := "https://news.example.com/feed.xml"
	h.feeds[url] = []byte(rssFixture)
	h.act(t, 2, "subscribe", "", map[string]string{"feed_url": url})

	// Honest rejections first, while the action is still enabled.
	v := h.act(t, 2, "unsubscribe", "", nil)
	if !strings.Contains(v.Status, "select a feed") {
		t.Errorf("no-selection status = %q", v.Status)
	}
	v = h.act(t, 2, "unsubscribe", "item_1", nil)
	if !strings.Contains(v.Status, "applies to feeds") {
		t.Errorf("headline-selection status = %q", v.Status)
	}

	v = h.act(t, 2, "unsubscribe", "feed_1", nil)
	if feedItem(v, "feed_1") != nil {
		t.Error("feed row survived unsubscribe")
	}
	for _, it := range v.Items {
		if strings.HasPrefix(it.ID, "item_") {
			t.Errorf("headline row %s survived its feed's unsubscribe", it.ID)
		}
	}
	if !strings.Contains(v.Status, "unsubscribed from Example News — 3 stored headlines removed") {
		t.Errorf("status = %q", v.Status)
	}
	// The persisted file agrees.
	var st diskState
	if err := json.Unmarshal(h.files[statePath], &st); err != nil || len(st.Feeds) != 0 {
		t.Errorf("persisted feeds = %+v, want empty", st.Feeds)
	}
}

func TestDedupeOnRefresh(t *testing.T) {
	h := newHarness(t)
	url := "https://news.example.com/feed.xml"
	h.feeds[url] = []byte(rssFixture)
	h.act(t, 2, "subscribe", "", map[string]string{"feed_url": url})
	v := h.act(t, 2, "refresh", "feed_1", nil)
	if got := len(h.app.feeds[0].Items); got != 3 {
		t.Fatalf("items after same-doc refresh = %d, want 3", got)
	}
	if !strings.Contains(v.Status, "up to date") {
		t.Errorf("status = %q", v.Status)
	}
	if len(h.notifies) != 0 {
		t.Error("no new items should mean no notification")
	}
}

func TestNewItemsNotifyOnce(t *testing.T) {
	h := newHarness(t)
	url := "https://news.example.com/feed.xml"
	h.feeds[url] = []byte(rssFixture)
	h.act(t, 2, "subscribe", "", map[string]string{"feed_url": url})
	if len(h.notifies) != 0 {
		t.Fatal("first fetch is baseline sync — it must not notify")
	}

	// The feed publishes a fourth item; the next fetch finds it.
	h.feeds[url] = []byte(strings.ReplaceAll(rssFixture,
		"</channel>",
		`<item><title>Fresh Post</title><link>https://news.example.com/fresh</link><guid>fresh-1</guid><pubDate>Thu, 08 Oct 2026 08:30:00 +0000</pubDate></item></channel>`))

	v := h.act(t, 2, "refresh", "feed_1", nil)
	if got := len(h.app.feeds[0].Items); got != 4 {
		t.Fatalf("items after refresh = %d, want 4", got)
	}
	if len(h.notifies) != 1 || !strings.Contains(h.notifies[0].Body, "1 new headline") {
		t.Fatalf("notifies = %+v, want one '1 new headline'", h.notifies)
	}
	if !strings.Contains(v.Status, "1 new headline — Example News") {
		t.Errorf("status = %q", v.Status)
	}
	// And the fresh headline sorts first.
	var firstHeadline *sdk.Item
	for i := range v.Items {
		if strings.HasPrefix(v.Items[i].ID, "item_") {
			firstHeadline = &v.Items[i]
			break
		}
	}
	if firstHeadline == nil || !strings.Contains(firstHeadline.Label, "Fresh Post") {
		t.Errorf("newest headline did not sort first: %+v", firstHeadline)
	}
}

func TestNotifyDegradesQuietly(t *testing.T) {
	h := newHarness(t)
	url := "https://news.example.com/feed.xml"
	h.feeds[url] = []byte(rssFixture)
	h.act(t, 2, "subscribe", "", map[string]string{"feed_url": url})

	h.denyNotify = true
	h.feeds[url] = []byte(strings.ReplaceAll(rssFixture,
		"</channel>",
		`<item><title>Fresh Post</title><link>https://news.example.com/fresh</link><guid>fresh-1</guid></item></channel>`))
	h.app.poll(context.Background())
	if !h.app.notifyDown {
		t.Error("denied notify/post should set notifyDown")
	}
	v := h.view(t, 2)
	if v.State != sdk.ViewReady {
		t.Errorf("view state = %q after notify denial", v.State)
	}
	if !strings.Contains(v.Status, "alerts unavailable") {
		t.Errorf("status should mark alerts unavailable: %q", v.Status)
	}
	// Grant restored: the next batch posts again and clears the latch.
	h.denyNotify = false
	h.feeds[url] = []byte(strings.ReplaceAll(rssFixture,
		"</channel>",
		`<item><title>Fresh Post</title><link>https://news.example.com/fresh</link><guid>fresh-1</guid></item><item><title>Fresher</title><link>https://news.example.com/fresher</link><guid>fresh-2</guid></item></channel>`))
	h.app.poll(context.Background())
	if h.app.notifyDown {
		t.Error("notifyDown should clear on success")
	}
	if len(h.notifies) == 0 {
		t.Error("restored grant should resume posting")
	}
}

func TestFeedBoundEnforced(t *testing.T) {
	h := newHarness(t)
	for i := 1; i <= maxFeeds; i++ {
		url := fmt.Sprintf("https://f%d.example.com/feed", i)
		h.feeds[url] = []byte(rssFixture)
		v := h.act(t, 2, "subscribe", "", map[string]string{"feed_url": url})
		if feedItem(v, fmt.Sprintf("feed_%d", i)) == nil {
			t.Fatalf("feed %d did not subscribe", i)
		}
	}
	if a := findAction(h.view(t, 2), "subscribe"); !a.Disabled {
		t.Error("subscribe action should disable at the feed bound")
	}
	if _, err := h.tryAct(2, "subscribe", "", map[string]string{"feed_url": "https://extra.example.com/f"}); err == nil {
		t.Error("17th subscribe should be rejected")
	}
	if len(h.app.feeds) != maxFeeds {
		t.Errorf("feeds = %d, want %d", len(h.app.feeds), maxFeeds)
	}
}

func TestItemBoundAndTruncationIndicator(t *testing.T) {
	h := newHarness(t)
	url := "https://news.example.com/feed.xml"
	h.feeds[url] = []byte(makeRSS("Big Feed", maxItemsPerFeed+10, t0))
	h.act(t, 2, "subscribe", "", map[string]string{"feed_url": url})
	if got := len(h.app.feeds[0].Items); got != maxItemsPerFeed {
		t.Fatalf("stored items = %d, want capped at %d", got, maxItemsPerFeed)
	}
	// The newest items survive the cap.
	var newest bool
	for _, it := range h.app.feeds[0].Items {
		if it.Title == "Story 1" {
			newest = true
		}
	}
	if !newest {
		t.Error("the newest headline should survive the per-feed cap")
	}

	// With a second feed the snapshot exceeds the row budget and the
	// tail becomes an honest "… and N more" row.
	url2 := "https://two.example.com/feed"
	h.feeds[url2] = []byte(makeRSS("Big Feed Two", maxItemsPerFeed, t0))
	v := h.act(t, 2, "subscribe", "", map[string]string{"feed_url": url2})
	more := findItem(v, itemMore)
	if more == nil || !strings.Contains(more.Label, "and") {
		t.Fatalf("truncation indicator missing: %+v", v.Items)
	}
	if len(v.Items) != maxListItems {
		t.Errorf("snapshot rows = %d, want %d", len(v.Items), maxListItems)
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	h := newHarness(t)
	url := "https://news.example.com/feed.xml"
	h.feeds[url] = []byte(rssFixture)
	h.act(t, 2, "subscribe", "", map[string]string{"feed_url": url})
	h.act(t, 2, "mark_read", "item_1", nil)
	if err := h.app.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Relaunch: same fs, new instance. No fetch needed — stored state
	// renders immediately.
	h2 := newHarness(t)
	h2.files = h.files
	v := h2.view(t, 2)
	feed := feedItem(v, "feed_1")
	if feed == nil || feed.Label != "Example News" {
		t.Fatalf("feed did not persist: %+v", v.Items)
	}
	if !strings.Contains(feed.Detail, "2 unread of 3") {
		t.Errorf("restored detail = %q, want 2 unread of 3", feed.Detail)
	}
	if !strings.Contains(findItem(v, "item_1").Label, "[x]") {
		t.Error("read state did not persist")
	}
	if h2.fetchCount() != 0 {
		t.Error("restoring state should not fetch")
	}
	// Unsubscribing on the relaunched instance removes the saved feed.
	v = h2.act(t, 2, "unsubscribe", "feed_1", nil)
	var st diskState
	if err := json.Unmarshal(h2.files[statePath], &st); err != nil || len(st.Feeds) != 0 {
		t.Error("unsubscribe did not persist")
	}
}

func TestFailingFeedSurvivesRelaunch(t *testing.T) {
	h := newHarness(t)
	url := "https://blocked.example.com/feed"
	h.fetchErr[url] = errors.New(`policy_denied: host "blocked.example.com" is not in allowed hosts whitelist`)
	h.act(t, 2, "subscribe", "", map[string]string{"feed_url": url})
	if err := h.app.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Relaunch: the persisted status renders the honest summary line —
	// no statusNote exists yet to win the line.
	h2 := newHarness(t)
	h2.files = h.files
	v := h2.view(t, 2)
	if !strings.Contains(v.Status, "1 feed error") {
		t.Errorf("relaunch status = %q, want the failing-feed count", v.Status)
	}
	if d := feedItem(v, "feed_1").Detail; !strings.Contains(d, "not on the operator allowlist") {
		t.Errorf("restored feed detail = %q", d)
	}
}

func TestCorruptSaveStartsEmpty(t *testing.T) {
	h := newHarness(t)
	h.files[statePath] = []byte("{definitely not json")
	v := h.view(t, 2)
	if !strings.Contains(v.Error, "unreadable") {
		t.Errorf("corrupt save error = %q", v.Error)
	}
	if findItem(v, itemEmpty) == nil {
		t.Error("corrupt save should render the empty notice")
	}
	// Still usable: subscribing writes a healed file.
	h.feeds["https://x.example.com/f"] = []byte(rssFixture)
	h.act(t, 2, "subscribe", "", map[string]string{"feed_url": "https://x.example.com/f"})
	var st diskState
	if err := json.Unmarshal(h.files[statePath], &st); err != nil || len(st.Feeds) != 1 {
		t.Error("subscribe after corrupt load did not heal the file")
	}
}

func TestFSDenialIsHonestButUsable(t *testing.T) {
	h := newHarness(t)
	h.denyFS = true
	h.feeds["https://x.example.com/f"] = []byte(rssFixture)
	v := h.view(t, 2)
	if !strings.Contains(v.Error, "denied") {
		t.Errorf("fs denial should land on the error banner: %q", v.Error)
	}
	v = h.act(t, 2, "subscribe", "", map[string]string{"feed_url": "https://x.example.com/f"})
	if !strings.Contains(v.Status, "save failed") {
		t.Errorf("subscribe under fs denial status = %q", v.Status)
	}
}

func TestRefreshAllAndRefreshSelection(t *testing.T) {
	h := newHarness(t)
	u1, u2 := "https://a.example.com/f", "https://b.example.com/f"
	h.feeds[u1] = []byte(rssFixture)
	h.feeds[u2] = []byte(atomFixture)
	h.act(t, 2, "subscribe", "", map[string]string{"feed_url": u1})
	h.act(t, 2, "subscribe", "", map[string]string{"feed_url": u2})
	if h.fetchCount() != 2 {
		t.Fatalf("fetches = %d", h.fetchCount())
	}
	// No selection refreshes everything.
	h.act(t, 2, "refresh", "", nil)
	if h.fetchCount() != 4 {
		t.Errorf("refresh-all fetches = %d, want 4", h.fetchCount())
	}
	// A feed row refreshes just that feed.
	h.act(t, 2, "refresh", "feed_1", nil)
	if h.fetchCount() != 5 {
		t.Errorf("selective refresh fetches = %d, want 5", h.fetchCount())
	}
	// A headline row is an honest rejection.
	v := h.act(t, 2, "refresh", "item_1", nil)
	if !strings.Contains(v.Status, "applies to feeds") {
		t.Errorf("headline-refresh status = %q", v.Status)
	}
}

func TestRoutes(t *testing.T) {
	h := newHarness(t)
	url := "https://news.example.com/feed.xml"
	h.feeds[url] = []byte(rssFixture)

	var sub map[string]any
	h.route(t, "subscribe", map[string]string{"url": url}, &sub)
	if sub["id"] != "feed_1" || sub["headlines"] != float64(3) {
		t.Fatalf("subscribe route = %+v", sub)
	}
	var st map[string]any
	h.route(t, "state", nil, &st)
	if st["feeds_total"] != float64(1) || st["unread"] != float64(3) {
		t.Errorf("state = %+v", st)
	}
	var list map[string]any
	h.route(t, "list", map[string]any{"feed": "feed_1"}, &list)
	if list["count"] != float64(3) {
		t.Errorf("list = %+v", list)
	}
	h.route(t, "list", map[string]any{"unread_only": true}, &list)
	if list["count"] != float64(3) {
		t.Errorf("unread list = %+v", list)
	}
	var mark map[string]any
	h.route(t, "mark_read", map[string]string{"id": "item_1"}, &mark)
	if mark["read"] != true {
		t.Errorf("mark_read = %+v", mark)
	}
	h.route(t, "list", map[string]any{"unread_only": true}, &list)
	if list["count"] != float64(2) {
		t.Errorf("unread list after mark = %+v", list)
	}
	var refr map[string]any
	h.route(t, "refresh", map[string]string{"id": "feed_1"}, &refr)
	if refr["refreshed"] != float64(1) {
		t.Errorf("refresh route = %+v", refr)
	}
	var unsub map[string]any
	h.route(t, "unsubscribe", map[string]string{"id": "feed_1"}, &unsub)
	if unsub["removed"] != true {
		t.Errorf("unsubscribe = %+v", unsub)
	}
	// Route-level errors are honest IPC errors.
	hd := h.handlers["mark_read"]
	raw, _ := json.Marshal(map[string]string{"id": "item_999"})
	if _, err := hd(context.Background(), raw); err == nil {
		t.Error("mark_read of unknown item should be an IPC error")
	}
}

func TestPollFetchesAndReports(t *testing.T) {
	h := newHarness(t)
	url := "https://news.example.com/feed.xml"
	h.feeds[url] = []byte(rssFixture)
	h.act(t, 2, "subscribe", "", map[string]string{"feed_url": url})

	h.feeds[url] = []byte(strings.ReplaceAll(rssFixture,
		"</channel>",
		`<item><title>Poll Post</title><link>https://news.example.com/poll</link><guid>poll-1</guid><pubDate>Thu, 08 Oct 2026 08:00:00 +0000</pubDate></item></channel>`))
	h.advance(10 * time.Minute)
	h.app.poll(context.Background())
	if h.fetchCount() != 2 {
		t.Errorf("poll fetches = %d, want 2", h.fetchCount())
	}
	if len(h.notifies) != 1 || !strings.Contains(h.notifies[0].Body, "1 new headline") {
		t.Errorf("poll notifies = %+v", h.notifies)
	}
	if got := h.app.feeds[0].Items[0].Title; got != "Poll Post" {
		t.Errorf("newest item = %q, want the poll find", got)
	}
	v := h.view(t, 2)
	if !strings.Contains(v.Status, "1 new headline") {
		t.Errorf("poll status = %q", v.Status)
	}
}

func TestRunPollsOnIntervalAndExits(t *testing.T) {
	h := newHarness(t)
	url := "https://news.example.com/feed.xml"
	h.feeds[url] = []byte(rssFixture)
	h.act(t, 2, "subscribe", "", map[string]string{"feed_url": url})
	setPollInterval(h.app, 10*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.app.Run(ctx) }()

	// The ticker drives real fetches beyond the subscribe-time one.
	deadline := time.Now().Add(2 * time.Second)
	for h.fetchCount() < 3 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if h.fetchCount() < 3 {
		t.Fatalf("poll interval never fetched: fetches = %d", h.fetchCount())
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit on cancel — goroutine leak")
	}
}

func TestUnknownActionIsIPCError(t *testing.T) {
	h := newHarness(t)
	if _, err := h.tryAct(2, "launch_missiles", "", nil); err == nil {
		t.Error("unknown action should be an IPC error")
	}
}

// A transient fs/read failure (a canceled call racing the first view, a
// host I/O error, the read-size bound) is not "no save yet": the app must
// say so honestly, retry the load, and never let the next change overwrite
// the saved subscriptions.
func TestTransientReadErrorNeverOverwritesFeeds(t *testing.T) {
	h := newHarness(t)
	url := "https://news.example.com/feed.xml"
	h.feeds[url] = []byte(rssFixture)
	h.act(t, 2, "subscribe", "", map[string]string{"feed_url": url})

	h2 := newHarness(t)
	h2.files = h.files
	h2.readErr = context.Canceled // first fs/read fails once, transiently
	url2 := "https://other.example.com/feed.xml"
	h2.feeds[url2] = []byte(rssFixture)

	v := h2.view(t, 2)
	if v.State != sdk.ViewError || v.Error == "" {
		t.Fatalf("transient read failure view = %+v, want an honest error", v)
	}

	// The retry loads the real file; a subscribe then extends it, not
	// replaces it.
	v = h2.act(t, 2, "subscribe", "", map[string]string{"feed_url": url2})
	var st diskState
	if err := json.Unmarshal(h2.files[statePath], &st); err != nil {
		t.Fatalf("post-change save is not valid json: %v", err)
	}
	if len(st.Feeds) != 2 {
		t.Fatalf("%d feed(s) on disk after a transient read error and one subscribe, want 2", len(st.Feeds))
	}
}
