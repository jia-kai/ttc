package tui

import (
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"ttc/internal/render"
)

// transcript owns a lazily laid-out history. Row heights for unseen blocks are
// estimates; a Fenwick index locates the viewport without flattening history.
// The first visible block/row anchors reading across resize and height discovery.
type transcript struct {
	lines             []line
	blocks            []textBlock
	tree              []int
	width             int
	firstLine         int
	followTail        bool
	anchor, anchorRow int
	anchorSource      int // Byte position in the original block, independent of wrapping.
	resolveAnchor     bool
	visible           []line
	total             int
	layout            func(line, int) []displayRow
	recent            []int
	renders           int // Number of blocks laid out, useful for pressure tests.
	calls             map[string]int
	streams           map[int64]*strings.Builder // Owned by the live view, released at completion.
	requests          map[int64]int              // Assistant blocks, indexed by producing request across replay/stream updates.
}
type textBlock struct {
	owner  int
	text   string
	height int
	rows   []displayRow
	fence  string // Synthetic opening fence for a chunk continuing a code block.
	indent string // Container/code indentation when the first physical line was split.
}

func newTranscript() *transcript {
	return &transcript{followTail: true, streams: map[int64]*strings.Builder{}, calls: map[string]int{}, requests: map[int64]int{}, width: 80, layout: func(v line, w int) []displayRow { return rowsOf(layoutText(v, w)) }}
}
func layoutText(v line, width int) []string {
	if !v.markdown {
		return wrap(render.Clean(v.text), width)
	}
	var text string
	var err error
	if v.brief {
		if render.DiffBriefing(v.text) {
			header, body, _ := strings.Cut(v.text, "\n\n")
			text, err = render.TerminalBriefing(header, width, true)
			if err == nil {
				var diff string
				diff, err = render.Terminal(body, width)
				text += "\n" + strings.Trim(diff, "\n")
			}
		} else {
			text, err = render.TerminalBriefing(v.text, width, true)
		}
	} else {
		text, err = render.Terminal(v.text, width)
	}
	if err != nil {
		return append([]string{"Markdown rendering failed: " + err.Error()}, wrap(render.Clean(v.text), width)...)
	}
	return styledRows(strings.Trim(text, "\n"))
}

