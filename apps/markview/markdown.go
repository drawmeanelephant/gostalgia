package markview

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// rowKind identifies the rendered block element a view row carries.
type rowKind int

const (
	rowPara rowKind = iota
	rowHeading
	rowList
	rowTask
	rowQuote
	rowCode
	rowRule
	rowTable
	rowDef
)

// row is one rendered view row. Label carries the display text with inline
// markup already flattened to plain text; detail describes the structure
// and the source line the row came from.
type row struct {
	kind  rowKind
	line  int    // 1-based source line the row starts on
	label string // presentation text
	aux   string // fence language for code rows
	level int    // heading level or list/quote depth, else 0
}

// detail annotates a row for the item detail slot: structure word plus the
// source line, so the reader can locate any row in the original document.
func (r row) detail() string {
	switch r.kind {
	case rowHeading:
		return fmt.Sprintf("h%d · line %d", r.level, r.line)
	case rowList:
		return fmt.Sprintf("list · line %d", r.line)
	case rowTask:
		return fmt.Sprintf("task · line %d", r.line)
	case rowQuote:
		return fmt.Sprintf("quote · line %d", r.line)
	case rowCode:
		if r.aux != "" {
			return fmt.Sprintf("code · %s · line %d", r.aux, r.line)
		}
		return fmt.Sprintf("code · line %d", r.line)
	case rowRule:
		return fmt.Sprintf("rule · line %d", r.line)
	case rowTable:
		return fmt.Sprintf("table · line %d", r.line)
	case rowDef:
		return fmt.Sprintf("linkdef · line %d", r.line)
	default:
		return fmt.Sprintf("line %d", r.line)
	}
}

// renderer accumulates bounded rows from a line-oriented Markdown walk.
type renderer struct {
	limit  int
	rows   []row
	capped bool
}

func (r *renderer) emit(k rowKind, line int, label, aux string, level int) {
	if r.capped {
		return
	}
	if len(r.rows) >= r.limit {
		r.capped = true
		return
	}
	label = strings.TrimRight(label, " ")
	r.rows = append(r.rows, row{kind: k, line: line, label: clipRunes(label, maxLabelBytes), aux: aux, level: level})
}

// emitBlock flushes a paragraph or setext heading: lines join with spaces,
// inline markup flattens, and the text wraps greedily so a paragraph reads
// as several rows rather than one clipped line. level 0 is a paragraph;
// 1 or 2 is the corresponding setext heading.
func (r *renderer) emitBlock(lines []string, start, level int) {
	trimmed := make([]string, len(lines))
	for i, l := range lines {
		trimmed[i] = strings.TrimSpace(l)
	}
	text := flattenInline(strings.Join(trimmed, " "))
	kind, kLevel, prefix := rowPara, 0, ""
	if level > 0 {
		kind, kLevel = rowHeading, level
		prefix = strings.Repeat("#", level) + " "
	}
	chunks := wrapText(text, paraWrapRunes)
	for i, c := range chunks {
		label := c
		if i == 0 {
			label = prefix + c
		} else {
			label = "  " + c
		}
		r.emit(kind, start, label, "", kLevel)
		if r.capped {
			return
		}
	}
}

