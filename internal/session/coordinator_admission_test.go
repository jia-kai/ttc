package session

import (
	"context"
	"reflect"
	"strings"
	"testing"

	contextbuild "ttc/internal/context"
	"ttc/internal/llm"
)

func admittedMainFixture(t *testing.T) (*Runtime, *SessionCoordinator, uint64) {
	t.Helper()
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "seed")
	c := r.Coordinator()
	execution := c.executions.begin("main", "main", "", r.Generation())
	c.active = &execution
	c.executions.setPhase(execution.ID, "running")
	ctx, cancel := context.WithCancelCause(r.ctx)
	t.Cleanup(func() { cancel(nil) })
	r.mu.Lock()
	r.activeTurn = "admitted-fixture"
	r.activeCancel = cancel
	r.mu.Unlock()
	r.openMainAdmission(ctx)
	return r, c, execution.ID
}

func TestCoordinatorFinalDecisionSealsSteeringBeforePersistence(t *testing.T) {
	r, c, id := admittedMainFixture(t)
	policy := mainAgentPolicy{runtime: r}
	done, err := policy.complete(context.Background(), r.CurrentSelection(), llm.Message{Type: llm.TypeAssistant, Content: "final"})
	if err != nil || !done {
		t.Fatal("did not reach final decision", done, err)
	}
	// Runtime active fields deliberately remain live, exactly as while FinishTurn
	// and terminal persistence run after the policy has decided no more requests.
	original := contextbuild.Input{Text: "late instruction", Attachments: []contextbuild.Attachment{{Path: "gone", Kind: "text", Text: "immutable"}}}
	var result OperationResult
	c.acceptInput(Request{Kind: RequestInput, Generation: r.Generation(), OperationID: id, Steer: true, Input: original}, &result)
	if result.Err != nil || result.Text == "" || len(r.steers) != 0 || len(c.queue) != 1 || !c.queue[0].futureOnly {
		t.Fatal("terminal decision left main steering admission open", len(c.queue), len(r.steers), result)
	}
	if !reflect.DeepEqual(c.queue[0].input.Attachments, original.Attachments) {
		t.Fatal("stale terminal input lost snapshots")
	}
}

func TestCoordinatorMainSteeringBoundIsAtomicAndRestoresOverflow(t *testing.T) {
	r, c, id := admittedMainFixture(t)
	input := contextbuild.Input{Text: "pending guidance"}
	for i := 0; i < maxMainSteeringInputs; i++ {
		var result OperationResult
		c.acceptInput(Request{Kind: RequestInput, Generation: r.Generation(), OperationID: id, Steer: true, Input: input}, &result)
		if result.Err != nil {
			t.Fatalf("small input %d was rejected before count bound: %v", i, result.Err)
		}
	}
	original := contextbuild.Input{Text: "overflow", Attachments: []contextbuild.Attachment{{Path: "gone", Kind: "text", Text: "snapshot"}}}
	var result OperationResult
	c.acceptInput(Request{Kind: RequestInput, Generation: r.Generation(), OperationID: id, Steer: true, Input: original}, &result)
	if result.Err == nil || result.Restored == nil || !reflect.DeepEqual(*result.Restored, original) || len(r.steers) != maxMainSteeringInputs {
		t.Fatal("steering bypassed bound or lost overflow snapshots", len(r.steers), result)
	}
	if _, err := r.CancelSteer(); err != nil {
		t.Fatal(err)
	}
	batch := []contextbuild.Input{{Text: "first batch member"}, {Text: "second batch member"}}
	if err := r.SteerInputs(batch); err == nil || len(r.steers) != maxMainSteeringInputs-1 {
		t.Fatal("overflow batch was partially accepted", len(r.steers), err)
	}
	if r.ctx.Err() != nil {
		t.Fatal("steering backpressure killed healthy session", r.ctx.Err())
	}
}

func TestCoordinatorSteeringPayloadAndQuestionRedirectShareBound(t *testing.T) {
	for _, question := range []bool{false, true} {
		t.Run(map[bool]string{false: "steer", true: "dismissed question"}[question], func(t *testing.T) {
			r, c, id := admittedMainFixture(t)
			if question {
				r.questions.pending = &questionForm{view: QuestionForm{ID: "form", Dismissed: true}, reply: make(chan questionReply, 1)}
			}
			original := contextbuild.Input{Text: strings.Repeat("x", maxMainSteeringBytes+1)}
			var result OperationResult
			c.acceptInput(Request{Kind: RequestInput, Generation: r.Generation(), OperationID: id, Steer: true, Input: original}, &result)
			if result.Err == nil || result.Restored == nil || result.Restored.Text != original.Text || len(r.steers) != 0 {
				t.Fatal("payload overflow bypassed shared retained-input bound", result.Err, len(r.steers))
			}
			if question && (r.questions.pending.resolved || len(r.questions.pending.reply) != 0) {
				t.Fatal("rejected redirect resolved question")
			}
		})
	}
}
