// Package rss is Gostalgia's feed reader: subscribe to RSS 2.0 and Atom
// feeds, poll them on a bounded interval while the app runs, and read
// headlines with read/unread tracking. It exists to exercise
// capability-gated egress — every fetch goes through net/fetch under the
// net.egress grant, gated again by the operator's network policy.
//
// Feeds live on arbitrary domains, so the operator must allowlist each
// feed host (or allow all hosts) before headlines arrive; a denied,
// unlisted, unreachable, or malformed feed degrades to honest status
// text on its row instead of corrupting or emptying state. Subscriptions
// and read state persist in the app-private partition
// (/apps/data/com.gostalgia.rss/state.json) via atomic fs/save. New
// headlines found by a refresh or poll post one notify/post summary
// while the notify grant is held; without it the app logs once and keeps
// working.
package rss

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"gostalgia/sdk"
)

// ID is the reverse-DNS identifier for the RSS reader.
const ID = "com.gostalgia.rss"

// statePath is the single persisted document inside the app-private
// partition. fs/save stages and renames atomically, so a crash mid-write
// leaves the previous state intact.
const statePath = "/apps/data/com.gostalgia.rss/state.json"

const stateVersion = 1

// Bounds. Feeds live on arbitrary hosts and each poll costs a fetch, so
// the subscription list stays short; stored headlines and rendered rows
// stay bounded so the save file and the snapshot stay small.
const (
	maxFeeds        = 16
	maxItemsPerFeed = 50
	maxListItems    = 64
	maxTitleLen     = 200
	maxLinkLen      = 2048
	maxURLLen       = 2048
	maxStatusLen    = 300
	fetchTimeoutSec = 15
)

// pollInterval is the background refresh cadence while the app runs.
const pollInterval = 5 * time.Minute

// Reserved item IDs that are notices, not feeds or headlines.
// sanitizeLocked reassigns any stored id that collides with one.
const (
	itemEmpty = "empty"
	itemMore  = "more"
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

// diskState is the persisted document: subscriptions with their stored
// headlines (read state rides inside each headline) plus the id counters
// that keep future feed and item ids unique.
type diskState struct {
	Version int    `json:"version"`
	Feeds   []Feed `json:"feeds"`
	FeedSeq int    `json:"feed_seq"`
	ItemSeq int    `json:"item_seq"`
}

// fetchOutcome summarizes one fetch batch — manual refresh, subscribe's
// first fetch, or a background poll cycle. newItems counts only items
// merged into an already-fetched feed: a feed's first successful fetch
// is baseline sync, not news.
type fetchOutcome struct {
	attempted int
	succeeded int
	newItems  int
	names     []string // titles of feeds that contributed new items
}

// RSS is the running in-process instance. All state is guarded by mu.
type RSS struct {
	app       *sdk.Context
	now       func() time.Time
	pollEvery time.Duration

	mu         sync.Mutex
	loaded     bool
	feeds      []Feed
	feedSeq    int
	itemSeq    int
	dirty      bool      // in-memory change has not reached disk
	lastPoll   time.Time // last completed background poll (session state)
	statusNote string    // transient feedback line; cleared on each action
	lastError  string

	notifyDown bool // notify/post failed; logged once, stays quiet
}

// Factory constructs a fresh, uninitialized RSS instance.
func Factory() (sdk.Instance, error) {
	return &RSS{now: time.Now, pollEvery: pollInterval}, nil
}

// setClock injects a deterministic clock for tests.
func setClock(r *RSS, now func() time.Time) { r.now = now }

// setPollInterval injects a short poll cadence for tests.
func setPollInterval(r *RSS, d time.Duration) { r.pollEvery = d }

// Init registers routes and the presentation callbacks. State loads
// lazily on first use so Init stays prompt.
func (r *RSS) Init(app *sdk.Context) error {
	r.app = app
	if r.now == nil {
		r.now = time.Now
	}
	if r.pollEvery <= 0 {
		r.pollEvery = pollInterval
	}
	for _, route := range []struct {
		name string
		h    sdk.Handler
	}{
		{"state", r.stateRoute},
		{"subscribe", r.subscribeRoute},
		{"unsubscribe", r.unsubscribeRoute},
		{"refresh", r.refreshRoute},
		{"list", r.listRoute},
		{"mark_read", r.markReadRoute},
		{"mark_unread", r.markUnreadRoute},
	} {
		if err := app.Handle(route.name, route.h); err != nil {
			return err
		}
	}
	return app.Present(r.view, r.act)
}

// Run polls every feed on a bounded interval until canceled. Fetches run
// outside the state lock so a slow feed never stalls rendering.
func (r *RSS) Run(ctx context.Context) error {
	r.app.Log.Info("rss running")
	ticker := time.NewTicker(r.pollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.poll(ctx)
		}
	}
}