// renderMarkdown parses src into bounded view rows. It is line-oriented
// CommonMark-ish by design: ATX and setext headings, thematic breaks,
// block quotes, bulleted/numbered/task lists, fenced and indented code,
// pipe tables, link reference definitions, and paragraphs. Container
// blocks continue lazily: a paragraph line under a quote or list row keeps
// that row's marker until a blank line or a new block start, and a
// blank-separated line indented to a list item's content column stays
// inside the item. Inline markup flattens to plain text — emphasis
// markers drop away, links show their target, code spans show their
// contents — because the presentation contract carries structure, never
// raw markup.
func renderMarkdown(src string, limit int) (rows []row, capped bool) {
	src = strings.ReplaceAll(src, "\r\n", "\n")
	src = strings.ReplaceAll(src, "\r", "\n")
	lines := strings.Split(src, "\n")
	if n := len(lines); n > 0 && strings.TrimSpace(lines[n-1]) == "" {
		lines = lines[:n-1]
	}

	r := &renderer{limit: limit}
	paraFrom := -1    // 1-based source line where the pending paragraph began; -1 = none
	prevBlank := true // previous source line was blank or document start; gates indented code
	quoteDepth := 0   // nesting depth of the open quote's lazy paragraph; 0 = no quote open
	itemIndent := -1  // content column of the open list item; -1 = no item open
	itemDepth := 0    // display depth of the open list item
	for i := 0; i < len(lines) && !r.capped; {
		line := lines[i]
		ln := i + 1
		trimmed := strings.TrimSpace(line)

		if paraFrom >= 0 {
			// A paragraph is pending on lines [paraFrom-1, i): blank line
			// ends it, a setext underline re-levels it, a new block start
			// flushes it, anything else continues it lazily.
			switch {
			case trimmed == "":
				r.emitBlock(lines[paraFrom-1:i], paraFrom, 0)
				paraFrom, prevBlank = -1, true
				i++
			case isSetext(line):
				r.emitBlock(lines[paraFrom-1:i], paraFrom, setextLevel(line))
				paraFrom, prevBlank = -1, false
				i++
			case startsBlock(lines, i):
				r.emitBlock(lines[paraFrom-1:i], paraFrom, 0)
				paraFrom = -1
			default:
				i++
			}
			continue
		}

		switch {
		case trimmed == "":
			prevBlank = true
			quoteDepth = 0
			i++
		case isFenceStart(line):
			ch, n, info, _ := fenceStart(line)
			ind := leadingIndent(line)
			i++
			start := ln
			for i < len(lines) {
				if fenceEnd(lines[i], ch, n) {
					break
				}
				// An indented opening fence dedents its body by up to the
				// same number of columns.
				body := stripIndent(lines[i], ind)
				if strings.TrimSpace(body) == "" {
					body = " "
				}
				r.emit(rowCode, i+1, "┃ "+body, info, 0)
				i++
			}
			if i == start {
				r.emit(rowCode, ln, "┃ (empty code block)", info, 0)
			}
			if i < len(lines) {
				i++ // consume the closing fence
			}
			prevBlank, quoteDepth, itemIndent = false, 0, -1
		case isATXHeading(line):
			level, text, _ := atxHeading(line)
			r.emit(rowHeading, ln, strings.Repeat("#", level)+" "+flattenInline(text), "", level)
			i++
			prevBlank, quoteDepth, itemIndent = false, 0, -1
		case isRule(line):
			r.emit(rowRule, ln, "───", "", 0)
			i++
			prevBlank, quoteDepth, itemIndent = false, 0, -1
		case isQuote(line):
			depth, text := quoteParts(line)
			r.emit(rowQuote, ln, strings.Repeat("│ ", depth)+flattenInline(text), "", depth)
			i++
			prevBlank, quoteDepth, itemIndent = false, depth, -1
		case isTableStart(lines, i):
			i = emitTable(r, lines, i)
			prevBlank, quoteDepth, itemIndent = false, 0, -1
		case isListItem(line):
			depth, content, label, task := listItem(line)
			kind := rowList
			if task {
				kind = rowTask
			}
			r.emit(kind, ln, label, "", depth)
			i++
			prevBlank, quoteDepth, itemIndent, itemDepth = false, 0, content, depth
		case isLinkDef(line):
			id, rest, _ := linkDef(line)
			r.emit(rowDef, ln, "↳ ["+id+"]: "+rest, "", 0)
			i++
			prevBlank, quoteDepth, itemIndent = false, 0, -1
		case quoteDepth > 0 && !prevBlank:
			// Lazy quote continuation: the paragraph inside the quote keeps
			// running on lines without the `>` marker until a blank line or
			// a new block start.
			r.emit(rowQuote, ln, strings.Repeat("│ ", quoteDepth)+flattenInline(trimmed), "", quoteDepth)
			i++
		case itemIndent >= 0 && !prevBlank:
			// Lazy item continuation: a paragraph line under an item stays
			// inside it without repeating the marker.
			r.emit(rowList, ln, strings.Repeat("  ", itemDepth+1)+flattenInline(trimmed), "", itemDepth)
			i++
		case itemIndent >= 0 && leadingIndent(line) >= itemIndent:
			// Blank-separated content indented to the item's content column
			// is a new block inside the item; four columns deeper is an
			// indented code block.
			if leadingIndent(line) >= itemIndent+4 {
				for i < len(lines) && leadingIndent(lines[i]) >= itemIndent+4 && strings.TrimSpace(lines[i]) != "" {
					r.emit(rowCode, i+1, strings.Repeat("  ", itemDepth)+"┃ "+stripIndent(lines[i], itemIndent+4), "", 0)
					i++
				}
			} else {
				r.emit(rowList, ln, strings.Repeat("  ", itemDepth+1)+flattenInline(trimmed), "", itemDepth)
				i++
			}
			prevBlank = false
		case leadingIndent(line) >= 4 && prevBlank:
			// Indented code block: a run of >=4-column lines after a blank
			// line, with the four-column margin stripped.
			for i < len(lines) {
				l := lines[i]
				if strings.TrimSpace(l) == "" {
					break
				}
				if leadingIndent(l) < 4 {
					break
				}
				body := stripIndent(l, 4)
				r.emit(rowCode, i+1, "┃ "+body, "", 0)
				i++
			}
			prevBlank, quoteDepth, itemIndent = false, 0, -1
		default:
			paraFrom = ln
			itemIndent = -1
			i++
		}
	}
	if paraFrom >= 0 && !r.capped {
		r.emitBlock(lines[paraFrom-1:], paraFrom, 0)
	}
	return r.rows, r.capped
}