// append partitions exceptionally long messages so scrolling never typesets an
// unbounded message. Inspectors/history preserve the complete original source.
func (t *transcript) append(v line) {
	i := len(t.lines)
	t.lines = append(t.lines, v)
	if v.requestID > 0 {
		t.requests[v.requestID] = i
	}
	if v.callID != "" {
		t.calls[v.callID] = i
	}
	before := len(t.blocks)
	t.appendBlocks(i, v)
	t.extendIndex(before)
}
func (t *transcript) appendBlocks(owner int, v line) {
	const chunkBytes = 16 << 10
	text := v.text
	fence := ""
	indent := ""
	for {
		n := min(len(text), chunkBytes-min(len(fence)+len(indent)+1, chunkBytes/2))
		if n < len(text) {
			for n > 0 && !utf8.RuneStart(text[n]) {
				n--
			}
			if k := strings.LastIndexByte(text[:n], '\n'); k > n/2 {
				n = k + 1
			}
			if v.markdown && !v.brief {
				if cut := render.MathChunkCut(text, n); cut > 0 {
					n = cut
				}
			}
		}
		part := text[:n]
		height := max(1, (len(part)+t.width-1)/t.width)
		if v.brief {
			height = 1
			if render.DiffBriefing(v.text) {
				height = 8
			}
		}
		if v.image != nil {
			height = 10
		}
		t.blocks = append(t.blocks, textBlock{owner: owner, text: part, height: height, fence: fence, indent: indent})
		if v.markdown {
			fence = continuedFence(part, fence)
			nextIndent := ""
			if n < len(text) && !strings.HasSuffix(part, "\n") {
				last := strings.LastIndexByte(part, '\n')
				row := part[last+1:]
				if last < 0 {
					row = indent + row
				}
				nextIndent = continuationIndent(row, fence != "")
			}
			indent = nextIndent
		}
		text = text[n:]
		if text == "" {
			break
		}
	}
}
func (t *transcript) reset() {
	t.lines = nil
	t.streams = map[int64]*strings.Builder{}
	t.calls = map[string]int{}
	t.requests = map[int64]int{}
	t.blocks = nil
	t.tree = nil
	t.visible = nil
	t.recent = nil
	t.firstLine = 0
	t.anchor = 0
	t.anchorRow = 0
	t.anchorSource, t.resolveAnchor = 0, false
	t.followTail = true
}
func (t *transcript) replace(i int, v line) {
	if i < 0 || i >= len(t.lines) {
		return
	}
	if old := t.lines[i].callID; old != "" && old != v.callID {
		delete(t.calls, old)
	}
	if old := t.lines[i].requestID; old > 0 && old != v.requestID {
		delete(t.requests, old)
	}
	t.lines[i] = v
	if v.requestID > 0 {
		t.requests[v.requestID] = i
	}
	if v.callID != "" {
		t.calls[v.callID] = i
	}
	begin := sort.Search(len(t.blocks), func(n int) bool { return t.blocks[n].owner >= i })
	end := begin
	for end < len(t.blocks) && t.blocks[end].owner == i {
		end++
	}
	// Keep the reading position in source bytes if this message is repartitioned.
	anchored := t.anchor >= begin && t.anchor < end
	source := t.anchorSource
	if anchored {
		for n := begin; n < t.anchor; n++ {
			source += len(t.blocks[n].text)
		}
	}
	// Replaced blocks lose their cached rows. Retain only unchanged cache entries.
	recent := t.recent[:0]
	for _, n := range t.recent {
		if n < begin || n >= end {
			recent = append(recent, n)
		}
	}
	t.recent = recent
	if end-begin == 1 && (v.brief || len(v.text) <= 16<<10) {
		old := t.blocks[begin].height
		t.blocks[begin].text = v.text
		t.blocks[begin].rows = nil
		height := max(1, (len(v.text)+t.width-1)/t.width)
		if v.brief {
			height = 1
		}
		if v.image != nil {
			height = 10
		}
		t.blocks[begin].height = height
		t.addHeight(begin, height-old)
		if anchored {
			t.resolveAnchor = true
		}
		return
	}
	tail := append([]textBlock(nil), t.blocks[end:]...)
	t.blocks = t.blocks[:begin]
	t.appendBlocks(i, v)
	newEnd := len(t.blocks)
	delta := newEnd - end
	t.blocks = append(t.blocks, tail...)
	for n, index := range t.recent {
		if index >= end {
			t.recent[n] = index + delta
		}
	}
	if anchored {
		t.anchor = begin
		for t.anchor+1 < newEnd && source >= len(t.blocks[t.anchor].text) {
			source -= len(t.blocks[t.anchor].text)
			t.anchor++
		}
		t.anchorSource = min(source, len(t.blocks[t.anchor].text))
		t.resolveAnchor = true
	} else if t.anchor >= end {
		t.anchor += delta
	}
	if len(tail) == 0 {
		// Replacing the newest message changes only the indexed suffix. Earlier
		// Fenwick nodes never include later blocks, so keep them across deltas.
		t.tree = t.tree[:begin+1]
		t.extendIndex(begin)
	} else {
		t.reindex()
	}
}

// growPlain appends only to the last bounded chunk of a streaming message.
// Earlier chunks and their layout/index entries remain unchanged. Completed
// Markdown uses replace once, when the full source is available.
func (t *transcript) growPlain(owner int, item line, delta string) {
	end := sort.Search(len(t.blocks), func(n int) bool { return t.blocks[n].owner > owner })
	last := end - 1
	if last < 0 || t.blocks[last].owner != owner {
		panic("streaming message has no text block")
	}
	oldHeight := t.blocks[last].height
	text := t.blocks[last].text + delta
	tail := append([]textBlock(nil), t.blocks[end:]...)
	t.blocks = t.blocks[:last]
	t.appendBlocks(owner, line{text: text})
	newEnd := len(t.blocks)
	shift := newEnd - end
	t.blocks = append(t.blocks, tail...)
	recent := t.recent[:0]
	for _, n := range t.recent {
		if n == last {
			continue
		}
		if n >= end {
			n += shift
		}
		recent = append(recent, n)
	}
	t.recent = recent
	if t.anchor == last {
		for t.anchor+1 < newEnd && t.anchorSource >= len(t.blocks[t.anchor].text) {
			t.anchorSource -= len(t.blocks[t.anchor].text)
			t.anchor++
		}
		t.resolveAnchor = true
	} else if t.anchor >= end {
		t.anchor += shift
	}
	t.lines[owner] = item
	if len(tail) == 0 {
		t.tree = t.tree[:last+1]
		t.extendIndex(last)
	} else if shift == 0 {
		t.addHeight(last, t.blocks[last].height-oldHeight)
	} else {
		t.reindex()
	}
}