// Stop flushes a pending save — the retry after an earlier save failure.
// A failed flush is logged, never fatal.
func (r *RSS) Stop(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.loaded && r.dirty {
		if err := r.persistLocked(ctx); err != nil && r.app.Log != nil {
			r.app.Log.Warn("rss: final save failed", "err", err)
		}
	}
	return nil
}

// poll runs one background refresh cycle across every subscription. It
// is quiet unless it finds news: failures land on the feed rows, not in
// logs or popups.
func (r *RSS) poll(ctx context.Context) {
	r.mu.Lock()
	if err := r.ensureLoadedLocked(ctx); err != nil {
		r.lastError = err.Error()
		r.mu.Unlock()
		return
	}
	ids := make([]string, 0, len(r.feeds))
	for i := range r.feeds {
		ids = append(ids, r.feeds[i].ID)
	}
	r.mu.Unlock()
	if len(ids) == 0 {
		return
	}
	out := r.fetchFeeds(ctx, ids)
	r.mu.Lock()
	r.lastPoll = r.now()
	if out.newItems > 0 {
		r.statusNote = fmt.Sprintf("%d new headline%s — %s",
			out.newItems, plural(out.newItems), namesSummary(out.names))
	}
	r.mu.Unlock()
}

// ensureLoadedLocked reads and decodes state.json once. A missing file
// is simply no subscriptions. A corrupt file is an honest banner over an
// empty reader; the bad bytes are left alone until the next real change
// saves over them. A denied read is also reported, since nothing will
// persist without the grant.
func (r *RSS) ensureLoadedLocked(ctx context.Context) error {
	if r.loaded {
		return nil
	}
	r.loaded = true

	var out struct {
		DataBase64 string `json:"data_base64"`
	}
	err := r.app.Call(ctx, "fs/read", map[string]string{"path": statePath}, &out)
	if err != nil {
		if looksDenied(err) {
			r.lastError = fmt.Sprintf("storage access denied (%v) — subscriptions will not be saved", err)
		}
		return nil // no save yet: empty reader
	}
	data, err := base64.StdEncoding.DecodeString(out.DataBase64)
	if err != nil {
		r.lastError = "saved reader state is unreadable — starting empty; the next change saves over it"
		return nil
	}
	var st diskState
	if err := json.Unmarshal(data, &st); err != nil || st.Version != stateVersion {
		r.lastError = "saved reader state is unreadable — starting empty; the next change saves over it"
		return nil
	}
	r.feeds = st.Feeds
	r.feedSeq = st.FeedSeq
	r.itemSeq = st.ItemSeq
	if r.sanitizeLocked() {
		// The loaded file needed repair; write the healed copy back now so
		// a crash does not reload the same damage. Best-effort: the
		// in-memory state is already sound, so a failure only shows up on
		// the banner.
		if err := r.persistLocked(ctx); err != nil {
			r.lastError = fmt.Sprintf("save failed: %v", err)
		}
	}
	return nil
}

// looksDenied reports whether an fs error is a grant denial rather than
// a missing file. The two are intentionally different: absence is normal
// on first launch, denial means nothing the user does will persist.
func looksDenied(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "denied") || strings.Contains(msg, "permission")
}

