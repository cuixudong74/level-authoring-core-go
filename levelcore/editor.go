package levelcore

// Config 控制 Editor 的上限与合并策略。
type Config struct {
	// HistoryLimit 为撤销历史条目上限；<=0 使用 DefaultHistoryLimit。
	HistoryLimit int
	// CacheLimit 为派生产物块缓存上限；<=0 使用 DefaultCacheLimit。
	CacheLimit int
	// Coalesce 定义连续同类微调的合并时间窗；
	// 时间戳由提交方随命令提供，Editor 不读取本机墙钟。
	Coalesce MergePolicy
}

// DefaultConfig 返回默认配置。
func DefaultConfig() Config {
	return Config{
		HistoryLimit: DefaultHistoryLimit,
		CacheLimit:   DefaultCacheLimit,
		Coalesce:     MergePolicy{Window: DefaultMergeWindow},
	}
}

// SubmitResult 汇报一次提交的结果。
type SubmitResult struct {
	// EntryIndex 为本次提交后该历史条目在已应用历史中的位置。
	EntryIndex int
	// Merged 表示本次命令被并入上一条历史（连续同类微调）。
	Merged bool
	// Revision 为提交后产物修订号。
	Revision uint64
}

// Editor 是编辑内核的对外门面。
type Editor struct {
	doc     *Document // 工作副本（提交成功后冻结）
	frozen  *Document // 最近一次成功提交的冻结版本
	hist    *history
	builder *artifactBuilder
	cfg     Config
	cons    *ConstraintSet
	rev     uint64
}

// NewEditor 基于初始文档创建编辑器。文档会先通过全量一致性校验，
// 并从零全量构建一次派生产物。
func NewEditor(d *Document, cfg Config) (*Editor, error) {
	if d == nil {
		return nil, rejectf(-1, RejectInvalidValue, "document must not be nil")
	}
	if cfg.HistoryLimit <= 0 {
		cfg.HistoryLimit = DefaultHistoryLimit
	}
	if cfg.CacheLimit <= 0 {
		cfg.CacheLimit = DefaultCacheLimit
	}
	if d.SchemaVersion == 0 {
		d.SchemaVersion = FormatVersion
	}
	cons := DefaultConstraints()
	if err := cons.ValidateAll(d); err != nil {
		return nil, err
	}
	d.rebuildIndex()
	e := &Editor{
		doc:     d,
		frozen:  d,
		hist:    newHistory(cfg.HistoryLimit),
		builder: newArtifactBuilder(cfg.CacheLimit),
		cfg:     cfg,
		cons:    cons,
	}
	e.builder.rebuildFrom(d, e.rev)
	return e, nil
}

// Document 返回当前文档的不可变快照（零拷贝，可长期持有与比较）。
func (e *Editor) Document() *Document { return e.frozen }

// Artifact 返回当前派生产物的不可变快照（零拷贝，可安全长期持有与比较）。
func (e *Editor) Artifact() *Artifact {
	return e.builder.artifact
}

// Revision 返回当前产物修订号。
func (e *Editor) Revision() uint64 { return e.rev }

// History 返回确定性历史快照（仅元数据，不含命令体）。
func (e *Editor) History() Snapshot { return e.hist.snapshot() }

func (e *Editor) CanUndo() bool { return e.hist.canUndo() }
func (e *Editor) CanRedo() bool { return e.hist.canRedo() }

// Submit 提交一条命令。
//
// 若该命令与最近一条已应用历史属于"连续同类微调"（同目标、同种类、
// 时间戳在合并窗内），则并入该历史：合并前/后的文档状态等价于
// 直接执行最终命令，历史长度保持不变，Count 加 1。
// 命令被约束拒绝时，文档与产物完全不变，错误可定位（单条时下标为 0）。
func (e *Editor) Submit(c Command) (SubmitResult, error) {
	return e.submit(cs1(c), false)
}

// SubmitBatch 把多条命令作为一个整体提交：
//   - 全部命令先在文档上执行，再做最终状态约束校验；
//   - 任一命令的结构前置检查失败，或最终校验失败，立即逆序回滚已执行部分，
//     文档、产物、历史完全回到提交前；
//   - 返回的 *RejectError.CommandIndex 定位到批次中的具体命令。
//
// 批次内部不做命令合并；作为一条历史记录呈现。
func (e *Editor) SubmitBatch(cs []Command) (SubmitResult, error) {
	return e.submit(cs, true)
}

func cs1(c Command) []Command { return []Command{c} }