// startsBlock reports whether lines[i] begins a block element other than a
// paragraph or a setext underline — the constructs that interrupt a pending
// paragraph. Indented code never interrupts one.
func startsBlock(lines []string, i int) bool {
	line := lines[i]
	return isFenceStart(line) || isATXHeading(line) || isRule(line) ||
		isQuote(line) || isTableStart(lines, i) || isListItem(line) || isLinkDef(line)
}

// leadingIndent measures the indentation of line in columns: a space is one
// column, a tab advances to the next multiple of four.
func leadingIndent(line string) int {
	col := 0
	for _, c := range line {
		switch c {
		case ' ':
			col++
		case '\t':
			col = (col/4 + 1) * 4
		default:
			return col
		}
	}
	return col
}

// stripIndent removes up to n columns of leading indentation.
func stripIndent(line string, n int) string {
	col, i := 0, 0
	for i < len(line) && col < n {
		switch line[i] {
		case ' ':
			col++
			i++
		case '\t':
			next := (col/4 + 1) * 4
			if next > n {
				return line[i:]
			}
			col = next
			i++
		default:
			return line[i:]
		}
	}
	return line[i:]
}

// isFenceStart reports whether line opens a fenced code block: up to three
// leading spaces, then a run of three or more backticks or tildes.
func isFenceStart(line string) bool {
	_, _, _, ok := fenceStart(line)
	return ok
}

func fenceStart(line string) (ch byte, n int, info string, ok bool) {
	s := strings.TrimLeft(line, " ")
	if len(line)-len(s) > 3 || len(s) < 3 {
		return 0, 0, "", false
	}
	c := s[0]
	if c != '`' && c != '~' {
		return 0, 0, "", false
	}
	for n < len(s) && s[n] == c {
		n++
	}
	info = strings.TrimSpace(s[n:])
	if n < 3 || (c == '`' && strings.IndexByte(info, '`') >= 0) {
		return 0, 0, "", false
	}
	return c, n, info, true
}

// fenceEnd reports whether line closes a fence opened with a run of n
// characters ch: same character, at least as many, nothing after.
func fenceEnd(line string, ch byte, n int) bool {
	s := strings.TrimLeft(line, " ")
	if len(line)-len(s) > 3 {
		return false
	}
	m := 0
	for m < len(s) && s[m] == ch {
		m++
	}
	return m >= n && strings.TrimSpace(s[m:]) == ""
}

// isATXHeading reports whether line is an ATX heading (`#` through `######`
// followed by whitespace or end of line).
func isATXHeading(line string) bool {
	_, _, ok := atxHeading(line)
	return ok
}