// sanitizeLocked repairs loaded state in place and reports whether it
// changed anything: feed/item seqs pushed past every stored id,
// duplicate or reserved ids reassigned, texts normalized and bounded,
// per-feed items deduped by key and capped, the feed list capped.
func (r *RSS) sanitizeLocked() bool {
	changed := false
	for _, f := range r.feeds {
		var n int
		if _, err := fmt.Sscanf(f.ID, "feed_%d", &n); err == nil && n > r.feedSeq {
			r.feedSeq = n
		}
		for _, h := range f.Items {
			var m int
			if _, err := fmt.Sscanf(h.ID, "item_%d", &m); err == nil && m > r.itemSeq {
				r.itemSeq = m
			}
		}
	}
	if len(r.feeds) > maxFeeds {
		r.feeds = r.feeds[:maxFeeds]
		changed = true
	}
	now := r.now()
	seenFeeds := map[string]bool{itemEmpty: true, itemMore: true}
	seenItems := map[string]bool{itemEmpty: true, itemMore: true}
	kept := r.feeds[:0]
	for _, f := range r.feeds {
		if f.ID == "" || seenFeeds[f.ID] {
			r.feedSeq++
			f.ID = fmt.Sprintf("feed_%d", r.feedSeq)
			changed = true
		}
		seenFeeds[f.ID] = true
		if u, err := normalizeFeedURL(f.URL); err != nil {
			// A stored feed that no longer validates stays — it is user
			// data, and unsubscribe still removes it — but it cannot
			// fetch, which the status says honestly.
			if f.Status == "" {
				f.Status = "stored feed URL is no longer valid — unsubscribe to remove it"
				changed = true
			}
		} else if u != f.URL {
			f.URL = u
			changed = true
		}
		if t := cleanText(f.Title); t != f.Title {
			f.Title = t
			changed = true
		}
		if f.Title == "" {
			f.Title = feedNameFallback(f.URL)
			changed = true
		}
		if f.AddedAt.IsZero() || f.AddedAt.After(now) {
			f.AddedAt = now
			changed = true
		}
		if len(f.Status) > maxStatusLen {
			f.Status = f.Status[:maxStatusLen]
			changed = true
		}
		if len(f.Items) > maxItemsPerFeed {
			f.Items = f.Items[:maxItemsPerFeed]
			changed = true
		}
		seenKeys := map[string]bool{}
		keptItems := f.Items[:0]
		for _, h := range f.Items {
			if h.Key == "" || seenKeys[h.Key] {
				changed = true
				continue // keyless or duplicate entries cannot dedupe: drop
			}
			seenKeys[h.Key] = true
			if h.ID == "" || seenItems[h.ID] {
				r.itemSeq++
				h.ID = fmt.Sprintf("item_%d", r.itemSeq)
				changed = true
			}
			seenItems[h.ID] = true
			if t := cleanText(h.Title); t != h.Title {
				h.Title = t
				changed = true
			}
			if h.Title == "" {
				h.Title = "(untitled headline)"
				changed = true
			}
			if len(h.Link) > maxLinkLen {
				h.Link = h.Link[:maxLinkLen]
				changed = true
			}
			if h.Published.After(now) {
				h.Published = now
				changed = true
			}
			keptItems = append(keptItems, h)
		}
		f.Items = keptItems
		sortHeadlines(f.Items)
		kept = append(kept, f)
	}
	r.feeds = kept
	return changed
}

// persistLocked writes the whole state document atomically. Callers hold
// mu.
func (r *RSS) persistLocked(ctx context.Context) error {
	feeds := r.feeds
	if feeds == nil {
		feeds = []Feed{}
	}
	data, err := json.Marshal(diskState{
		Version: stateVersion,
		Feeds:   feeds,
		FeedSeq: r.feedSeq,
		ItemSeq: r.itemSeq,
	})
	if err != nil {
		return err
	}
	err = r.app.Call(ctx, "fs/save", map[string]any{
		"path":        statePath,
		"data_base64": base64.StdEncoding.EncodeToString(data),
	}, nil)
	if err == nil {
		r.dirty = false
	}
	return err
}

// saveErrorLocked persists and wraps a failure so the report says what
// failed, not just why the fs call did. Callers hold mu.
func (r *RSS) saveErrorLocked(ctx context.Context) error {
	if err := r.persistLocked(ctx); err != nil {
		return fmt.Errorf("save failed: %w", err)
	}
	return nil
}

// notifyLocked posts one new-headlines summary through notify/post. The
// notify capability may be absent or revoked; failures are logged once
// and the flag suppresses retry noise until a call succeeds again.
func (r *RSS) notifyLocked(ctx context.Context, body string) bool {
	err := r.app.Call(ctx, "notify/post", map[string]string{
		"severity": "info",
		"title":    "RSS",
		"body":     body,
	}, nil)
	if err != nil {
		if !r.notifyDown && r.app.Log != nil {
			r.app.Log.Info("rss: notify/post unavailable", "err", err)
		}
		r.notifyDown = true
		return false
	}
	r.notifyDown = false
	return true
}

// --- fetching ---

// fetchURL is the app's only egress: one GET through net/fetch under the
// net.egress grant, bounded by fetchTimeoutSec. Policy rejections (no
// grant, egress disabled, host not allowlisted) come back as honest
// status text via describeFetchFailure; non-2xx responses are failures
// too.
func (r *RSS) fetchURL(ctx context.Context, url string) ([]byte, error) {
	var out struct {
		Status     int    `json:"status"`
		StatusText string `json:"status_text"`
		DataBase64 string `json:"data_base64"`
	}
	if err := r.app.Call(ctx, "net/fetch", map[string]any{
		"url":             url,
		"timeout_seconds": fetchTimeoutSec,
	}, &out); err != nil {
		return nil, fmt.Errorf("%s", describeFetchFailure(err))
	}
	if out.Status < 200 || out.Status > 299 {
		return nil, fmt.Errorf("feed server returned HTTP %d", out.Status)
	}
	data, err := base64.StdEncoding.DecodeString(out.DataBase64)
	if err != nil {
		return nil, fmt.Errorf("fetch returned unreadable data: %v", err)
	}
	return data, nil
}

