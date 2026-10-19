package migrate

// Progress describes work within one schema-version upgrade, not a committed
// transaction. Completed and Total are work units, not bytes or elapsed time;
// their ratio is an approximate overall fraction for this version pair. Total
// stays positive and fixed, and Completed never decreases within that pair.
// ToVersion is always FromVersion+1. Current schemas produce the phases
// read_entries, validate_retention, rewrite_retention, update_cursors,
// convert_messages, rebuild_entries, convert_child_completions,
// remove_archive_metadata, index_entry_created, index_prompt_recall,
// index_workspace_sessions, remove_global_session_index, update_schema_version,
// and complete. Index phases report completed SQL operations, not row counts or
// elapsed-time estimates; duration within each operation is unknown.
// Phases can be skipped when there is no corresponding work.
type Progress struct {
	FromVersion int
	ToVersion   int
	Completed   int64
	Total       int64
	// Phase identifies the current operation. "complete" means the step and
	// its user_version update succeeded inside the still-uncommitted transaction.
	Phase string
}

// progressBatchSize bounds callbacks during entry-by-entry migration work.
const progressBatchSize = 64

type progressReporter struct {
	report       func(Progress)
	value        Progress
	lastReported int64
}

func (p *progressReporter) start(total int64, phase string) {
	p.value.Total = total
	p.phase(phase)
}

func (p *progressReporter) advance(count int64) {
	if p == nil {
		return
	}
	p.value.Completed += count
	if p.value.Completed-p.lastReported >= progressBatchSize {
		p.phase(p.value.Phase)
	}
}

func (p *progressReporter) phase(phase string) {
	if p == nil {
		return
	}
	p.value.Phase = phase
	p.lastReported = p.value.Completed
	p.report(p.value)
}

func (p *progressReporter) finish() {
	if p == nil {
		return
	}
	p.value.Completed = p.value.Total
	p.phase("complete")
}