// publish applies a current presentation to one stable block. Pending streamed
// tool identities may become committed call IDs; completed blocks reject late
// updates. Both tools and assistant replies use this UI-owned snapshot path.
func (t *transcript) publish(item line, pending string, delta bool) bool {
	index, found := t.calls[item.callID]
	if item.callID == "" {
		found = false
	}
	if !found && pending != "" {
		index, found = t.calls[pending]
	}
	if item.requestID > 0 {
		index, found = t.requests[item.requestID]
	}
	if found {
		old := t.lines[index]
		if old.complete {
			return false
		}
		deltaText := item.text
		if delta {
			builder := t.streams[item.requestID]
			if builder == nil {
				builder = &strings.Builder{}
				builder.WriteString(old.text)
				t.streams[item.requestID] = builder
			}
			builder.WriteString(deltaText)
			item.text = builder.String()
		}
		if item.image == nil {
			item.image = old.image
		}
		if delta && !old.markdown && !item.markdown {
			t.growPlain(index, item, deltaText)
		} else {
			t.replace(index, item)
		}
	} else {
		t.append(item)
	}
	if item.complete {
		delete(t.streams, item.requestID)
	}
	return true
}

// assistant publishes deltas as plain text and replaces them with Markdown when
// finished. Request identity prevents duplicate completion after replay.
func (t *transcript) assistant(request int64, speaker, text string, entry int64, complete bool) bool {
	return t.publish(line{text: text, speaker: speaker, id: entry, requestID: request, markdown: complete, complete: complete}, "", !complete)
}

// snapshot shares immutable text/rows and copies mutable indices for copy mode.
// Background publications continue in the live transcript; this view stays fixed.
func (t *transcript) snapshot() *transcript {
	v := *t
	v.streams = nil
	v.lines = append([]line(nil), t.lines...)
	v.blocks = append([]textBlock(nil), t.blocks...)
	v.tree = append([]int(nil), t.tree...)
	v.recent = append([]int(nil), t.recent...)
	v.visible = nil
	v.calls = make(map[string]int, len(t.calls))
	for k, i := range t.calls {
		v.calls[k] = i
	}
	v.requests = make(map[int64]int, len(t.requests))
	for k, i := range t.requests {
		v.requests[k] = i
	}
	return &v
}