// fetchFeeds fetches each named feed sequentially — outside mu, so a
// slow or dead feed never stalls rendering — and merges each result back
// under the lock. A feed unsubscribed mid-batch is skipped honestly. At
// the end it persists once and posts one notify summary for genuinely
// new headlines.
func (r *RSS) fetchFeeds(ctx context.Context, ids []string) fetchOutcome {
	var out fetchOutcome
	for _, id := range ids {
		r.mu.Lock()
		var url string
		if f := r.feedByIDLocked(id); f != nil {
			url = f.URL
		}
		r.mu.Unlock()
		if url == "" {
			continue
		}

		body, err := r.fetchURL(ctx, url)
		out.attempted++

		r.mu.Lock()
		f := r.feedByIDLocked(id)
		if f == nil {
			r.mu.Unlock()
			continue
		}
		f.CheckedAt = r.now()
		switch {
		case err != nil:
			if ctx.Err() == nil {
				f.Status = err.Error()
			}
		case ctx.Err() != nil:
			// Shutdown raced the fetch: leave the last honest status.
		default:
			pf, perr := parseFeed(body)
			if perr != nil {
				f.Status = perr.Error()
			} else {
				wasFetched := !f.FetchedAt.IsZero()
				f.Status = ""
				f.FetchedAt = r.now()
				out.succeeded++
				if added := r.mergeLocked(f, pf); added > 0 && wasFetched {
					out.newItems += added
					out.names = append(out.names, f.Title)
				}
			}
		}
		if len(f.Status) > maxStatusLen {
			f.Status = f.Status[:maxStatusLen]
		}
		r.mu.Unlock()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if out.attempted > 0 {
		r.dirty = true
		if err := r.persistLocked(ctx); err != nil {
			r.lastError = fmt.Sprintf("save failed: %v", err)
		}
		// Background polls and IPC-route fetches change the snapshot with
		// no action reply to carry it; the version-3 hint is best-effort
		// and its error deliberately ignored.
		if r.app != nil {
			_ = r.app.Invalidate(ctx)
		}
	}
	if out.newItems > 0 {
		r.notifyLocked(ctx, fmt.Sprintf("%d new headline%s — %s",
			out.newItems, plural(out.newItems), namesSummary(out.names)))
	}
	return out
}

// mergeLocked folds a parsed feed into stored headlines: dedupes by
// content key, assigns fresh item_N ids, adopts the feed's own title,
// sorts newest-first, and caps the stored list. Callers hold mu.
func (r *RSS) mergeLocked(f *Feed, pf parsedFeed) int {
	existing := make(map[string]bool, len(f.Items))
	for _, h := range f.Items {
		existing[h.Key] = true
	}
	added := 0
	for _, it := range pf.Items {
		key := itemKey(it)
		if existing[key] {
			continue
		}
		existing[key] = true
		title := it.Title
		if title == "" {
			title = "(untitled headline)"
		}
		link := it.Link
		if len(link) > maxLinkLen {
			link = link[:maxLinkLen]
		}
		r.itemSeq++
		f.Items = append(f.Items, Headline{
			ID:        fmt.Sprintf("item_%d", r.itemSeq),
			Key:       key,
			Title:     title,
			Link:      link,
			Published: it.Published,
		})
		added++
	}
	if pf.Title != "" {
		f.Title = pf.Title
	}
	sortHeadlines(f.Items)
	if len(f.Items) > maxItemsPerFeed {
		f.Items = f.Items[:maxItemsPerFeed]
	}
	return added
}

// feedByIDLocked finds a feed by its feed_N id. Callers hold mu.
func (r *RSS) feedByIDLocked(id string) *Feed {
	for i := range r.feeds {
		if r.feeds[i].ID == id {
			return &r.feeds[i]
		}
	}
	return nil
}

// findHeadlineLocked locates one stored headline by its item_N id,
// returning the feed and index. Callers hold mu.
func (r *RSS) findHeadlineLocked(id string) (*Feed, int) {
	for i := range r.feeds {
		for j := range r.feeds[i].Items {
			if r.feeds[i].Items[j].ID == id {
				return &r.feeds[i], j
			}
		}
	}
	return nil, -1
}

// --- mutations: error-returning, shared by presentation and routes ---

// subscribeLocked validates and records a new subscription. The feed
// lands in state before its first fetch, so a subscription survives even
// if the fetch cannot complete; the fetch's outcome lands on the feed
// row as honest status.
func (r *RSS) subscribeLocked(ctx context.Context, raw string) (string, error) {
	u, err := normalizeFeedURL(raw)
	if err != nil {
		return "", err
	}
	for _, f := range r.feeds {
		if f.URL == u {
			return "", fmt.Errorf("already subscribed to %s", u)
		}
	}
	if len(r.feeds) >= maxFeeds {
		return "", fmt.Errorf("the subscription list is full (%d feeds) — unsubscribe one first", maxFeeds)
	}
	r.feedSeq++
	id := fmt.Sprintf("feed_%d", r.feedSeq)
	r.feeds = append(r.feeds, Feed{
		ID:      id,
		URL:     u,
		Title:   feedNameFallback(u),
		AddedAt: r.now(),
	})
	r.statusNote = fmt.Sprintf("subscribed to %s — fetching", u)
	r.dirty = true
	if err := r.saveErrorLocked(ctx); err != nil {
		return "", err
	}
	return id, nil
}

// unsubscribeLocked drops a feed and every stored headline it carried.
func (r *RSS) unsubscribeLocked(ctx context.Context, itemID string) error {
	if itemID == "" {
		return fmt.Errorf("select a feed row to unsubscribe")
	}
	if itemID == itemEmpty || itemID == itemMore {
		return fmt.Errorf("that row is a notice, not a feed")
	}
	if !strings.HasPrefix(itemID, "feed_") {
		return fmt.Errorf("unsubscribe applies to feeds — select a feed row, not a headline")
	}
	for i := range r.feeds {
		if r.feeds[i].ID == itemID {
			title := r.feeds[i].Title
			n := len(r.feeds[i].Items)
			r.feeds = append(r.feeds[:i], r.feeds[i+1:]...)
			r.statusNote = fmt.Sprintf("unsubscribed from %s — %d stored headline%s removed", title, n, plural(n))
			r.dirty = true
			return r.saveErrorLocked(ctx)
		}
	}
	return fmt.Errorf("no feed %q — select a feed row to unsubscribe", itemID)
}

// refreshTargetsLocked resolves the refresh action's selection: no
// selection refreshes every feed, a feed row refreshes just that one,
// anything else is an honest rejection.
func (r *RSS) refreshTargetsLocked(itemID string) ([]string, error) {
	if itemID == "" {
		if len(r.feeds) == 0 {
			return nil, fmt.Errorf("no feeds to refresh — subscribe first")
		}
		ids := make([]string, 0, len(r.feeds))
		for i := range r.feeds {
			ids = append(ids, r.feeds[i].ID)
		}
		return ids, nil
	}
	if !strings.HasPrefix(itemID, "feed_") {
		return nil, fmt.Errorf("refresh applies to feeds — select a feed row, or refresh all with nothing selected")
	}
	if r.feedByIDLocked(itemID) == nil {
		return nil, fmt.Errorf("no feed %q", itemID)
	}
	return []string{itemID}, nil
}

// markHeadlineLocked flips read state: on a headline row it marks that
// one; on a feed row (read=true only) it marks every headline on that
// feed.
func (r *RSS) markHeadlineLocked(ctx context.Context, itemID string, read bool) error {
	if itemID == "" {
		return fmt.Errorf("select a headline to mark, or a feed row to mark all of it read")
	}
	if itemID == itemEmpty || itemID == itemMore {
		return fmt.Errorf("that row is a notice, not a headline")
	}
	if strings.HasPrefix(itemID, "feed_") {
		if !read {
			return fmt.Errorf("only a headline can be marked unread — select a headline row")
		}
		f := r.feedByIDLocked(itemID)
		if f == nil {
			return fmt.Errorf("no feed %q — select a feed row", itemID)
		}
		n := 0
		for i := range f.Items {
			if !f.Items[i].Read {
				f.Items[i].Read = true
				n++
			}
		}
		if n == 0 {
			return fmt.Errorf("nothing unread on %q", f.Title)
		}
		r.statusNote = fmt.Sprintf("marked %d headline%s read on %s", n, plural(n), f.Title)
		r.dirty = true
		return r.saveErrorLocked(ctx)
	}
	f, idx := r.findHeadlineLocked(itemID)
	if f == nil {
		return fmt.Errorf("no headline %q — select a headline row", itemID)
	}
	h := &f.Items[idx]
	if h.Read == read {
		if read {
			return fmt.Errorf("%q is already read", h.Title)
		}
		return fmt.Errorf("%q is already unread", h.Title)
	}
	h.Read = read
	if read {
		r.statusNote = fmt.Sprintf("marked %q read", h.Title)
	} else {
		r.statusNote = fmt.Sprintf("marked %q unread", h.Title)
	}
	r.dirty = true
	return r.saveErrorLocked(ctx)
}

// --- presentation ---

func (r *RSS) view(ctx context.Context) (sdk.View, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ensureLoadedLocked(ctx); err != nil {
		return sdk.View{Title: "RSS", State: sdk.ViewError, Error: err.Error()}, nil
	}
	return r.viewLocked(), nil
}

func (r *RSS) act(ctx context.Context, req sdk.ActionRequest) (sdk.View, error) {
	r.mu.Lock()
	if err := r.ensureLoadedLocked(ctx); err != nil {
		r.mu.Unlock()
		return sdk.View{Title: "RSS", State: sdk.ViewError, Error: err.Error()}, nil
	}
	r.lastError = ""
	r.statusNote = ""

	var fetchIDs []string
	var err error
	switch req.Action {
	case "subscribe":
		var id string
		id, err = r.subscribeLocked(ctx, req.Values["feed_url"])
		if err == nil {
			fetchIDs = []string{id}
		}
	case "unsubscribe":
		err = r.unsubscribeLocked(ctx, req.ItemID)
	case "refresh":
		fetchIDs, err = r.refreshTargetsLocked(req.ItemID)
	case "mark_read":
		err = r.markHeadlineLocked(ctx, req.ItemID, true)
	case "mark_unread":
		err = r.markHeadlineLocked(ctx, req.ItemID, false)
	default:
		r.mu.Unlock()
		return sdk.View{}, fmt.Errorf("unknown action %q", req.Action)
	}
	r.mu.Unlock()

	if err == nil && len(fetchIDs) > 0 {
		out := r.fetchFeeds(ctx, fetchIDs)
		r.mu.Lock()
		r.noteFetchOutcomeLocked(req.Action, fetchIDs, out)
		r.mu.Unlock()
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		// Domain rejections land in the status line, not the error
		// banner: the reader stays fully usable.
		r.statusNote = err.Error()
	}
	return r.viewLocked(), nil
}

// noteFetchOutcomeLocked writes the status line for a user-driven fetch
// (subscribe's first fetch, a manual refresh). Background polls stay
// quiet unless they find news; interactive ones always report. Callers
// hold mu.
func (r *RSS) noteFetchOutcomeLocked(action string, ids []string, out fetchOutcome) {
	failed := out.attempted - out.succeeded
	switch {
	case out.newItems > 0:
		s := fmt.Sprintf("%d new headline%s — %s",
			out.newItems, plural(out.newItems), namesSummary(out.names))
		if failed > 0 {
			s += fmt.Sprintf(" · %d feed%s could not update", failed, plural(failed))
		}
		r.statusNote = s
	case action == "subscribe":
		f := r.feedByIDLocked(ids[0])
		if f == nil {
			return
		}
		if f.Status != "" {
			r.statusNote = "subscribed, but the first fetch failed — the feed row shows why"
		} else {
			r.statusNote = fmt.Sprintf("subscribed to %s — %d headline%s",
				f.Title, len(f.Items), plural(len(f.Items)))
		}
	case failed > 0:
		r.statusNote = fmt.Sprintf("refresh finished — %d feed%s could not update (see feed rows)",
			failed, plural(failed))
	case action == "refresh" && out.attempted > 0:
		r.statusNote = "feeds are up to date"
	}
}

func (r *RSS) viewLocked() sdk.View {
	now := r.now()

	unreadTotal, headlineTotal, failing := 0, 0, 0
	items := make([]sdk.Item, 0, len(r.feeds)+8)
	for i := range r.feeds {
		f := &r.feeds[i]
		unreadTotal += f.unread()
		headlineTotal += len(f.Items)
		if f.Status != "" {
			failing++
		}
		items = append(items, sdk.Item{ID: f.ID, Label: f.Title, Detail: feedDetail(f, now)})
	}

	// Headlines flatten newest-first across every feed, capped so the
	// snapshot stays inside the shared row budget.
	type row struct {
		h    *Headline
		feed string
	}
	rows := make([]row, 0, headlineTotal)
	for i := range r.feeds {
		f := &r.feeds[i]
		for j := range f.Items {
			rows = append(rows, row{h: &f.Items[j], feed: f.Title})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i].h.Published, rows[j].h.Published
		if a.Equal(b) {
			return itemSeqOf(rows[i].h) > itemSeqOf(rows[j].h)
		}
		if a.IsZero() {
			return false
		}
		if b.IsZero() {
			return true
		}
		return a.After(b)
	})

	budget := maxListItems - len(items)
	shown, truncated := rows, 0
	if len(rows) > budget {
		if budget > 0 {
			shown = rows[:budget-1]
		} else {
			shown = nil
		}
		truncated = len(rows) - len(shown)
	}
	for _, rw := range shown {
		items = append(items, headlineItem(rw.h, rw.feed, now))
	}
	if truncated > 0 {
		items = append(items, sdk.Item{
			ID:     itemMore,
			Label:  fmt.Sprintf("… and %d more", truncated),
			Detail: "the list is capped — older headlines stay stored until the per-feed bound trims them",
		})
	}
	if len(items) == 0 {
		items = append(items, sdk.Item{
			ID:     itemEmpty,
			Label:  "No feeds yet",
			Detail: "paste a feed URL in the field below and press Subscribe — each feed's host must be on the operator's network allowlist",
		})
	} else if headlineTotal == 0 {
		items = append(items, sdk.Item{
			ID:     itemEmpty,
			Label:  "No headlines yet",
			Detail: "feeds fetch on subscribe, on Refresh, and on the poll interval — failures show on the feed rows above",
		})
	}

	return sdk.View{
		Title:  "RSS",
		State:  sdk.ViewReady,
		Items:  items,
		Fields: []sdk.Field{{ID: "feed_url", Label: "Feed URL (https://…)"}},
		Actions: []sdk.Action{
			{ID: "subscribe", Label: "Subscribe", Disabled: len(r.feeds) >= maxFeeds},
			{ID: "refresh", Label: "Refresh", Disabled: len(r.feeds) == 0},
			{ID: "unsubscribe", Label: "Unsubscribe", Disabled: len(r.feeds) == 0},
			{ID: "mark_read", Label: "Mark read", Disabled: len(r.feeds) == 0},
			{ID: "mark_unread", Label: "Mark unread", Disabled: headlineTotal == 0},
		},
		Status: r.statusLocked(now, unreadTotal, failing),
		Error:  r.lastError,
	}
}

