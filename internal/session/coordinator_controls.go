package session

import (
	"errors"
	"fmt"
)

// dispatchControl transfers potentially blocking effects to one bounded worker.
// A pending history command fences later effects and starts only after earlier
// effects have settled, so an old BTW/login admission cannot cross a reset.
func (c *SessionCoordinator) dispatchControl(r Request) {
	if c.pendingCommand != nil || c.changingSession || c.command && r.Kind == RequestBTW || c.active != nil && r.Kind == RequestLogin {
		c.result(OperationResult{RequestID: r.requestID, Kind: r.Kind, Generation: r.Generation, Err: errors.New("wait for the current history command")})
		return
	}
	select {
	case c.effects <- r:
		c.effectCount++
		c.project()
	default:
		c.result(OperationResult{RequestID: r.requestID, Kind: r.Kind, Generation: r.Generation, Err: ErrCoordinatorFull})
	}
}
func (c *SessionCoordinator) controlLoop() {
	defer close(c.effectsDone)
	for {
		select {
		case <-c.stop:
			return
		case r := <-c.effects:
			result := c.executeControl(r)
			select {
			case c.effectResults <- result:
			case <-c.stop:
				return
			}
		}
	}
}
func (c *SessionCoordinator) executeControl(r Request) OperationResult {
	result := OperationResult{RequestID: r.requestID, Kind: r.Kind, OperationID: r.OperationID, Generation: r.Generation}
	if c.effectCtx.Err() != nil {
		result.Err = c.effectCtx.Err()
		return result
	}
	if r.Generation != c.runtime.Generation() {
		result.Err = errors.New("session changed before control admission")
		return result
	}
	switch r.Kind {
	case RequestCancelSteer:
		input, err := c.runtime.CancelSteer()
		result.Err = err
		if err == nil {
			result.Restored = &input
			result.Text = "Cancelled pending input · restored to composer"
		}
	case RequestModel:
		if r.Selection == nil {
			result.Err = errors.New("main model selection is missing")
		} else {
			result.Err = c.runtime.RequestModel(*r.Selection)
			if result.Err == nil {
				result.Text = "Model selected for next tool boundary · " + r.Selection.Model.ID + " · " + r.Selection.Variant
			}
		}
	case RequestEasyModel:
		result.Err = c.runtime.RequestEasyModel(r.Selection)
		if result.Err == nil {
			if r.Selection == nil {
				result.Text = "Easy model disabled · using main model"
			} else {
				result.Text = "Easy model selected · " + r.Selection.Model.ID + " · " + r.Selection.Variant
			}
		}
	case RequestBTW:
		_, result.Err = c.runtime.StartBTW(r.Command)
	case RequestAnswer:
		result.Err = c.runtime.AnswerQuestion(r.ID, r.Answers)
	case RequestDismissQuestion:
		result.Err = c.runtime.DismissQuestion(r.ID)
	case RequestReopenQuestion:
		result.Err = c.runtime.ReopenQuestion(r.ID)
	case RequestConfirmImage:
		result.Err = c.runtime.ConfirmImage(r.ID, r.Point)
	case RequestBackground:
		if r.ID != "" {
			_, result.Err = c.runtime.Jobs.Promote("main", r.ID)
		} else {
			for _, job := range c.runtime.Jobs.Foreground("main") {
				_, err := c.runtime.Jobs.Promote("main", job.ID)
				result.Err = errors.Join(result.Err, err)
			}
		}
	case RequestReconnect:
		if c.controlEffects.Reconnect == nil {
			result.Err = errors.New("provider does not support reconnecting")
		} else {
			result.Err = c.controlEffects.Reconnect()
			if result.Err == nil {
				result.Text = "Provider connections reset; active requests use normal retry/recovery. Connectivity has not been verified."
			}
		}
	case RequestLogin:
		if c.controlEffects.Login == nil {
			result.Err = errors.New("login is unavailable")
			break
		}
		result.Err = c.runtime.ClearEasyModelForLogin()
		if result.Err == nil {
			result.Models, result.Err = c.controlEffects.Login(c.effectCtx)
		}
		if result.Err == nil {
			result.Text = "Login complete; use /model to select from the current account's catalog"
		} else {
			result.Models = nil
		}
	default:
		result.Err = fmt.Errorf("unsupported session control %q", r.Kind)
	}
	return result
}
func (c *SessionCoordinator) settleControl(result OperationResult) {
	if result.Kind != RequestInterrupt {
		c.effectCount--
	} else {
		c.requestsPending.Add(-1)
	}
	if result.Err == nil {
		switch result.Kind {
		case RequestModel, RequestReconnect:
			c.paused = false
		}
	}
	if result.Kind == RequestModel && result.Err == nil && c.active == nil {
		// Plain control acknowledgment fences idle model application. A following
		// /quit cannot discard the accepted switch's runtime status publication.
		if event, err := c.runtime.ApplyModel(""); err != nil {
			result.Err = err
		} else if event.Kind != "" {
			c.runtime.emit(event)
		}
	}
	c.result(result)
	c.project()
}

// interruptLoop is deliberately independent of blocking admission/effect work.
// Identity check and cancellation share the execution registry gate: a stale
// request cannot cancel a replacement execution, even in the same generation.
func (c *SessionCoordinator) interruptLoop() {
	defer close(c.interruptsDone)
	for {
		select {
		case <-c.stop:
			return
		case r := <-c.interrupts:
			c.interruptObserved(r)
			result := OperationResult{RequestID: r.requestID, Kind: RequestInterrupt, OperationID: r.OperationID, Generation: r.Generation}
			select {
			case c.effectResults <- result:
			case <-c.stop:
				return
			}
		}
	}
}

func (c *SessionCoordinator) interruptObserved(request Request) {
	c.executions.mu.Lock()
	defer c.executions.mu.Unlock()
	id := c.executions.actors["main"]
	e, ok := c.executions.active[id]
	if ok && e.ID == request.OperationID && e.Generation == request.Generation {
		c.executions.interrupted[id] = true
		c.runtime.cancelForeground(errUserInterrupted)
	}
}
