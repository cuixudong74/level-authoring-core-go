package levelcore

// HistoryEntry 是一条历史记录（可能对应一次合并提交或一个批次）。
// 字段全部可序列化/可比较，便于跨机器逐字段对齐。
type HistoryEntry struct {
	Kind    string
	Summary string
	// Count 为被合并进本条的原始命令数（最小为 1）。
	Count int
	// commands 为逆序应用所需的命令（批次按提交顺序保存，撤销时倒序）。
	commands []Command
}

// history 是有上限的撤销/重做栈。
//
// 语义：
//   - entries[:cursor] 为当前已应用、可撤销的历史；
//   - entries[cursor:] 为被撤销、可重做的历史；
//   - 撤销之后再提交新编辑，必须丢弃 entries[cursor:]（截断重做历史）；
//   - 历史条数超过 limit 时，淘汰最旧的已应用条目，不影响最近编辑。
type history struct {
	entries []*HistoryEntry
	cursor  int
	limit   int
}

// DefaultHistoryLimit 是撤销历史的默认上限。
const DefaultHistoryLimit = 256

func newHistory(limit int) *history {
	if limit <= 0 {
		limit = DefaultHistoryLimit
	}
	return &history{limit: limit}
}

// push 在 cursor 处追加一条：先截断重做历史，再按需淘汰最旧条目。
func (h *history) push(e *HistoryEntry) {
	if h.cursor < len(h.entries) {
		h.entries = h.entries[:h.cursor]
	}
	h.entries = append(h.entries, e)
	h.cursor = len(h.entries)
	h.evict()
}

// evict 超出上限时丢弃最旧的已应用条目；重做侧为空时才会到达这里，
// 因此只可能淘汰 entries 前端，cursor 随之回移。
func (h *history) evict() {
	for len(h.entries) > h.limit {
		// 正常路径：重做栈已被截断，cursor==len，淘汰 entries[0]。
		if h.cursor == len(h.entries) {
			h.entries = h.entries[1:]
			h.cursor--
			continue
		}
		// 防御路径：若 cursor 未到底，也从最旧端淘汰并修正 cursor。
		h.entries = append(h.entries[:0], h.entries[1:]...)
		h.cursor--
	}
	if h.cursor < 0 {
		h.cursor = 0
	}
}

func (h *history) canUndo() bool { return h.cursor > 0 }
func (h *history) canRedo() bool { return h.cursor < len(h.entries) }

// popUndo 取出最近一条已应用记录并回退 cursor；调用方负责执行逆操作。
func (h *history) popUndo() *HistoryEntry {
	if h.cursor == 0 {
		return nil
	}
	h.cursor--
	return h.entries[h.cursor]
}

// nextRedo 取出下一条可重做记录并推进 cursor；调用方负责重新应用。
func (h *history) nextRedo() *HistoryEntry {
	if h.cursor >= len(h.entries) {
		return nil
	}
	e := h.entries[h.cursor]
	h.cursor++
	return e
}

// lastApplied 返回最近一条已应用记录（用于合并判断），不存在时返回 nil。
func (h *history) lastApplied() *HistoryEntry {
	if h.cursor == 0 {
		return nil
	}
	return h.entries[h.cursor-1]
}

// Snapshot 是用于跨机器对齐的确定性历史视图。
type Snapshot struct {
	Cursor  int
	Entries []HistoryEntry
}

// snapshot 生成不包含命令体的确定性历史快照。
func (h *history) snapshot() Snapshot {
	out := make([]HistoryEntry, len(h.entries))
	for i, e := range h.entries {
		out[i] = HistoryEntry{Kind: e.Kind, Summary: e.Summary, Count: e.Count}
	}
	return Snapshot{Cursor: h.cursor, Entries: out}
}