// feedDetail renders one subscription row: the last fetch's outcome in
// honest terms, or the unread count when healthy.
func feedDetail(f *Feed, now time.Time) string {
	switch {
	case f.CheckedAt.IsZero():
		return fmt.Sprintf("queued for first fetch · subscribed %s", ago(now.Sub(f.AddedAt)))
	case f.Status != "":
		d := f.Status + " · checked " + ago(now.Sub(f.CheckedAt))
		if !f.FetchedAt.IsZero() {
			d += fmt.Sprintf(" · showing %d headline%s from %s ago",
				len(f.Items), plural(len(f.Items)), ago(now.Sub(f.FetchedAt)))
		}
		return d
	default:
		return fmt.Sprintf("%d unread of %d · updated %s", f.unread(), len(f.Items), ago(now.Sub(f.FetchedAt)))
	}
}

// headlineItem renders one headline row: the checkbox lives in the
// label, the feed and age in the detail, so read state reads at a
// glance.
func headlineItem(h *Headline, feedTitle string, now time.Time) sdk.Item {
	box := "[ ] "
	if h.Read {
		box = "[x] "
	}
	detail := feedTitle
	if h.Published.IsZero() {
		detail += " · no date"
	} else {
		detail += " · " + ago(now.Sub(h.Published))
	}
	if h.Link != "" {
		link := h.Link
		if len(link) > 120 {
			link = link[:117] + "..."
		}
		detail += " · " + link
	}
	return sdk.Item{ID: h.ID, Label: box + h.Title, Detail: detail}
}