func (e *Editor) submit(cs []Command, isBatch bool) (SubmitResult, error) {
	if len(cs) == 0 {
		return SubmitResult{}, rejectf(-1, RejectInvalidValue, "empty command batch")
	}
	for i, c := range cs {
		if c == nil {
			return SubmitResult{}, rejectf(i, RejectInvalidValue, "command must not be nil")
		}
	}

	// 合并判断：仅单条提交、且最近历史为可合并目标时进行。
	var mergeTarget *HistoryEntry
	if !isBatch {
		if last := e.hist.lastApplied(); last != nil && len(last.commands) > 0 {
			if last.commands[0].canMergeTo(cs[0], e.cfg.Coalesce) {
				mergeTarget = last
			}
		}
	}

	// 从最近冻结版本派生本批次的工作副本（COW，零单元复制）。
	e.doc = e.frozen.clone()
	dl := e.doc.beginDelta()
	applied := 0
	for i, c := range cs {
		if err := c.apply(e.doc); err != nil {
			// 结构性失败：逆序回滚已应用命令。
			for j := applied - 1; j >= 0; j-- {
				cs[j].undo(e.doc)
			}
			e.doc.endDelta()
			e.doc = e.frozen
			err.CommandIndex = i
			return SubmitResult{}, err
		}
		applied++
	}

	// 最终状态约束：逐命令在"全部应用后的文档"上校验。
	for i := len(cs) - 1; i >= 0; i-- {
		if err := cs[i].verify(e.doc); err != nil {
			err.CommandIndex = i
			// 约束失败：逆序回滚全部命令，丢弃增量。
			for j := len(cs) - 1; j >= 0; j-- {
				cs[j].undo(e.doc)
			}
			e.doc.endDelta()
			e.doc = e.frozen
			return SubmitResult{}, err
		}
	}
	e.doc.endDelta()
	e.doc.markFrozen()
	e.frozen = e.doc // 提交成功：冻结本工作版本

	// 提交成功：更新产物与历史。
	e.rev++
	if mergeTarget != nil {
		// 折叠式合并：
		//   commands[0] 保留最早命令的逆操作信息（undo 回退到合并前）；
		//   commands[1] 为最新命令（持有当前状态，供重放/摘要）。
		// 无论合并多少次，槽位恒定为 2，历史成本不随微调次数增长。
		first := mergeTarget.commands[0]
		first.merge(cs[0])
		latest := cs[0]
		if len(mergeTarget.commands) >= 2 {
			mergeTarget.commands[1] = latest
		} else {
			mergeTarget.commands = append(mergeTarget.commands, latest)
		}
		mergeTarget.Count++
		mergeTarget.Summary = latest.describe()
		mergeTarget.Kind = latest.kind()
		e.builder.applyDelta(e.doc, e.rev, dl)
		return SubmitResult{EntryIndex: e.hist.cursor - 1, Merged: true, Revision: e.rev}, nil
	}

	entry := &HistoryEntry{
		Kind:     cs[0].kind(),
		Summary:  cs[0].describe(),
		Count:    len(cs),
		commands: append([]Command(nil), cs...),
	}
	if isBatch {
		entry.Kind = "Batch"
		entry.Summary = batchSummary(cs)
	}
	e.hist.push(entry)
	e.builder.applyDelta(e.doc, e.rev, dl)
	return SubmitResult{EntryIndex: e.hist.cursor - 1, Merged: false, Revision: e.rev}, nil
}

// Undo 撤销最近一条历史（批次内命令按倒序撤销）。
// 撤销后产物同步回到相应状态，不残留过期内容。
func (e *Editor) Undo() bool {
	entry := e.hist.popUndo()
	if entry == nil {
		return false
	}
	e.doc = e.frozen.clone()
	dl := e.doc.beginDelta()
	for i := len(entry.commands) - 1; i >= 0; i-- {
		entry.commands[i].undo(e.doc)
	}
	e.doc.endDelta()
	e.doc.markFrozen()
	e.frozen = e.doc
	e.rev++
	e.builder.applyDelta(e.doc, e.rev, dl)
	return true
}

// Redo 重做最近被撤销的历史（批次内命令按顺序重新应用）。
// 重放使用命令已记录的逆操作信息，不依赖任何外部状态。
func (e *Editor) Redo() bool {
	entry := e.hist.nextRedo()
	if entry == nil {
		return false
	}
	e.doc = e.frozen.clone()
	startCursor := e.hist.cursor - 1
	dl := e.doc.beginDelta()
	for i, c := range entry.commands {
		if err := c.apply(e.doc); err != nil {
			// 已应用命令的结构前置条件在撤销前成立，此处理论上不会触发；
			// 若触发则回滚已应用部分并把 cursor 复位，保持内核一致。
			for j := i - 1; j >= 0; j-- {
				entry.commands[j].undo(e.doc)
			}
			e.doc.endDelta()
			e.hist.cursor = startCursor
			return false
		}
	}
	e.doc.endDelta()
	e.doc.markFrozen()
	e.frozen = e.doc
	e.rev++
	e.builder.applyDelta(e.doc, e.rev, dl)
	return true
}

func batchSummary(cs []Command) string {
	counts := map[string]int{}
	order := []string{}
	for _, c := range cs {
		k := c.kind()
		if _, seen := counts[k]; !seen {
			order = append(order, k)
		}
		counts[k]++
	}
	out := "Batch("
	for i, k := range order {
		if i > 0 {
			out += ","
		}
		out += k + "x" + intToStr(int32(counts[k]))
	}
	return out + ")"
}