func atxHeading(line string) (level int, text string, ok bool) {
	s := strings.TrimLeft(line, " ")
	if len(line)-len(s) > 3 {
		return 0, "", false
	}
	for level < len(s) && s[level] == '#' {
		level++
	}
	if level == 0 || level > 6 {
		return 0, "", false
	}
	if level < len(s) && s[level] != ' ' && s[level] != '\t' {
		return 0, "", false // `#5 bolt` is a paragraph, not a heading
	}
	content := strings.TrimSpace(s[level:])
	// An optional closing run of #'s is stripped when separated by a space.
	if j := strings.LastIndex(content, " #"); j >= 0 && strings.Trim(content[j+1:], "# ") == "" {
		content = strings.TrimSpace(content[:j])
	} else if strings.Trim(content, "# ") == "" {
		content = ""
	}
	return level, content, true
}

// isSetext reports whether line is a setext heading underline: a run of `=`
// (level 1) or `-` (level 2) with up to three leading spaces. It only acts
// on a pending paragraph; alone, the same line is paragraph text or a rule.
func isSetext(line string) bool { return setextLevel(line) > 0 }

func setextLevel(line string) int {
	s := strings.TrimSpace(line)
	if s == "" {
		return 0
	}
	c := s[0]
	if c != '=' && c != '-' {
		return 0
	}
	if strings.Trim(s, string(c)) != "" {
		return 0
	}
	if leadingIndent(line) > 3 {
		return 0
	}
	if c == '=' {
		return 1
	}
	return 2
}

// isRule reports whether line is a thematic break: three or more `-`, `*`,
// or `_`, possibly interleaved with spaces or tabs.
func isRule(line string) bool {
	var b strings.Builder
	for _, c := range line {
		if c != ' ' && c != '\t' {
			b.WriteRune(c)
		}
	}
	s := b.String()
	if len(s) < 3 {
		return false
	}
	c := s[0]
	if c != '-' && c != '*' && c != '_' {
		return false
	}
	return strings.Trim(s, string(c)) == "" && leadingIndent(line) <= 3
}

// isQuote reports whether line opens a block quote.
func isQuote(line string) bool {
	s := strings.TrimLeft(line, " ")
	return len(line)-len(s) <= 3 && strings.HasPrefix(s, ">")
}

// quoteParts strips the `>` markers from a quote line and returns the
// nesting depth and remaining text.
func quoteParts(line string) (depth int, text string) {
	s := strings.TrimLeft(line, " ")
	for len(s) > 0 && s[0] == '>' {
		depth++
		s = s[1:]
		s = strings.TrimPrefix(s, " ")
	}
	return depth, s
}

// isListItem reports whether line is a list item marker: a bullet (`-`,
// `+`, `*`) or an ordered marker (`1.`, `2)`) followed by whitespace.
func isListItem(line string) bool {
	_, _, _, _, _, ok := listItemParts(line)
	return ok
}

// listItem decomposes a list item line into indent depth, content column,
// and label text, detecting task markers (`[ ]`, `[x]`) on bullet items.
func listItem(line string) (depth, content int, label string, task bool) {
	depth, content, marker, body, t, _ := listItemParts(line)
	indent := strings.Repeat("  ", depth)
	body = strings.TrimLeft(body, " \t")
	if t {
		mark := "[ ]"
		if marker == "x" || marker == "X" {
			mark = "[x]"
		}
		return depth, content, indent + mark + " " + flattenInline(body), true
	}
	return depth, content, indent + marker + " " + flattenInline(body), false
}

