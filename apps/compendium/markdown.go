package compendium

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxTitleBytes bounds a note title (and therefore its file name).
const MaxTitleBytes = 200

// validTitle enforces titles that are unambiguous inside [[wikilinks]] and
// portable as file names once encoded.
func validTitle(title string) error {
	switch {
	case strings.TrimSpace(title) == "":
		return fmt.Errorf("title is required")
	case strings.TrimSpace(title) != title:
		return fmt.Errorf("title %q has leading or trailing whitespace", title)
	case len(title) > MaxTitleBytes:
		return fmt.Errorf("title exceeds %d bytes", MaxTitleBytes)
	case !utf8.ValidString(title):
		return fmt.Errorf("title is not valid UTF-8")
	case strings.ContainsAny(title, "[]|#/\\"):
		return fmt.Errorf("title %q may not contain [ ] | # / or \\", title)
	}
	for _, r := range title {
		if unicode.IsControl(r) {
			return fmt.Errorf("title may not contain control characters")
		}
	}
	return nil
}

// fold is the case-insensitive identity of a title: [[b]] resolves to "B",
// and two notes may not differ only by case (hosts may be case-insensitive).
// It maps every rune to one representative of its Unicode simple case
// folding orbit, so it agrees with strings.EqualFold: final and medial
// sigma, long s, and the Kelvin sign all fold together, which plain
// strings.ToLower does not do. When the orbit representative drops a mark
// the rune's canonical decomposition carries — the dot in İ, spelled out
// as "İ" — the folded decomposition is emitted instead so the mark is
// not silently lost.
func fold(title string) string {
	ascii := true
	for i := 0; i < len(title); i++ {
		if title[i] >= utf8.RuneSelf {
			ascii = false
			break
		}
	}
	if ascii {
		return strings.ToLower(title)
	}
	var b strings.Builder
	b.Grow(len(title))
	for _, r := range title {
		rep := foldRune(r)
		if d := decompositions[r]; d != "" && decompositions[rep] == "" && hasMark(d) {
			b.WriteString(strings.Map(foldRune, d))
		} else {
			b.WriteRune(rep)
		}
	}
	return b.String()
}

func hasMark(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Mn, r) {
			return true
		}
	}
	return false
}

func foldRune(r rune) rune {
	min := r
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		if f < min {
			min = f
		}
	}
	return unicode.ToLower(min)
}

