package catalog

import (
	"context"
	"errors"
	"fmt"
	"time"

	"ttc/internal/llm"
)

// Source fetches metadata without owning cache storage or application policy.
type Source interface {
	Models(context.Context) ([]llm.ModelInfo, error)
}

// Scope identifies a catalog. Endpoint and Account are raw, in-memory values;
// only their SHA-256 hashes are persisted. Version identifies the source format.
// Account may be empty for sources without authentication.
type Scope struct{ Provider, Endpoint, Account, Version string }

// Binding captures a source and its identity for one operation. Guard, when
// supplied, must validate that identity and hold its synchronization through
// commit; it covers preparation, cache replacement and update publication.
// Validate adds provider-specific metadata invariants to generic validation.
type Binding struct {
	Scope    Scope
	Source   Source
	Guard    func(context.Context, func() error) error
	Validate func([]llm.ModelInfo) error
}

// Config supplies discovery dependencies. Empty CachePath disables disk I/O.
// Timeout defaults to 30 seconds when zero; negative values are invalid. Bind
// must be context-aware, and every Source must honor cancellation for Close to
// join ongoing work. Zero Policy selects DefaultPolicy.
type Config struct {
	Bind      func(context.Context) (Binding, error)
	CachePath string
	Timeout   time.Duration
	Policy    Policy
}

// Update reports one background refresh result. Failure leaves the consumer's
// prior choices unchanged. Models are an immutable, independently owned snapshot.
type Update struct {
	Models []llm.ModelSpec
	Err    error
}

// Manager owns discovery and workers. Initial and Notice are immutable startup
// values, not current choices. Updates stays open until Close, including after a
// worker finishes. Refresh returns a new snapshot directly, not through Updates.
type Manager struct {
	Initial      []llm.ModelSpec
	Notice       string
	Updates      <-chan Update
	config       Config
	ctx          context.Context
	cancel       context.CancelFunc
	updates      chan Update
	opGate       chan struct{} // Serializes lifecycle operations, never held by workers.
	workerCancel context.CancelFunc
	workerDone   <-chan struct{}
}

// Open uses a valid scoped cache immediately and starts one bounded background
// refresh. Missing/incompatible caches require synchronous discovery. Corruption
// is noted after replacement; operational errors fail startup explicitly.
func Open(ctx context.Context, c Config) (*Manager, error) {
	if c.Bind == nil {
		return nil, errors.New("catalog Bind is required")
	}
	if c.Timeout == 0 {
		c.Timeout = 30 * time.Second
	}
	if c.Timeout < 0 {
		return nil, errors.New("catalog Timeout must be positive")
	}
	if c.Policy == (Policy{}) {
		c.Policy = DefaultPolicy()
	}
	if err := c.Policy.validate(); err != nil {
		return nil, err
	}
	root, cancel := context.WithCancel(ctx)
	updates := make(chan Update, 1)
	m := &Manager{config: c, ctx: root, cancel: cancel, updates: updates, Updates: updates, opGate: make(chan struct{}, 1)}
	opCtx, stop := context.WithTimeout(root, c.Timeout)
	defer stop()
	b, err := m.bind(opCtx)
	if err != nil {
		cancel()
		return nil, err
	}
	raw, err := readCache(opCtx, c.CachePath, b)
	if errors.Is(err, ErrInvalidCache) {
		m.Notice = fmt.Sprintf("Ignoring invalid model catalog cache; fetched fresh metadata: %v", err)
		raw = nil
		err = nil
	}
	if err != nil {
		cancel()
		return nil, fmt.Errorf("read model catalog cache: %w", err)
	}
	if len(raw) > 0 {
		err = m.guarded(opCtx, b, func() error { var e error; m.Initial, e = prepare(raw, c.Policy); return e })
		if err != nil {
			cancel()
			return nil, err
		}
		m.startWorker(b)
	} else {
		m.Initial, err = m.fetch(opCtx, b, nil)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("fetch model catalog: %w", err)
		}
	}
	return m, nil
}