// listItemParts splits a list item line into indent depth, content column
// (the indentation nested content must reach to stay inside the item),
// marker text ("•" or the ordered digits), body, and task-list flag
// without rendering.
func listItemParts(line string) (depth, content int, marker, body string, task, ok bool) {
	if strings.TrimSpace(line) == "" {
		return 0, 0, "", "", false, false
	}
	indent := leadingIndent(line)
	depth = indent / 2
	s := strings.TrimLeft(line, " \t")
	if len(s) >= 2 && (s[0] == '-' || s[0] == '+' || s[0] == '*') && (s[1] == ' ' || s[1] == '\t') {
		rest := strings.TrimLeft(s[2:], " \t")
		if len(rest) >= 3 && rest[0] == '[' && rest[2] == ']' &&
			(rest[1] == ' ' || rest[1] == 'x' || rest[1] == 'X') &&
			(len(rest) == 3 || rest[3] == ' ' || rest[3] == '\t') {
			return depth, indent + 2, string(rest[1]), rest[3:], true, true
		}
		return depth, indent + 2, "•", rest, false, true
	}
	k := 0
	for k < len(s) && k <= 9 && s[k] >= '0' && s[k] <= '9' {
		k++
	}
	if k >= 1 && k+1 < len(s) && (s[k] == '.' || s[k] == ')') && (s[k+1] == ' ' || s[k+1] == '\t') {
		return depth, indent + k + 2, s[:k+1], s[k+2:], false, true
	}
	return 0, 0, "", "", false, false
}

// isTableStart reports whether lines[i] is a pipe-table header: it contains
// a pipe and lines[i+1] is a delimiter row with the same cell count.
func isTableStart(lines []string, i int) bool {
	if i+1 >= len(lines) || !strings.Contains(lines[i], "|") || strings.TrimSpace(lines[i]) == "" {
		return false
	}
	aligns, ok := tableDelims(lines[i+1])
	return ok && len(aligns) == len(splitCells(lines[i]))
}

// tableAlign is a column's alignment per its delimiter cell.
type tableAlign int

const (
	alignNone   tableAlign = iota // `---`
	alignLeft                     // `:--`
	alignCenter                   // `:-:`
	alignRight                    // `--:`
)

// tableDelims parses a pipe-table delimiter row into per-column alignment,
// reporting false when the line is not a delimiter row: every cell must be
// a run of `-` with optional `:` alignment edges.
func tableDelims(line string) ([]tableAlign, bool) {
	s := strings.TrimSpace(line)
	if s == "" || !strings.Contains(s, "-") {
		return nil, false
	}
	cells := splitCells(s)
	aligns := make([]tableAlign, len(cells))
	for i, c := range cells {
		c = strings.TrimSpace(c)
		if c == "" || strings.Trim(c, ":-") != "" || !strings.Contains(c, "-") {
			return nil, false
		}
		left, right := strings.HasPrefix(c, ":"), strings.HasSuffix(c, ":")
		switch {
		case left && right:
			aligns[i] = alignCenter
		case right:
			aligns[i] = alignRight
		case left:
			aligns[i] = alignLeft
		}
	}
	return aligns, true
}

// splitCells breaks a pipe-table line into raw cells, dropping unescaped
// framing pipes at either end. A backslash-escaped `\|` stays inside its
// cell and decodes during inline flattening.
func splitCells(line string) []string {
	s := strings.TrimSpace(line)
	s = strings.TrimPrefix(s, "|")
	if n := len(s); n > 0 && s[n-1] == '|' && !escapedPipe(s, n-1) {
		s = s[:n-1]
	}
	var cells []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '|' && !escapedPipe(s, i) {
			cells = append(cells, s[start:i])
			start = i + 1
		}
	}
	return append(cells, s[start:])
}

// escapedPipe reports whether the `|` at s[i] is backslash-escaped —
// preceded by an odd-length run of backslashes.
func escapedPipe(s string, i int) bool {
	bs := 0
	for j := i - 1; j >= 0 && s[j] == '\\'; j-- {
		bs++
	}
	return bs%2 == 1
}