var windowsDevices = map[string]bool{"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true, "com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true, "lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true}

// fileStem encodes a title as a readable, portable, reversible file stem:
// bytes that are reserved on common hosts become %XX, as do a leading dot,
// trailing dots/spaces, and the first byte of a Windows device name. '%'
// itself is always encoded, so decoding is unambiguous.
func fileStem(title string) string {
	var b strings.Builder
	for i := 0; i < len(title); i++ {
		c := title[i]
		reserved := c < 0x20 || c == 0x7f || strings.IndexByte(`%/\:*?"<>|`, c) >= 0 ||
			(i == 0 && c == '.') ||
			(i == 0 && windowsDevices[strings.ToLower(strings.SplitN(title, ".", 2)[0])]) ||
			((c == '.' || c == ' ') && strings.Trim(title[i:], ". ") == "")
		if reserved {
			fmt.Fprintf(&b, "%%%02X", c)
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// titleFromStem reverses fileStem. Foreign names (imports) decode best-effort.
func titleFromStem(stem string) string {
	var b strings.Builder
	for i := 0; i < len(stem); i++ {
		if stem[i] == '%' && i+2 < len(stem) && isHex(stem[i+1]) && isHex(stem[i+2]) {
			b.WriteByte(unhex(stem[i+1])<<4 | unhex(stem[i+2]))
			i += 2
			continue
		}
		b.WriteByte(stem[i])
	}
	return b.String()
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c <= '9':
		return c - '0'
	case c >= 'a':
		return c - 'a' + 10
	}
	return c - 'A' + 10
}

// span is a wikilink occurrence. Start/End cover "[[...]]"; NameStart and
// NameEnd cover the target title only, so a rename rewrites exactly those
// bytes and leaves aliases, anchors, and everything else untouched.
type span struct {
	Start, End         int
	NameStart, NameEnd int
	Name               string
}

// scanMarkdown walks content once, skipping fenced code blocks and inline
// code spans, and reports each wikilink and each #tag outside code. Rename,
// rendering, indexing, and backlinks all share this scanner, so they always
// agree on what is a link.
//
// Code follows CommonMark: a fence closes only on a run of the same
// character at least as long as the opener with nothing after it, and a
// code span closes only on a backtick run of exactly the opener's length,
// possibly on a later line of the same paragraph.
func scanMarkdown(content string, onLink func(span), onTag func(tag string, at int)) {
	sc := scanner{content: content, onLink: onLink, onTag: onTag, failed: map[int]int{}}
	var fenceCh byte
	fenceN := 0
	lineStart, pos := 0, 0
	for lineStart <= len(content) {
		lineEnd := strings.IndexByte(content[lineStart:], '\n')
		if lineEnd < 0 {
			lineEnd = len(content)
		} else {
			lineEnd += lineStart
		}
		if pos == lineStart {
			ch, n, rest, ok := fenceRun(content[lineStart:lineEnd])
			switch {
			case fenceN > 0:
				if ok && ch == fenceCh && n >= fenceN && strings.TrimSpace(rest) == "" {
					fenceN = 0
				}
				goto next
			case ok:
				fenceCh, fenceN = ch, n
				goto next
			}
		}
		if p := sc.line(pos, lineEnd); p > lineEnd {
			// A code span ran onto a later line: continue right after it.
			lineStart = strings.LastIndexByte(content[:p], '\n') + 1
			pos = p
			continue
		}
	next:
		if lineEnd == len(content) {
			break
		}
		lineStart = lineEnd + 1
		pos = lineStart
	}
}

// fenceRun reports a code fence line: up to three spaces of indent, then
// three or more backticks or tildes. A backtick fence's info string may
// not contain a backtick (that line is inline code instead).
func fenceRun(line string) (ch byte, n int, rest string, ok bool) {
	t := strings.TrimLeft(line, " ")
	if len(line)-len(t) > 3 || len(t) < 3 || (t[0] != '`' && t[0] != '~') {
		return 0, 0, "", false
	}
	ch = t[0]
	for n < len(t) && t[n] == ch {
		n++
	}
	rest = t[n:]
	if n < 3 || (ch == '`' && strings.IndexByte(rest, '`') >= 0) {
		return 0, 0, "", false
	}
	return ch, n, rest, true
}

type scanner struct {
	content string
	onLink  func(span)
	onTag   func(string, int)
	// Caches that keep scanning linear on adversarial input:
	// paraFrom..para is a range of line ends known to share paragraph end
	// para; failed[n] is a paragraph end before which no closing run of n
	// backticks exists; noClose is a line end before which, from
	// noCloseFrom on, there is no "]]"; wStart..wEnd is the word last
	// checked for a URL and wURL its answer.
	paraFrom, para       int
	failed               map[int]int
	noCloseFrom, noClose int
	wStart, wEnd         int
	wURL                 bool
}

// paraEnd returns the end of the paragraph whose line ends at lineEnd: the
// newline before the next blank line or fence line, or the end of content.
func (sc *scanner) paraEnd(lineEnd int) int {
	if sc.para > 0 && sc.paraFrom <= lineEnd && lineEnd <= sc.para {
		return sc.para
	}
	c := sc.content
	p := lineEnd
	for p < len(c) {
		ns := p + 1
		ne := strings.IndexByte(c[ns:], '\n')
		if ne < 0 {
			ne = len(c)
		} else {
			ne += ns
		}
		line := c[ns:ne]
		if _, _, _, fence := fenceRun(line); fence || strings.TrimSpace(line) == "" {
			break
		}
		p = ne
	}
	sc.paraFrom, sc.para = lineEnd, p
	return p
}

// closingRun finds a run of exactly n backticks in content[from:to].
func (sc *scanner) closingRun(from, to, n int) int {
	if sc.failed[n] == to {
		return -1
	}
	c := sc.content
	for i := from; i < to; {
		if c[i] != '`' {
			i++
			continue
		}
		j := i
		for j < to && c[j] == '`' {
			j++
		}
		if j-i == n {
			return i
		}
		i = j
	}
	sc.failed[n] = to
	return -1
}

// urlWordLimit bounds how far back a #tag looks for a URL scheme.
const urlWordLimit = 2048

func wordDelim(c byte) bool {
	return c == ' ' || c == '\t' || c == '<' || c == '(' || c == '"' || c == '\'' || c == '['
}

// inURL reports whether the '#' at i sits inside a URL-like word. A later
// '#' in the same word extends the previous answer instead of rescanning.
func (sc *scanner) inURL(start, i int) bool {
	c := sc.content
	if sc.wEnd > sc.wStart && sc.wStart >= start && sc.wEnd <= i {
		gap := c[sc.wEnd:i]
		delim := false
		for k := 0; k < len(gap) && !delim; k++ {
			delim = wordDelim(gap[k])
		}
		if !delim {
			if !sc.wURL {
				from := sc.wEnd - 2
				if from < sc.wStart {
					from = sc.wStart
				}
				sc.wURL = strings.Contains(c[from:i], "://")
			}
			sc.wEnd = i
			return sc.wURL
		}
	}
	ws := i
	for ws > start && i-ws < urlWordLimit && !wordDelim(c[ws-1]) {
		ws--
	}
	word := strings.ToLower(c[ws:i])
	sc.wStart, sc.wEnd = ws, i
	sc.wURL = strings.Contains(word, "://") || strings.HasPrefix(word, "www.") || strings.HasPrefix(word, "mailto:")
	return sc.wURL
}

// line scans content[start:end] and returns where scanning should resume:
// end, or a position past end when a code span closed on a later line.
func (sc *scanner) line(start, end int) int {
	content := sc.content
	i := start
	for i < end {
		c := content[i]
		switch {
		case c == '`':
			n := 0
			for i+n < end && content[i+n] == '`' {
				n++
			}
			closeAt := sc.closingRun(i+n, sc.paraEnd(end), n)
			if closeAt < 0 {
				i += n // an unclosed run is literal text
				continue
			}
			i = closeAt + n
			if i > end {
				return i
			}
		case c == '\\' && i+1 < end:
			i += 2
		case c == '[' && i+1 < end && content[i+1] == '[':
			closeAt := -1
			if sc.noClose != end || i+2 < sc.noCloseFrom {
				closeAt = strings.Index(content[i+2:end], "]]")
			}
			if closeAt < 0 {
				sc.noCloseFrom, sc.noClose = i+2, end
				i += 2
				continue
			}
			inner := content[i+2 : i+2+closeAt]
			if inner == "" || strings.ContainsAny(inner, "[]") {
				// Retry from the innermost "[[" (its inner text is shorter),
				// so nested or repeated openers cost one pass, not one each.
				if k := strings.LastIndex(inner, "[["); k >= 0 {
					i += 2 + k
				} else {
					i += 2
				}
				continue
			}
			nameEnd := len(inner)
			if k := strings.IndexAny(inner, "|#"); k >= 0 {
				nameEnd = k
				if k > 0 && inner[k] == '|' && inner[k-1] == '\\' {
					nameEnd = k - 1 // [[Title\|alias]]: the pipe escaped for a table cell
				}
			}
			// The name and its offsets come from one trim with one
			// predicate (unicode.IsSpace, as strings.TrimSpace uses), so a
			// leading NBSP or ideographic space can never make NameStart
			// land inside a multi-byte rune.
			raw := inner[:nameEnd]
			trimmedLeft := strings.TrimLeftFunc(raw, unicode.IsSpace)
			lead := len(raw) - len(trimmedLeft)
			name := strings.TrimRightFunc(trimmedLeft, unicode.IsSpace)
			// A name carrying a backtick is not a link: it would hold the
			// line open as a fence info string only as long as the name is
			// there, so renaming it away could drop the link into a fence.
			if name != "" && !strings.ContainsRune(name, '`') && sc.onLink != nil {
				sc.onLink(span{Start: i, End: i + 2 + closeAt + 2,
					NameStart: i + 2 + lead, NameEnd: i + 2 + lead + len(name), Name: name})
			}
			i += 2 + closeAt + 2
		case c == '#':
			if i > start {
				prev, _ := utf8.DecodeLastRuneInString(content[start:i])
				if unicode.IsLetter(prev) || unicode.IsDigit(prev) || prev == '_' || prev == '#' || prev == '/' || prev == '&' ||
					(prev == '(' && i-2 >= start && content[i-2] == ']') || // [text](#anchor)
					sc.inURL(start, i) {
					i++
					continue
				}
			}
			j := i + 1
			letter := false
			for j < end {
				r, size := utf8.DecodeRuneInString(content[j:end])
				if unicode.IsLetter(r) {
					letter = true
				} else if !unicode.IsDigit(r) && r != '_' && r != '-' && r != '/' {
					break
				}
				j += size
			}
			tag := strings.TrimRight(content[i+1:j], "/-")
			if letter && tag != "" && sc.onTag != nil {
				sc.onTag(strings.ToLower(tag), i)
			}
			if j == i+1 {
				j++
			}
			i = j
		default:
			i++
		}
	}
	return end
}

// parsed is what the index keeps about one note.
type parsed struct {
	Links []string // distinct link targets, first-seen spelling, in order
	Tags  []string // distinct lowercase tags, in order
}

func parseNote(content string) parsed {
	var p parsed
	seenL, seenT := map[string]bool{}, map[string]bool{}
	scanMarkdown(content, func(s span) {
		if k := fold(s.Name); !seenL[k] {
			seenL[k] = true
			p.Links = append(p.Links, s.Name)
		}
	}, func(tag string, _ int) {
		if !seenT[tag] {
			seenT[tag] = true
			p.Tags = append(p.Tags, tag)
		}
	})
	return p
}

// rewriteLinks replaces the target of every link to oldTitle (case
// insensitive) with newTitle, preserving aliases and anchors byte for byte.
func rewriteLinks(content, oldTitle, newTitle string) (string, int) {
	var spans []span
	scanMarkdown(content, func(s span) {
		if fold(s.Name) == fold(oldTitle) {
			spans = append(spans, s)
		}
	}, nil)
	if len(spans) == 0 {
		return content, 0
	}
	var b strings.Builder
	last := 0
	for _, s := range spans {
		b.WriteString(content[last:s.NameStart])
		b.WriteString(newTitle)
		last = s.NameEnd
	}
	b.WriteString(content[last:])
	return b.String(), len(spans)
}

// linksTo reports whether content links to title.
func linksTo(content, title string) bool {
	found := false
	scanMarkdown(content, func(s span) { found = found || fold(s.Name) == fold(title) }, nil)
	return found
}

// render resolves wikilinks for display as plain text: resolved links
// become «Title» (or «alias → Title»); broken ones are flagged as missing.
// No styling, markup, or terminal control is emitted; the shell owns that.
func render(content string, resolve func(name string) (string, bool)) string {
	var b strings.Builder
	last := 0
	scanMarkdown(content, func(s span) {
		b.WriteString(content[last:s.Start])
		inner := content[s.Start+2 : s.End-2]
		alias := ""
		if k := strings.IndexByte(inner, '|'); k >= 0 {
			alias = strings.TrimSpace(inner[k+1:])
		}
		if title, ok := resolve(s.Name); ok {
			if alias != "" {
				fmt.Fprintf(&b, "«%s → %s»", alias, title)
			} else {
				fmt.Fprintf(&b, "«%s»", title)
			}
		} else {
			fmt.Fprintf(&b, "«%s (missing)»", s.Name)
		}
		last = s.End
	}, nil)
	b.WriteString(content[last:])
	return b.String()
}

// contextLine returns the trimmed line of content containing the first link
// to title, for the backlinks panel.
func contextLine(content, title string) string {
	at := -1
	scanMarkdown(content, func(s span) {
		if at < 0 && fold(s.Name) == fold(title) {
			at = s.Start
		}
	}, nil)
	if at < 0 {
		return ""
	}
	start := strings.LastIndexByte(content[:at], '\n') + 1
	end := strings.IndexByte(content[at:], '\n')
	if end < 0 {
		end = len(content)
	} else {
		end += at
	}
	return clip(strings.TrimSpace(strings.TrimSuffix(content[start:end], "\r")), 240)
}

// clip truncates s to at most n bytes on a rune boundary, marking the cut.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// normKey is the identity under which two titles would collide on a
// normalization-insensitive host (macOS APFS treats "Caf\u00e9" and
// "Cafe\u0301" as one file name). The standard library has no Unicode
// normalization, so this is a deliberately conservative fold used only to
// detect collisions, never to rewrite a title:
//
//  1. case-fold (fold), so it also covers case-insensitive hosts;
//  2. decompose precomposed letters with the generated canonical table in
//     normtable.go (Latin, Greek, Cyrillic, letterlike singletons) and
//     Hangul syllables algorithmically;
//  3. case-fold again (a decomposed base may be upper case; fold even maps
//     some bases, such as Greek iota, to a combining mark);
//  4. sort each run of combining marks by code point (a superset of
//     canonical reordering: it may call two titles equal that a real NFD
//     would keep apart, which only ever rejects a title, never merges data).
//
// Precomposed characters outside the table are compared as they are, so a
// collision among them is still caught physically: new note and draft
// files are written with overwrite=false and the host refuses a name it
// already resolves.
func normKey(title string) string {
	s := fold(title)
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			ascii = false
			break
		}
	}
	if ascii {
		return s
	}
	// Folding a decomposed string can (rarely) yield a precomposed letter
	// again, so decompose and fold until neither changes anything.
	for i := 0; i < 4; i++ {
		next := fold(string(decompose(s)))
		if next == s {
			break
		}
		s = next
	}
	out := []rune(s)
	for i := 0; i < len(out); {
		if !unicode.Is(unicode.Mn, out[i]) {
			i++
			continue
		}
		j := i
		for j < len(out) && unicode.Is(unicode.Mn, out[j]) {
			j++
		}
		sort.Slice(out[i:j], func(a, b int) bool { return out[i+a] < out[i+b] })
		i = j
	}
	return string(out)
}

// decompose applies the canonical decomposition table and the Hangul
// syllable algorithm.
func decompose(s string) []rune {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= hangulBase && r < hangulBase+hangulCount:
			i := r - hangulBase
			out = append(out, 0x1100+i/(21*28), 0x1161+(i%(21*28))/28)
			if t := i % 28; t != 0 {
				out = append(out, 0x11A7+t)
			}
		case decompositions[r] != "":
			out = append(out, []rune(decompositions[r])...)
		default:
			out = append(out, r)
		}
	}
	return out
}

const hangulBase, hangulCount = 0xAC00, 11172