func (m *Manager) bind(ctx context.Context) (Binding, error) {
	b, err := m.config.Bind(ctx)
	if err != nil {
		return b, fmt.Errorf("bind model catalog: %w", err)
	}
	if b.Source == nil {
		return b, errors.New("catalog source is required")
	}
	if err := llm.ValidateChoiceText("provider", b.Scope.Provider, llm.MaxProviderIDBytes); err != nil {
		return b, err
	}
	if err := llm.ValidateChoiceText("catalog scope version", b.Scope.Version, llm.MaxModelIDBytes); err != nil {
		return b, err
	}
	return b, ctx.Err()
}
func (m *Manager) guarded(ctx context.Context, b Binding, commit func() error) error {
	checked := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return commit()
	}
	if b.Guard != nil {
		return b.Guard(ctx, checked)
	}
	return checked()
}
func (m *Manager) fetch(ctx context.Context, b Binding, publish func([]llm.ModelSpec)) ([]llm.ModelSpec, error) {
	raw, err := b.Source.Models(ctx)
	if err != nil {
		return nil, err
	}
	if err := validate(raw, b.Validate); err != nil {
		return nil, fmt.Errorf("invalid model metadata: %w", err)
	}
	var models []llm.ModelSpec
	err = m.guarded(ctx, b, func() error {
		var err error
		models, err = prepare(raw, m.config.Policy)
		if err != nil {
			return err
		}
		if err := saveCache(ctx, m.config.CachePath, b, raw); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if publish != nil {
			publish(models)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return models, nil
}
func (m *Manager) startWorker(b Binding) {
	ctx, cancel := context.WithTimeout(m.ctx, m.config.Timeout)
	done := make(chan struct{})
	m.workerCancel = cancel
	m.workerDone = done
	go func() {
		defer close(done)
		defer cancel()
		_, err := m.fetch(ctx, b, func(models []llm.ModelSpec) { m.updates <- Update{Models: models} })
		// Cancellation during Close/Refresh must never leave stale delivery behind.
		if err != nil && !errors.Is(ctx.Err(), context.Canceled) {
			m.updates <- Update{Err: err}
		}
	}()
}
func (m *Manager) stopWorker() {
	if m.workerCancel != nil {
		m.workerCancel()
		<-m.workerDone
		m.workerCancel = nil
		m.workerDone = nil
	}
}
func (m *Manager) drain() {
	select {
	case <-m.updates:
	default:
	}
}

// Invalidate cancels and joins background discovery and discards its buffered
// delivery before an external identity change, including failed authorization.
// It leaves the manager open for Refresh and is safe concurrently with Close.
func (m *Manager) Invalidate() {
	m.opGate <- struct{}{}
	defer func() { <-m.opGate }()
	m.stopWorker()
	m.drain()
}

// Refresh cancels and joins the old worker, discards its buffered update, binds
// the current identity, then synchronously fetches, prepares and stores choices.
// It is safe concurrently with Refresh and Close. Failure does not mutate
// Initial or publish a replacement. Waiting for another lifecycle operation
// honors caller and manager cancellation. Close cancels even a synchronous fetch.
func (m *Manager) Refresh(ctx context.Context) ([]llm.ModelSpec, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-m.ctx.Done():
		return nil, m.ctx.Err()
	case m.opGate <- struct{}{}:
	}
	defer func() { <-m.opGate }()
	// A ready gate can win the select even when cancellation is already ready.
	// Check again before canceling a worker or invoking discovery dependencies.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := m.ctx.Err(); err != nil {
		return nil, err
	}
	m.stopWorker()
	m.drain()
	request, cancel := context.WithTimeout(ctx, m.config.Timeout)
	defer cancel()
	stop := context.AfterFunc(m.ctx, cancel)
	defer stop()
	if err := m.ctx.Err(); err != nil {
		return nil, err
	}
	b, err := m.bind(request)
	if err != nil {
		return nil, err
	}
	return m.fetch(request, b, nil)
}

// Close cancels and joins all work and closes Updates. It is idempotent and
// safe concurrently with Refresh or another Close; workers never close Updates.
func (m *Manager) Close() {
	// CancelFunc is concurrency-safe and idempotent; cancel before waiting so
	// synchronous Refresh can finish and release the lifecycle gate.
	m.cancel()
	// Shutdown must join the active operation even though m.ctx is canceled.
	m.opGate <- struct{}{}
	defer func() { <-m.opGate }()
	m.stopWorker()
	// Clearing the channel prevents canceled account results being observed after
	// shutdown; it is closed only once, under lifecycle serialization.
	if m.updates != nil {
		m.drain()
		close(m.updates)
		m.updates = nil
	}
}