// emitTable renders the pipe table whose header sits at lines[i] and
// returns the first source index past it. Columns are padded to the widest
// cell (bounded by maxTableColRunes) and aligned per the delimiter row;
// the delimiter itself renders as a separator row so every source line
// still maps to a view row.
func emitTable(r *renderer, lines []string, i int) int {
	header := tableCells(lines[i])
	aligns, _ := tableDelims(lines[i+1])
	var body [][]string
	j := i + 2
	for j < len(lines) && strings.Contains(lines[j], "|") && strings.TrimSpace(lines[j]) != "" {
		body = append(body, tableCells(lines[j]))
		j++
	}
	widths := make([]int, len(header))
	measure := func(cells []string) {
		for c := range widths {
			w := 0
			if c < len(cells) {
				w = utf8.RuneCountInString(cells[c])
			}
			if w > widths[c] {
				widths[c] = w
			}
		}
	}
	measure(header)
	for _, b := range body {
		measure(b)
	}
	for c := range widths {
		if widths[c] > maxTableColRunes {
			widths[c] = maxTableColRunes
		}
	}
	r.emit(rowTable, i+1, tableRow(header, widths, aligns), "", 0)
	r.emit(rowTable, i+2, tableSep(widths, aligns), "", 0)
	for k, cells := range body {
		r.emit(rowTable, i+3+k, tableRow(cells, widths, aligns), "", 0)
		if r.capped {
			break
		}
	}
	return j
}

// tableCells flattens a pipe-table source line into cell text.
func tableCells(line string) []string {
	cells := splitCells(line)
	for i := range cells {
		cells[i] = flattenInline(strings.TrimSpace(cells[i]))
	}
	return cells
}

// tableRow renders one pipe-table row: cells padded to column width and
// aligned per the delimiter spec, joined by a table pipe. Missing cells
// pad empty and excess cells drop, matching GFM row normalization.
func tableRow(cells []string, widths []int, aligns []tableAlign) string {
	parts := make([]string, len(widths))
	for c := range parts {
		cell := ""
		if c < len(cells) {
			cell = cells[c]
		}
		parts[c] = alignCell(cell, widths[c], colAlign(aligns, c))
	}
	return strings.Join(parts, " │ ")
}

// tableSep renders the delimiter row as a visible separator: column-width
// runs of ─ joined by ┼, with a : edge marking non-default alignment.
func tableSep(widths []int, aligns []tableAlign) string {
	parts := make([]string, len(widths))
	for c, w := range widths {
		if w < 1 {
			w = 1
		}
		seg := strings.Repeat("─", w)
		switch colAlign(aligns, c) {
		case alignLeft:
			seg = ":" + strings.Repeat("─", w-1)
		case alignRight:
			seg = strings.Repeat("─", w-1) + ":"
		case alignCenter:
			if w == 1 {
				seg = ":"
			} else {
				seg = ":" + strings.Repeat("─", w-2) + ":"
			}
		}
		parts[c] = seg
	}
	return strings.Join(parts, "─┼─")
}

func colAlign(aligns []tableAlign, c int) tableAlign {
	if c < len(aligns) {
		return aligns[c]
	}
	return alignNone
}

// alignCell pads cell to width on the side its alignment demands; a cell
// already wider than the column passes through untouched.
func alignCell(cell string, width int, a tableAlign) string {
	gap := width - utf8.RuneCountInString(cell)
	if gap <= 0 {
		return cell
	}
	switch a {
	case alignRight:
		return strings.Repeat(" ", gap) + cell
	case alignCenter:
		left := gap / 2
		return strings.Repeat(" ", left) + cell + strings.Repeat(" ", gap-left)
	default:
		return cell + strings.Repeat(" ", gap)
	}
}

// isLinkDef reports whether line is a link reference definition:
// `[label]: destination`, with up to three leading columns.
func isLinkDef(line string) bool {
	_, _, ok := linkDef(line)
	return ok
}

// linkDef parses `[label]: destination "title"` into the label and the
// remainder (destination plus optional title). A missing destination or a
// `]` immediately after `[` is not a definition.
func linkDef(line string) (id, rest string, ok bool) {
	s := strings.TrimLeft(line, " ")
	if len(line)-len(s) > 3 || !strings.HasPrefix(s, "[") {
		return "", "", false
	}
	close := strings.IndexByte(s, ']')
	if close <= 1 || close+1 >= len(s) || s[close+1] != ':' {
		return "", "", false
	}
	rest = strings.TrimSpace(s[close+2:])
	if rest == "" {
		return "", "", false
	}
	return s[1:close], rest, true
}

// runLen counts the run of c at the start of s.
func runLen(s string, c byte) int {
	n := 0
	for n < len(s) && s[n] == c {
		n++
	}
	return n
}

