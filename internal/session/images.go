package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"ttc/internal/prompts"

	"ttc/internal/assets"
	"ttc/internal/llm"
	"ttc/internal/tool"
)

// ImageSnapshot identifies immutable source pixels in a private history asset.
// Width/Height and confirmed coordinates refer to original top-left pixel axes.
type ImageSnapshot struct {
	ID           string `json:"image_id"`
	Path         string `json:"path"`
	Snapshot     string `json:"snapshot"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	ClickPending bool   `json:"click_pending"`
	Actor        string `json:"actor"`
}
type pendingImage struct {
	view  ImageSnapshot
	reply chan llm.Message
}
type imageInteractions struct {
	mu           sync.Mutex
	enabled      bool
	pending      map[string]pendingImage
	childReplies map[string]chan llm.Message
	actors       map[string]string // Reserved from admission through child reply consumption.
}

// EnableImageClicks sets frontend capabilities; plain/unsupported terminals reject
// requested interactions before creating a pending handle.
func (r *Runtime) EnableImageClicks(enabled bool) {
	r.images.mu.Lock()
	r.images.enabled = enabled
	r.images.mu.Unlock()
}

// ImageClickPending reports only live state, never a persisted result's old flag.
func (r *Runtime) ImageClickPending(id string) bool {
	r.images.mu.Lock()
	defer r.images.mu.Unlock()
	_, ok := r.images.pending[id]
	return ok
}

func (r *Runtime) addImageTool() {
	r.images.mu.Lock()
	r.images.pending = map[string]pendingImage{}
	r.images.childReplies = map[string]chan llm.Message{}
	r.images.actors = map[string]string{}
	r.images.mu.Unlock()
	type args struct {
		Path  string `json:"path"`
		Click bool   `json:"request_click,omitempty"`
	}
	tool.Register(r.Tools, "image_show", prompts.ToolDescription("image_show"), map[string]any{"path": tool.Property("string"), "request_click": tool.Property("boolean")}, []string{"path"}, func(a args) error { return tool.Required("path", a.Path) }, func(ctx context.Context, x tool.Execution, a args) (any, error) {
		r.images.mu.Lock()
		if a.Click {
			if !r.images.enabled {
				r.images.mu.Unlock()
				return nil, tool.Fail("unsupported_interaction", "image clicks require the Kitty graphics/mouse TUI; omit request_click to display without requesting a point")
			}
			if r.images.actors[x.Actor] != "" {
				r.images.mu.Unlock()
				return nil, tool.Fail("click_already_pending", "an image selection is pending or its reply is unconsumed; wait for and process that reply before requesting another click")
			}
			r.images.actors[x.Actor] = x.CallID
		}
		r.images.mu.Unlock()
		published := false
		defer func() {
			if a.Click && !published {
				r.clearActorImage(x.Actor, x.CallID)
			}
		}()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		path := r.Workspace.Path(a.Path)
		b, m, err := assets.Read(path)
		if err != nil {
			return nil, err
		}
		snapshot, err := r.Store.Artifact(x.SessionID, "images", b)
		if err != nil {
			return nil, err
		}
		v := ImageSnapshot{ID: x.CallID, Path: path, Snapshot: snapshot, Width: m.Bounds().Dx(), Height: m.Bounds().Dy(), ClickPending: a.Click, Actor: x.Actor}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if a.Click {
			r.images.mu.Lock()
			if r.images.actors[x.Actor] != x.CallID {
				r.images.mu.Unlock()
				return nil, context.Canceled
			}
			reply := make(chan llm.Message, 1)
			r.images.pending[v.ID] = pendingImage{v, reply}
			if x.Actor != "main" {
				r.images.childReplies[x.Actor] = reply
			}
			published = true
			r.images.mu.Unlock()
		}
		r.routeMu.RLock()
		r.emit(Event{Kind: "image", Actor: x.Actor, CallID: x.CallID, Image: &v, SessionID: r.Current()})
		r.routeMu.RUnlock()
		return v, nil
	})
}

// ImageCard links a live request to its original inspectable history entry.
type ImageCard struct {
	Snapshot ImageSnapshot
	EntryID  int64
}

// PendingImages returns copied live cards for pinning during continuation replay.
// It never restores interactions from saved history; at most one exists per actor.
func (r *Runtime) PendingImages() ([]ImageCard, error) {
	r.images.mu.Lock()
	var views []ImageSnapshot
	for _, p := range r.images.pending {
		views = append(views, p.view)
	}
	r.images.mu.Unlock()
	sort.Slice(views, func(i, j int) bool { return views[i].ID < views[j].ID })
	var cards []ImageCard
	for _, v := range views {
		var id sql.NullInt64
		err := r.Store.DB.QueryRow("SELECT coalesce((SELECT entry_id FROM tool_records WHERE call_id=?),(SELECT id FROM entries WHERE kind='tool_call' AND json_extract(content_json,'$.call_id')=? ORDER BY id DESC LIMIT 1))", v.ID, v.ID).Scan(&id)
		if err != nil {
			return nil, err
		}
		if id.Valid {
			cards = append(cards, ImageCard{v, id.Int64})
		}
	}
	return cards, nil
}

// ConfirmImage completes exactly one live request. nil coordinates cancel it;
// non-nil coordinates are source pixels, with cell precision from terminal input.
func (r *Runtime) ConfirmImage(id string, point *[2]int) error {
	r.images.mu.Lock()
	p, ok := r.images.pending[id]
	if !ok {
		r.images.mu.Unlock()
		return errors.New("image click is not pending")
	}
	v := p.view
	content := map[string]any{"type": "image_click_cancelled", "image_id": id}
	if point != nil {
		if point[0] < 0 || point[0] >= v.Width || point[1] < 0 || point[1] >= v.Height {
			r.images.mu.Unlock()
			return fmt.Errorf("image coordinate outside %dx%d", v.Width, v.Height)
		}
		content = map[string]any{"type": "image_click", "image_id": id, "x": point[0], "y": point[1], "width": v.Width, "height": v.Height, "precision": "cell"}
	}
	b, err := json.Marshal(content)
	if err != nil {
		r.images.mu.Unlock()
		return err
	}
	m := llm.Message{Role: "user", Content: string(b), Runtime: true}
	delete(r.images.pending, id)
	if v.Actor != "main" {
		p.reply <- m
		r.images.mu.Unlock()
		return nil
	}
	delete(r.images.actors, v.Actor)
	r.images.mu.Unlock()
	return r.queueNotification(m.Content)
}

func (r *Runtime) childImageReply(ctx context.Context, actor string, wait bool) (*llm.Message, error) {
	r.images.mu.Lock()
	ch := r.images.childReplies[actor]
	r.images.mu.Unlock()
	if ch == nil {
		return nil, nil
	}
	var m llm.Message
	if wait {
		select {
		case m = <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	} else {
		select {
		case m = <-ch:
		default:
			return nil, nil
		}
	}
	r.images.mu.Lock()
	delete(r.images.childReplies, actor)
	delete(r.images.actors, actor)
	r.images.mu.Unlock()
	return &m, nil
}

func (r *Runtime) clearImages() {
	r.images.mu.Lock()
	r.images.pending = map[string]pendingImage{}
	r.images.childReplies = map[string]chan llm.Message{}
	r.images.actors = map[string]string{}
	r.images.mu.Unlock()
}

// clearActorImage releases a failed admission or all interactions on child exit.
// A nonempty id prevents an old admission from releasing a newer actor request.
func (r *Runtime) clearActorImage(actor, id string) {
	r.images.mu.Lock()
	defer r.images.mu.Unlock()
	if id != "" && r.images.actors[actor] != id {
		return
	}
	for key, p := range r.images.pending {
		if p.view.Actor == actor {
			delete(r.images.pending, key)
		}
	}
	delete(r.images.actors, actor)
	delete(r.images.childReplies, actor)
}