// statusLocked picks the line under the list: transient notes win, then
// the honest summary — feeds, unread, any fetch failures, poll cadence,
// and the "alerts unavailable" marker while notify is down.
func (r *RSS) statusLocked(now time.Time, unreadTotal, failing int) string {
	var s string
	if r.statusNote != "" {
		s = r.statusNote
	} else if len(r.feeds) == 0 {
		s = "no feeds yet — paste a feed URL below"
	} else {
		s = fmt.Sprintf("%d feed%s · %d unread", len(r.feeds), plural(len(r.feeds)), unreadTotal)
		if failing > 0 {
			s += fmt.Sprintf(" · %d feed error%s", failing, plural(failing))
		}
		if !r.lastPoll.IsZero() {
			s += " · polled " + ago(now.Sub(r.lastPoll))
		}
	}
	if r.notifyDown {
		s += " · alerts unavailable"
	}
	return s
}

// --- programmatic IPC routes ---

func (r *RSS) stateRoute(ctx context.Context, _ json.RawMessage) (any, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ensureLoadedLocked(ctx); err != nil {
		return nil, err
	}
	feeds := make([]map[string]any, 0, len(r.feeds))
	unread, headlines := 0, 0
	for i := range r.feeds {
		f := &r.feeds[i]
		u := f.unread()
		unread += u
		headlines += len(f.Items)
		feeds = append(feeds, map[string]any{
			"id":         f.ID,
			"url":        f.URL,
			"title":      f.Title,
			"status":     f.Status,
			"unread":     u,
			"headlines":  len(f.Items),
			"checked_at": f.CheckedAt,
			"fetched_at": f.FetchedAt,
		})
	}
	return map[string]any{
		"feeds":                 feeds,
		"feeds_total":           len(r.feeds),
		"headlines_total":       headlines,
		"unread":                unread,
		"poll_interval_seconds": int(r.pollEvery / time.Second),
		"notify_down":           r.notifyDown,
		"state_path":            statePath,
		"max_feeds":             maxFeeds,
		"max_items_per_feed":    maxItemsPerFeed,
	}, nil
}