// findRun returns the offset in s of a run of exactly n characters c, or -1.
func findRun(s string, c byte, n int) int {
	for i := 0; i < len(s); {
		if s[i] != c {
			i++
			continue
		}
		j := i
		for j < len(s) && s[j] == c {
			j++
		}
		if j-i == n {
			return i
		}
		i = j
	}
	return -1
}

// findEmphClose returns the offset in s of a run of c at least n long that
// can close emphasis. For `_` and `~` the closing run must sit on a word
// boundary (followed by whitespace, punctuation, or end of string), so
// intra-word underscores and tildes stay literal.
func findEmphClose(s string, from int, c byte, n int) int {
	for i := from; i < len(s); {
		if s[i] != c {
			i++
			continue
		}
		j := i
		for j < len(s) && s[j] == c {
			j++
		}
		if j-i >= n && (c == '*' || j == len(s) || !isWordByte(s[j])) {
			return i
		}
		i = j
	}
	return -1
}

// canOpenEmph reports whether a run of c at s[i] may open emphasis. `*` is
// unrestricted; `_` and `~` must start on a word boundary so snake_case and
// mid-word tildes stay literal.
func canOpenEmph(s string, i int, c byte) bool {
	if c == '*' {
		return true
	}
	return i == 0 || !isWordByte(s[i-1])
}

func isWordByte(c byte) bool {
	return c == '_' || c == '~' || ('0' <= c && c <= '9') || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}

func isASCIIPunct(c byte) bool {
	return strings.IndexByte("!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~", c) >= 0
}

// parseInlineLink parses `[text](target)` starting at s[i] == '['. It
// returns the link text, the target (an optional quoted title discarded),
// and the index just past the closing paren.
func parseInlineLink(s string, i int) (text, target string, next int, ok bool) {
	j := strings.IndexByte(s[i:], ']')
	if j < 0 {
		return "", "", 0, false
	}
	close := i + j
	inner := s[i+1 : close]
	if close+1 >= len(s) || s[close+1] != '(' {
		return "", "", 0, false
	}
	depth, k, end := 0, close+1, -1
	for k < len(s) {
		if s[k] == '(' {
			depth++
		}
		if s[k] == ')' {
			depth--
			if depth == 0 {
				end = k
				break
			}
		}
		k++
	}
	if end < 0 {
		return "", "", 0, false
	}
	target = strings.TrimSpace(s[close+2 : end])
	if sp := strings.IndexAny(target, " \t"); sp >= 0 {
		rest := strings.TrimSpace(target[sp:])
		if len(rest) >= 2 && (rest[0] == '"' || rest[0] == '\'' || rest[0] == '(') {
			target = target[:sp]
		}
	}
	return inner, target, end + 1, true
}

// looksAutolink reports whether an <...> run is an autolink (a URL scheme
// or an address) rather than a raw HTML tag.
func looksAutolink(inner string) bool {
	if strings.ContainsAny(inner, " \t\n") {
		return false
	}
	return strings.Contains(inner, "://") || strings.HasPrefix(inner, "mailto:") ||
		strings.HasPrefix(inner, "www.") || strings.Contains(inner, "@")
}

// decodeEntity decodes the HTML entity at the start of s (which begins with
// '&'), returning the decoded text and bytes consumed. Only the common
// named entities plus numeric character references are known.
func decodeEntity(s string) (string, int) {
	if len(s) < 3 || s[0] != '&' {
		return "", 0
	}
	semi := strings.IndexByte(s, ';')
	if semi < 0 || semi > 32 {
		return "", 0
	}
	ent := s[1:semi]
	switch ent {
	case "amp":
		return "&", semi + 1
	case "lt":
		return "<", semi + 1
	case "gt":
		return ">", semi + 1
	case "quot":
		return `"`, semi + 1
	case "apos":
		return "'", semi + 1
	case "nbsp":
		return "\u00a0", semi + 1
	}
	var n uint64
	var err error
	if strings.HasPrefix(ent, "#x") || strings.HasPrefix(ent, "#X") {
		n, err = strconv.ParseUint(ent[2:], 16, 32)
	} else if strings.HasPrefix(ent, "#") {
		n, err = strconv.ParseUint(ent[1:], 10, 32)
	} else {
		return "", 0
	}
	if err != nil || n == 0 || n > utf8.MaxRune || (n >= 0xD800 && n <= 0xDFFF) {
		return "", 0
	}
	return string(rune(n)), semi + 1
}

