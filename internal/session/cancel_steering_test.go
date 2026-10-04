package session

import (
	"context"
	"reflect"
	"sync"
	"testing"

	contextbuild "ttc/internal/context"
	"ttc/internal/provider"
)

func enableCancelTestSteering(r *Runtime) {
	r.mu.Lock()
	r.activeTurn = "cancel-test"
	_, r.activeCancel = context.WithCancel(r.ctx)
	r.mu.Unlock()
}

func TestSteeringPreviewIsBoundedAndOwnsTexts(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	for _, text := range []string{"first", "second", "third"} {
		r.steers = append(r.steers, contextbuild.Input{Text: text})
	}
	for _, limit := range []int{-1, 0, 1, 2, 10} {
		count, texts := r.SteeringPreview(limit)
		if count != 3 || len(texts) != min(max(0, limit), 3) {
			t.Fatal("preview lost count or exceeded limit", count, texts)
		}
		if len(texts) > 0 {
			if texts[0] != "first" {
				t.Fatal("preview changed order", texts)
			}
			texts[0] = "mutated"
		}
	}
}

func TestAbsentQuestionRedirectDoesNotExpandAttachments(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	input := contextbuild.Input{Attachments: make([]contextbuild.Attachment, 100)}
	for i := range input.Attachments {
		input.Attachments[i] = contextbuild.Attachment{Kind: "text", Path: "snapshot.txt", Text: "immutable snapshot"}
	}
	if allocations := testing.AllocsPerRun(20, func() {
		accepted, err := r.RedirectDismissedQuestion(input)
		if accepted || err != nil {
			t.Fatal(accepted, err)
		}
	}); allocations != 0 {
		t.Fatal("ineligible redirect expanded input", allocations)
	}
}

func TestSteeringInputValidation(t *testing.T) {
	for _, input := range []contextbuild.Input{
		{}, {Text: string([]byte{0xff})},
		{Attachments: []contextbuild.Attachment{{Path: string([]byte{0xff})}}},
		{Attachments: []contextbuild.Attachment{{Kind: "text", Text: string([]byte{0xff})}}},
	} {
		if err := validateSteeringInput(input); err == nil {
			t.Fatal("invalid authored input accepted", input)
		}
	}
	if err := validateSteeringInput(contextbuild.Input{Attachments: []contextbuild.Attachment{{Kind: "text", Path: "snapshot", Text: "saved"}}}); err != nil {
		t.Fatal("attachment-only input rejected", err)
	}
}

func TestCancelSteerRestoresNewestOriginalInput(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "initial")
	enableCancelTestSteering(r)
	older := contextbuild.Input{Text: "older"}
	latest := contextbuild.Input{Text: "  original\n\tλ text  ", Attachments: []contextbuild.Attachment{
		{Path: "/snapshot/text", Kind: "text", Text: "saved contents", Truncated: true},
		{Path: "/snapshot/directory", Kind: "directory", Text: "a\nb"},
		{Path: "/snapshot/image", Kind: "image", Image: &provider.BinaryFile{Path: "/snapshot/image", DataURL: "data:image/png;base64,c25hcHNob3Q="}},
	}}
	for _, input := range []contextbuild.Input{older, latest} {
		if err := r.Steer(input); err != nil {
			t.Fatal(err)
		}
	}
	restored, err := r.CancelSteer()
	if err != nil || !reflect.DeepEqual(restored, latest) {
		t.Fatal("original input was not restored", restored, err)
	}
	if count, pending := r.SteeringPreview(1); count != 1 || pending[0] != "older" {
		t.Fatal("older steering lost", pending)
	}
	// Cancelling also works after inference is idle, before another admission.
	r.mu.Lock()
	r.activeTurn = ""
	r.activeCancel = nil
	r.mu.Unlock()
	if restored, err = r.CancelSteer(); err != nil || !reflect.DeepEqual(restored, older) {
		t.Fatal(restored, err)
	}
	if _, err := r.CancelSteer(); err == nil {
		t.Fatal("empty cancellation succeeded")
	}
	var checkpoints, human int
	if err := r.Store.DB.QueryRow("SELECT count(*) FROM turns WHERE trigger='steer'").Scan(&checkpoints); err != nil {
		t.Fatal(err)
	}
	if err := r.Store.DB.QueryRow("SELECT count(*) FROM entries WHERE role='user' AND model_visible=1").Scan(&human); err != nil {
		t.Fatal(err)
	}
	if checkpoints != 0 || human != 1 {
		t.Fatal("cancelled input became durable", checkpoints, human)
	}
}

func TestCancelSteerCannotRemoveAdmittedInput(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "initial")
	enableCancelTestSteering(r)
	if err := r.Steer(contextbuild.Input{Text: "admitted"}); err != nil {
		t.Fatal(err)
	}
	admitted, _, err := r.admitMain(context.Background(), "", r.selection)
	if err != nil || len(admitted.SteerEntries) != 1 {
		t.Fatal(admitted, err)
	}
	if _, err := r.CancelSteer(); err == nil {
		t.Fatal("admitted instruction was cancelled")
	}
	found := false
	for _, message := range admitted.Messages {
		found = found || message.Role == "user" && !message.Runtime && message.Content == "admitted"
	}
	if !found {
		t.Fatal("admitted instruction disappeared", admitted.Messages)
	}
}

func TestCancelSteerAdmissionRaceHasExactlyOneWinner(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "initial")
	enableCancelTestSteering(r)
	for i := 0; i < 30; i++ {
		input := contextbuild.Input{Text: "racing instruction"}
		if err := r.Steer(input); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var restored contextbuild.Input
		var cancelErr, admissionErr error
		var admittedSteers int
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			restored, cancelErr = r.CancelSteer()
		}()
		go func() {
			defer wg.Done()
			<-start
			admitted, _, err := r.admitMain(context.Background(), "", r.selection)
			admissionErr, admittedSteers = err, len(admitted.SteerEntries)
		}()
		close(start)
		wg.Wait()
		if admissionErr != nil {
			t.Fatal(admissionErr)
		}
		if cancelErr == nil {
			if !reflect.DeepEqual(restored, input) || admittedSteers != 0 {
				t.Fatal("cancelled instruction was also admitted", restored, admittedSteers)
			}
		} else if admittedSteers != 1 {
			t.Fatal("instruction neither cancelled nor admitted", cancelErr, admittedSteers)
		}
		if count, _ := r.SteeringPreview(0); count != 0 {
			t.Fatal("race left pending steering")
		}
	}
}
