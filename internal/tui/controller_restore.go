package tui

import (
	contextbuild "ttc/internal/context"
	"ttc/internal/session"
)

// restoreBaseline distinguishes an explicit cancel's replacement intent from
// rejected input returned after the human may have started another draft.
type restoreBaseline struct {
	draftRevision   uint64
	attachmentEpoch uint64
	attachmentCount int
}

func (c *frontendController) restoreResult(result session.OperationResult) {
	baseline, explicit := c.pendingRestores[result.RequestID]
	delete(c.pendingRestores, result.RequestID)
	input := result.Restored
	if input == nil {
		return
	}
	text := input.Text
	if (!explicit || baseline.draftRevision != c.draft.revision) && c.draft.text != "" {
		text = c.draft.text + "\n\n" + text
	}
	c.recallInput(text)
	if explicit && baseline.attachmentEpoch == c.attachmentEpoch && baseline.attachmentCount <= len(c.attachments) {
		// Discard the pre-command draft's attachments, but retain attachments added
		// while the cancellation request was in flight. Snapshot bytes remain owned.
		newer := append([]contextbuild.Attachment(nil), c.attachments[baseline.attachmentCount:]...)
		c.attachments = append(append([]contextbuild.Attachment(nil), input.Attachments...), newer...)
	} else {
		c.attachments = append(c.attachments, input.Attachments...)
	}
	c.attachmentEpoch++
}