// flattenInline reduces inline Markdown to honest plain text: emphasis and
// strikethrough markers drop away, code spans show their contents, links
// show text plus target in parentheses, images collapse to an [image] note,
// autolinks stay readable, raw HTML tags are removed rather than passed
// through, entities decode, and backslash escapes reveal the literal.
func flattenInline(s string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s) && isASCIIPunct(s[i+1]):
			b.WriteByte(s[i+1])
			i += 2
		case c == '`':
			n := runLen(s[i:], '`')
			end := findRun(s[i+n:], '`', n)
			if end < 0 {
				b.WriteString(s[i : i+n])
				i += n
				continue
			}
			inner := strings.ReplaceAll(s[i+n:i+n+end], "\n", " ")
			if len(inner) > 2 && inner[0] == ' ' && inner[len(inner)-1] == ' ' && strings.TrimSpace(inner) != "" {
				inner = inner[1 : len(inner)-1]
			}
			b.WriteString(inner)
			i += n + end + n
		case c == '!' && i+1 < len(s) && s[i+1] == '[':
			inner, _, ni, ok := parseInlineLink(s, i+1)
			if !ok {
				b.WriteByte(c)
				i++
				continue
			}
			if inner != "" {
				b.WriteString("[image: " + flattenInline(inner) + "]")
			} else {
				b.WriteString("[image]")
			}
			i = ni
		case c == '[':
			inner, target, ni, ok := parseInlineLink(s, i)
			if !ok {
				b.WriteByte(c)
				i++
				continue
			}
			b.WriteString(flattenInline(inner))
			if target != "" {
				b.WriteString(" (" + target + ")")
			}
			i = ni
		case c == '<':
			end := strings.IndexByte(s[i:], '>')
			if end < 0 {
				b.WriteByte(c)
				i++
				continue
			}
			inner := s[i+1 : i+end]
			if inner == "" {
				b.WriteByte(c)
				i++
				continue
			}
			first, _ := utf8.DecodeRuneInString(inner)
			if looksAutolink(inner) {
				b.WriteString(inner)
				i += end + 1
			} else if unicode.IsLetter(first) || first == '/' || first == '!' || first == '?' {
				i += end + 1 // raw HTML tag: removed, not rendered
			} else {
				b.WriteByte(c)
				i++
			}
		case c == '*' || c == '_' || c == '~':
			n := runLen(s[i:], c)
			if !canOpenEmph(s, i, c) {
				b.WriteString(s[i : i+n])
				i += n
				continue
			}
			end := findEmphClose(s, i+n, c, n)
			if end < 0 {
				b.WriteString(s[i : i+n])
				i += n
				continue
			}
			b.WriteString(flattenInline(s[i+n : end]))
			i = end + n
		case c == '&':
			if text, n := decodeEntity(s[i:]); n > 0 {
				b.WriteString(text)
				i += n
			} else {
				b.WriteByte(c)
				i++
			}
		default:
			b.WriteByte(c)
			i++
		}
	}
	return strings.TrimSpace(b.String())
}

// wrapText greedily wraps s at width runes on spaces, hard-cutting words
// that exceed the window, so a paragraph renders as several readable rows.
func wrapText(s string, width int) []string {
	rs := []rune(s)
	var out []string
	for len(rs) > width {
		cut := -1
		for j := 0; j < width; j++ {
			if rs[j] == ' ' {
				cut = j
			}
		}
		if cut <= 0 {
			cut = width
			out = append(out, string(rs[:cut]))
			rs = rs[cut:]
			continue
		}
		out = append(out, string(rs[:cut]))
		rs = rs[cut+1:]
	}
	if len(rs) > 0 {
		out = append(out, string(rs))
	}
	return out
}

// clipRunes truncates s to at most n bytes on a rune boundary, marking the
// cut with an ellipsis, as the contract's label budget requires.
func clipRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