func (t *transcript) invalidate() {
	t.resolveAnchor = true
	for _, i := range t.recent {
		if i < len(t.blocks) {
			t.blocks[i].rows = nil

		}
	}
	t.recent = nil
}
func (t *transcript) extendIndex(begin int) {
	if len(t.tree) == 0 {
		t.tree = []int{0}
	}
	for i := begin; i < len(t.blocks); i++ {
		n := i + 1
		value := t.blocks[i].height + t.prefix(i) - t.prefix(n-(n&-n))
		t.tree = append(t.tree, value)
	}
}
func (t *transcript) reindex() {
	t.tree = make([]int, len(t.blocks)+1)
	for i := range t.blocks {
		t.addHeight(i, t.blocks[i].height)
	}
}
func (t *transcript) addHeight(i, delta int) {
	for i++; i < len(t.tree); i += i & -i {
		t.tree[i] += delta
	}
}
func (t *transcript) prefix(end int) int {
	n := 0
	for ; end > 0; end -= end & -end {
		n += t.tree[end]
	}
	return n
}
func (t *transcript) locate(row int) int {
	idx, sum := 0, 0
	bit := 1
	for bit < len(t.tree) {
		bit <<= 1
	}
	for bit >>= 1; bit > 0; bit >>= 1 {
		next := idx + bit
		if next < len(t.tree) && sum+t.tree[next] <= row {
			idx = next
			sum += t.tree[next]
		}
	}
	return min(idx, max(0, len(t.blocks)-1))
}
func (t *transcript) resize(width int) {
	width = max(1, width)
	if t.width == width {
		return
	}
	old := t.width
	t.width = width
	t.resolveAnchor = true
	for i := range t.blocks {
		b := &t.blocks[i]
		b.rows = nil
		b.height = max(1, (b.height*old+width-1)/width)
		if t.lines[b.owner].brief {
			b.height = 1
		}
	}
	t.reindex()
}
func (t *transcript) measure(i int) {
	b := &t.blocks[i]
	if b.rows != nil {
		return
	}
	v := t.lines[b.owner]
	v.text = b.text
	if b.indent != "" {
		v.text = b.indent + v.text
	}
	if b.fence != "" {
		v.text = b.fence + "\n" + v.text
	}
	indent := 0
	first := i == 0 || t.blocks[i-1].owner != b.owner
	badge := ""
	if first && v.subagentName != "" {
		badge = render.SubagentBadge(v.actor, v.subagentName, max(1, t.width/2), true)
	}
	badgeWidth := 0
	if badge != "" {
		badgeWidth = ansi.StringWidth(badge) + 1
	}
	if v.speaker != "" {
		indent = min(2, max(0, t.width-1))
	}
	bodyWidth := t.width - indent
	if v.speaker == "" {
		bodyWidth -= badgeWidth
	}
	b.rows = t.layout(v, max(1, bodyWidth))
	if len(b.rows) == 0 {
		b.rows = []displayRow{{text: ""}}
	}
	mapSources(b.rows, b.text)
	if v.speaker != "" {
		for row := range b.rows {
			b.rows[row].text = strings.Repeat(" ", indent) + b.rows[row].text
		}
		if first {
			label, err := render.TerminalBriefing("**"+render.Inline(v.speaker)+"**", max(1, t.width-badgeWidth), true)
			if err != nil {
				label = render.Clean(v.speaker)
			}
			if badge != "" {
				label = ansi.Truncate(badge+" "+label, max(1, t.width), "…")
			}
			b.rows = append([]displayRow{{text: label, styled: true}}, b.rows...)
		}
	} else if badge != "" {
		b.rows[0].text = ansi.Truncate(badge+" "+b.rows[0].text, max(1, t.width), "…")
		b.rows[0].styled = true
	}
	t.renders++
	t.recent = append(t.recent, i)
	if len(t.recent) > 256 {
		evict := t.recent[0]
		t.recent = t.recent[1:]
		if evict != i && evict < len(t.blocks) {
			t.blocks[evict].rows = nil

		}
	}
	delta := len(b.rows) - b.height
	b.height = len(b.rows)
	t.addHeight(i, delta)
}

// pageDown resumes following when invoked at the bottom; ordinary wheel
// scrolling still preserves a fixed reading position.
func (t *transcript) pageDown(height int) {
	if t.firstLine >= max(0, t.total-height) {
		t.followTail = true
		return
	}
	t.scroll(max(1, height/2), height)
}

