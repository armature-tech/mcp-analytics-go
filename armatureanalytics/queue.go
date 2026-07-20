package armatureanalytics

import (
	"context"
	"fmt"
	"log"
	"sync"
)

const (
	PrivacyQueueCapacity  = 1_000
	PrivacyQueueBatchSize = 20
)

type privacyFinalizer func() []Event

type privacyQueueItem struct {
	finalize privacyFinalizer
	done     chan struct{}
	once     sync.Once
}

func (item *privacyQueueItem) finish() {
	item.once.Do(func() { close(item.done) })
}

type privacyQueue struct {
	mu         sync.Mutex
	pending    []*privacyQueueItem
	running    bool
	accepting  bool
	warnedDrop bool
	signal     chan struct{}

	send       func(context.Context, Batch) error
	batch      func([]Event) Batch
	timeoutCtx func() (context.Context, context.CancelFunc)
	onError    func(error, Batch)
	onWarning  func(string)
	onDrop     func()
}

func newPrivacyQueue(
	send func(context.Context, Batch) error,
	batch func([]Event) Batch,
	timeoutCtx func() (context.Context, context.CancelFunc),
	onError func(error, Batch),
	onWarning func(string),
	onDrop func(),
) *privacyQueue {
	return &privacyQueue{
		accepting:  true,
		signal:     make(chan struct{}),
		send:       send,
		batch:      batch,
		timeoutCtx: timeoutCtx,
		onError:    onError,
		onWarning:  onWarning,
		onDrop:     onDrop,
	}
}

func (q *privacyQueue) enqueue(ctx context.Context, finalize privacyFinalizer, await bool) {
	if q == nil || finalize == nil {
		return
	}
	item := &privacyQueueItem{finalize: finalize, done: make(chan struct{})}
	var dropped *privacyQueueItem
	shouldWarn := false
	q.mu.Lock()
	for await && q.accepting && len(q.pending) >= PrivacyQueueCapacity {
		signal := q.signal
		q.mu.Unlock()
		select {
		case <-signal:
			q.mu.Lock()
		case <-ctx.Done():
			q.noteDrop()
			return
		}
	}
	if !q.accepting {
		q.mu.Unlock()
		q.noteDrop()
		return
	}
	if !await && len(q.pending) >= PrivacyQueueCapacity {
		dropped = q.pending[0]
		copy(q.pending, q.pending[1:])
		q.pending[len(q.pending)-1] = nil
		q.pending = q.pending[:len(q.pending)-1]
		if !q.warnedDrop {
			q.warnedDrop = true
			shouldWarn = true
		}
	}
	q.pending = append(q.pending, item)
	if !q.running {
		q.running = true
		go q.run()
	}
	q.notifyLocked()
	q.mu.Unlock()

	if dropped != nil {
		dropped.finish()
		q.noteDrop()
		if shouldWarn {
			q.warn("privacy queue overflow; dropped oldest candidate (further drops are counted but not warned)")
		}
	}
	if !await {
		return
	}
	select {
	case <-item.done:
	case <-ctx.Done():
	}
}

func (q *privacyQueue) run() {
	for {
		q.mu.Lock()
		if len(q.pending) == 0 {
			q.running = false
			q.notifyLocked()
			q.mu.Unlock()
			return
		}
		count := PrivacyQueueBatchSize
		if len(q.pending) < count {
			count = len(q.pending)
		}
		items := append([]*privacyQueueItem(nil), q.pending[:count]...)
		copy(q.pending, q.pending[count:])
		clear(q.pending[len(q.pending)-count:])
		q.pending = q.pending[:len(q.pending)-count]
		q.notifyLocked()
		q.mu.Unlock()

		events := make([]Event, 0, count)
		for _, item := range items {
			finalized, failed := safelyFinalize(item.finalize)
			if failed {
				q.warn("privacy queue candidate panicked and was dropped")
				continue
			}
			events = append(events, finalized...)
		}
		if len(events) > 0 {
			batch := q.batch(events)
			ctx, cancel := q.timeoutCtx()
			err := q.send(ctx, batch)
			cancel()
			if err != nil && q.onError != nil {
				q.onError(err, batch)
			}
		}
		for _, item := range items {
			item.finish()
		}
		q.mu.Lock()
		q.notifyLocked()
		q.mu.Unlock()
	}
}

func safelyFinalize(finalize privacyFinalizer) (events []Event, failed bool) {
	defer func() {
		if recover() != nil {
			events = nil
			failed = true
		}
	}()
	return finalize(), false
}

func (q *privacyQueue) flush(ctx context.Context) error {
	if q == nil {
		return nil
	}
	for {
		q.mu.Lock()
		if !q.running && len(q.pending) == 0 {
			q.mu.Unlock()
			return nil
		}
		signal := q.signal
		q.mu.Unlock()
		select {
		case <-signal:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (q *privacyQueue) close(ctx context.Context) error {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	q.accepting = false
	q.notifyLocked()
	q.mu.Unlock()
	return q.flush(ctx)
}

func (q *privacyQueue) notifyLocked() {
	close(q.signal)
	q.signal = make(chan struct{})
}

func (q *privacyQueue) noteDrop() {
	if q.onDrop != nil {
		q.onDrop()
	}
}

func (q *privacyQueue) warn(message string) {
	if q.onWarning != nil {
		q.onWarning(message)
		return
	}
	log.Printf("[mcp-analytics] %s", message)
}

func invalidDeliveryError(delivery string) error {
	return fmt.Errorf("armatureanalytics: unsupported delivery mode %q", delivery)
}