func (r *RSS) subscribeRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var params struct {
		URL string `json:"url"`
	}
	if err := sdk.DecodeParams(raw, &params); err != nil {
		return nil, err
	}
	r.mu.Lock()
	if err := r.ensureLoadedLocked(ctx); err != nil {
		r.mu.Unlock()
		return nil, err
	}
	id, err := r.subscribeLocked(ctx, params.URL)
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	out := r.fetchFeeds(ctx, []string{id})
	r.mu.Lock()
	defer r.mu.Unlock()
	f := r.feedByIDLocked(id)
	resp := map[string]any{
		"id":          id,
		"new_items":   out.newItems,
		"headlines":   0,
		"feed_status": "",
	}
	if f != nil {
		resp["headlines"] = len(f.Items)
		resp["feed_status"] = f.Status
		resp["title"] = f.Title
	}
	return resp, nil
}

func (r *RSS) unsubscribeRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var params struct {
		ID string `json:"id"`
	}
	if err := sdk.DecodeParams(raw, &params); err != nil {
		return nil, err
	}
	if params.ID == "" {
		return nil, fmt.Errorf("params.id is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ensureLoadedLocked(ctx); err != nil {
		return nil, err
	}
	if err := r.unsubscribeLocked(ctx, params.ID); err != nil {
		return nil, err
	}
	return map[string]any{"id": params.ID, "removed": true}, nil
}