func (t *transcript) scroll(delta, height int) {
	t.firstLine = min(max(0, t.firstLine+delta), max(0, t.total-height))
	t.followTail = false
	if len(t.blocks) > 0 {
		t.anchor = t.locate(t.firstLine)
		t.anchorRow = t.firstLine - t.prefix(t.anchor)
		b := &t.blocks[t.anchor]
		if b.rows != nil && t.anchorRow < len(b.rows) {
			t.anchorSource = b.rows[t.anchorRow].source
		} else {
			t.anchorSource = len(b.text) * t.anchorRow / max(1, b.height)
		}
		t.resolveAnchor = false
	}
}
func (t *transcript) viewport(width, height int) []line {
	t.resize(width)
	height = max(0, height)
	t.visible = t.visible[:0]
	if len(t.blocks) == 0 || height == 0 {
		t.total = t.prefix(len(t.blocks))
		return t.visible
	}
	start, row := min(t.anchor, len(t.blocks)-1), t.anchorRow
	if t.followTail {
		start = len(t.blocks) - 1
		remaining := height
		for start >= 0 {
			t.measure(start)
			remaining -= t.blocks[start].height
			if remaining <= 0 {
				row = -remaining
				break
			}
			start--
		}
		if start < 0 {
			start, row = 0, 0
		}
	}
	t.measure(start)
	if !t.followTail && t.resolveAnchor {
		row = max(0, sort.Search(len(t.blocks[start].rows), func(i int) bool { return t.blocks[start].rows[i].source > t.anchorSource })-1)
	}
	t.resolveAnchor = false
	row = min(max(0, row), t.blocks[start].height-1)
	t.firstLine = t.prefix(start) + row
	t.anchor, t.anchorRow = start, row
	t.anchorSource = t.blocks[start].rows[row].source
	for i := start; i < len(t.blocks) && len(t.visible) < height; i++ {
		t.measure(i)
		v := t.lines[t.blocks[i].owner]
		for j := row; j < len(t.blocks[i].rows) && len(t.visible) < height; j++ {
			v.text = t.blocks[i].rows[j].text
			v.styled = t.blocks[i].rows[j].styled
			v.assets = t.blocks[i].rows[j].assets
			t.visible = append(t.visible, v)
		}
		row = 0
	}
	t.total = t.prefix(len(t.blocks))
	// Keep a small rendering working set. History source/index stay available.

	return t.visible
}

// mapSources matches visible text monotonically back to a bounded source chunk.
// Markdown decorations and image cells have no source bytes; they retain the
// nearest preceding source position. Plain wrapped paragraphs match exactly.
func mapSources(rows []displayRow, source string) {
	cursor := 0
	for i := range rows {
		text := strings.TrimSpace(ansi.Strip(rows[i].text))
		position := -1
		if text != "" {
			position = strings.Index(source[cursor:], text)
		}
		if position < 0 {
			for _, word := range strings.Fields(text) {
				if len(word) > 1 && !strings.ContainsRune(word, '\U0010eeee') {
					if n := strings.Index(source[cursor:], word); n >= 0 {
						position = n
						text = word
						break
					}
				}
			}
		}
		rows[i].source = cursor
		if position >= 0 {
			rows[i].source = cursor + position
			cursor += position + len(text)
		}
	}
}

// continuedFence preserves code protection/language across lazy chunk boundaries.
func continuedFence(text, open string) string {
	for _, row := range strings.Split(text, "\n") {
		line := fenceText(row)
		if len(line) < 3 || (line[0] != '`' && line[0] != '~') {
			continue
		}
		n := 0
		for n < len(line) && line[n] == line[0] {
			n++
		}
		if n < 3 {
			continue
		}
		if open == "" {
			open = row
			continue
		}
		count := 0
		marker := fenceText(open)
		for count < len(marker) && marker[count] == marker[0] {
			count++
		}
		if line[0] == marker[0] && n >= count && strings.TrimSpace(line[n:]) == "" {
			open = ""
		}
	}
	return open
}

func fenceText(row string) string {
	text := strings.TrimLeft(row, " ")
	for strings.HasPrefix(text, ">") {
		text = strings.TrimLeft(text[1:], " ")
	}
	if len(text) > 1 && strings.ContainsRune("-*+", rune(text[0])) && text[1] == ' ' {
		text = strings.TrimLeft(text[2:], " ")
	}
	// Ordered-list markers may surround a fenced block too.
	n := 0
	for n < len(text) && text[n] >= '0' && text[n] <= '9' {
		n++
	}
	if n > 0 && n+1 < len(text) && (text[n] == '.' || text[n] == ')') && text[n+1] == ' ' {
		text = strings.TrimLeft(text[n+2:], " ")
	}
	return text
}
func continuationIndent(row string, fenced bool) string {
	position := 0
	for position < len(row) && row[position] == ' ' {
		position++
	}
	if position >= 4 {
		return row[:position]
	}
	quoted := false
	for position < len(row) && row[position] == '>' {
		quoted = true
		position++
		for position < len(row) && row[position] == ' ' {
			position++
		}
	}
	if quoted && (fenced || position >= 5) {
		return row[:position]
	}
	return ""
}
func (t *transcript) entryAt(y int) int64 {
	if y >= 0 && y < len(t.visible) {
		return t.visible[y].id
	}
	return 0
}
