package armatureanalytics

import (
	"context"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func queueTestEvent(id int) Event {
	return Event{EventID: strconv.Itoa(id), Kind: KindToolCall}
}

func newTestPrivacyQueue(
	send func(context.Context, Batch) error,
	onWarning func(string),
	onDrop func(),
) *privacyQueue {
	return newPrivacyQueue(
		send,
		func(events []Event) Batch { return Batch{SchemaVersion: SchemaVersion, Events: events} },
		func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) },
		nil,
		onWarning,
		onDrop,
	)
}

func TestPrivacyQueueFIFOAndNaturalBatching(t *testing.T) {
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var mu sync.Mutex
	var batches [][]string
	sends := 0
	queue := newTestPrivacyQueue(func(_ context.Context, batch Batch) error {
		ids := make([]string, len(batch.Events))
		for i, event := range batch.Events {
			ids[i] = event.EventID
		}
		mu.Lock()
		batches = append(batches, ids)
		sends++
		current := sends
		mu.Unlock()
		if current == 1 {
			close(firstStarted)
			<-releaseFirst
		}
		return nil
	}, nil, nil)

	queue.enqueue(context.Background(), func() []Event { return []Event{queueTestEvent(0)} }, false)
	<-firstStarted
	queue.enqueue(context.Background(), func() []Event { return []Event{queueTestEvent(1)} }, false)
	queue.enqueue(context.Background(), func() []Event { return []Event{queueTestEvent(2)} }, false)
	close(releaseFirst)
	if err := queue.flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(batches, [][]string{{"0"}, {"1", "2"}}) {
		t.Fatalf("batches = %#v", batches)
	}
}

func TestPrivacyQueueOverflowDropsOldestPending(t *testing.T) {
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var idsMu sync.Mutex
	var ids []string
	var drops atomic.Int64
	var warnings atomic.Int64
	queue := newTestPrivacyQueue(func(_ context.Context, batch Batch) error {
		idsMu.Lock()
		for _, event := range batch.Events {
			ids = append(ids, event.EventID)
		}
		first := len(ids) == 1
		idsMu.Unlock()
		if first {
			close(firstStarted)
			<-releaseFirst
		}
		return nil
	}, func(string) { warnings.Add(1) }, func() { drops.Add(1) })

	queue.enqueue(context.Background(), func() []Event { return []Event{queueTestEvent(0)} }, false)
	<-firstStarted
	for id := 1; id <= PrivacyQueueCapacity+2; id++ {
		captured := id
		queue.enqueue(context.Background(), func() []Event { return []Event{queueTestEvent(captured)} }, false)
	}
	close(releaseFirst)
	if err := queue.flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	idsMu.Lock()
	defer idsMu.Unlock()
	if len(ids) != PrivacyQueueCapacity+1 || ids[0] != "0" || ids[1] != "3" || ids[len(ids)-1] != strconv.Itoa(PrivacyQueueCapacity+2) {
		t.Fatalf("overflow FIFO ids: len=%d first=%v last=%v", len(ids), ids[:min(3, len(ids))], ids[len(ids)-1:])
	}
	if drops.Load() != 2 || warnings.Load() != 1 {
		t.Fatalf("drops=%d warnings=%d, want 2/1", drops.Load(), warnings.Load())
	}
}

func TestPrivacyQueueAwaitWaitsForExport(t *testing.T) {
	sendStarted := make(chan struct{})
	releaseSend := make(chan struct{})
	queue := newTestPrivacyQueue(func(context.Context, Batch) error {
		close(sendStarted)
		<-releaseSend
		return nil
	}, nil, nil)
	done := make(chan struct{})
	go func() {
		queue.enqueue(context.Background(), func() []Event { return []Event{queueTestEvent(1)} }, true)
		close(done)
	}()
	<-sendStarted
	select {
	case <-done:
		t.Fatal("await enqueue returned before export")
	default:
	}
	close(releaseSend)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("await enqueue did not finish after export")
	}
}

func TestPrivacyQueueAwaitBackpressuresInsteadOfDropping(t *testing.T) {
	var drops atomic.Int64
	queue := newTestPrivacyQueue(func(context.Context, Batch) error { return nil }, nil, func() {
		drops.Add(1)
	})

	queue.mu.Lock()
	queue.running = true // Keep the synthetic full queue from starting a worker.
	for id := 0; id < PrivacyQueueCapacity; id++ {
		queue.pending = append(queue.pending, &privacyQueueItem{
			finalize: func() []Event { return nil },
			done:     make(chan struct{}),
		})
	}
	queue.mu.Unlock()

	returned := make(chan struct{})
	go func() {
		queue.enqueue(context.Background(), func() []Event { return []Event{queueTestEvent(1)} }, true)
		close(returned)
	}()

	select {
	case <-returned:
		t.Fatal("await enqueue returned while the queue was full")
	case <-time.After(20 * time.Millisecond):
	}
	if drops.Load() != 0 {
		t.Fatalf("await overflow drops = %d, want 0", drops.Load())
	}

	queue.mu.Lock()
	queue.pending = queue.pending[1:]
	queue.notifyLocked()
	queue.mu.Unlock()

	var enqueued *privacyQueueItem
	deadline := time.After(time.Second)
	for enqueued == nil {
		queue.mu.Lock()
		if len(queue.pending) == PrivacyQueueCapacity {
			enqueued = queue.pending[len(queue.pending)-1]
		}
		queue.mu.Unlock()
		if enqueued == nil {
			select {
			case <-deadline:
				t.Fatal("await candidate was not enqueued after capacity became available")
			case <-time.After(time.Millisecond):
			}
		}
	}

	enqueued.finish()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("await enqueue did not return after its candidate completed")
	}
}