func (r *RSS) refreshRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var params struct {
		ID string `json:"id"`
	}
	if err := sdk.DecodeParams(raw, &params); err != nil {
		return nil, err
	}
	r.mu.Lock()
	if err := r.ensureLoadedLocked(ctx); err != nil {
		r.mu.Unlock()
		return nil, err
	}
	ids, err := r.refreshTargetsLocked(params.ID)
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	out := r.fetchFeeds(ctx, ids)
	return map[string]any{
		"refreshed": out.attempted,
		"succeeded": out.succeeded,
		"new_items": out.newItems,
	}, nil
}

func (r *RSS) listRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var params struct {
		Feed       string `json:"feed"`
		UnreadOnly bool   `json:"unread_only"`
	}
	if err := sdk.DecodeParams(raw, &params); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ensureLoadedLocked(ctx); err != nil {
		return nil, err
	}
	headlines := make([]map[string]any, 0)
	for i := range r.feeds {
		f := &r.feeds[i]
		if params.Feed != "" && f.ID != params.Feed {
			continue
		}
		for j := range f.Items {
			h := &f.Items[j]
			if params.UnreadOnly && h.Read {
				continue
			}
			headlines = append(headlines, map[string]any{
				"id":        h.ID,
				"feed":      f.ID,
				"title":     h.Title,
				"link":      h.Link,
				"published": h.Published,
				"read":      h.Read,
			})
		}
	}
	return map[string]any{"headlines": headlines, "count": len(headlines)}, nil
}

func (r *RSS) markReadRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	return r.markRoute(ctx, raw, true)
}

func (r *RSS) markUnreadRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	return r.markRoute(ctx, raw, false)
}

func (r *RSS) markRoute(ctx context.Context, raw json.RawMessage, read bool) (any, error) {
	var params struct {
		ID string `json:"id"`
	}
	if err := sdk.DecodeParams(raw, &params); err != nil {
		return nil, err
	}
	if params.ID == "" {
		return nil, fmt.Errorf("params.id is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ensureLoadedLocked(ctx); err != nil {
		return nil, err
	}
	if err := r.markHeadlineLocked(ctx, params.ID, read); err != nil {
		return nil, err
	}
	return map[string]any{"id": params.ID, "read": read}, nil
}

// --- helpers ---

// feedNameFallback is what a subscription is called before its feed
// supplies a title: the host it fetches from.
func feedNameFallback(feedURL string) string {
	if u, err := url.Parse(feedURL); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return "(unnamed feed)"
}

// itemSeqOf extracts the sequence number from an item_N id for ordering
// ties; unparseable ids sort as zero.
func itemSeqOf(h *Headline) int {
	var n int
	_, _ = fmt.Sscanf(h.ID, "item_%d", &n)
	return n
}

// namesSummary joins feed titles for status lines and notify bodies,
// bounded so the text stays inside the notification budget.
func namesSummary(names []string) string {
	if len(names) == 0 {
		return ""
	}
	s := strings.Join(names, ", ")
	if len(s) > 160 {
		s = strings.TrimSpace(s[:157]) + "..."
	}
	return s
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
