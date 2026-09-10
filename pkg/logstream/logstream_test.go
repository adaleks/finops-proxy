// Package logstream_test contains black-box unit tests for the pub/sub hub
// behind the live log stream (plan §4.1): fan-out, drop-oldest backpressure,
// unsubscribe semantics and concurrent publish. Every test runs clean under
// `go test -race ./internal/logstream/`.
package logstream_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/adaleks/finops-proxy/pkg/logstream"
)

// testEvent builds a distinguishable LogEvent. Timestamp doubles as a sequence
// marker so tests can assert FIFO order.
func testEvent(n int) logstream.LogEvent {
	return logstream.LogEvent{
		Type:      logstream.TypeRequestComplete,
		Level:     "info",
		Timestamp: int64(n),
		AgentID:   fmt.Sprintf("agent-%d", n),
		Model:     fmt.Sprintf("model-%d", n),
	}
}

// recv reads one event from c within timeout, reporting ok=false when nothing
// arrived (including when the channel is closed).
func recv(t *testing.T, c <-chan logstream.LogEvent, timeout time.Duration) (logstream.LogEvent, bool) {
	t.Helper()
	select {
	case ev, ok := <-c:
		return ev, ok
	case <-time.After(timeout):
		return logstream.LogEvent{}, false
	}
}

func TestHubPublishDeliversToSubscriber(t *testing.T) {
	hub := logstream.NewHub(nil)
	sub := hub.Subscribe(context.Background(), 4)
	defer sub.Close()

	if got := hub.Len(); got != 1 {
		t.Fatalf("Len() after subscribe = %d, want 1", got)
	}

	ev := testEvent(1)
	hub.Publish(ev)

	got, ok := recv(t, sub.C, time.Second)
	if !ok {
		t.Fatal("subscriber did not receive the published event")
	}
	if got != ev {
		t.Fatalf("received %+v, want %+v", got, ev)
	}
}

func TestHubFanoutDeliversToAllSubscribers(t *testing.T) {
	hub := logstream.NewHub(nil)
	const n = 5
	const m = 3
	subs := make([]*logstream.Subscription, n)
	for i := range subs {
		subs[i] = hub.Subscribe(context.Background(), m)
		defer subs[i].Close()
	}
	if got := hub.Len(); got != n {
		t.Fatalf("Len() = %d, want %d", got, n)
	}

	for i := 0; i < m; i++ {
		hub.Publish(testEvent(i))
	}

	for i, s := range subs {
		for j := 0; j < m; j++ {
			got, ok := recv(t, s.C, time.Second)
			if !ok {
				t.Fatalf("subscriber %d: missing event %d", i, j)
			}
			if want := testEvent(j); got != want {
				t.Fatalf("subscriber %d event %d = %+v, want %+v (FIFO)", i, j, got, want)
			}
		}
	}
}

// TestHubDropOldestWhenSubscriberSlow verifies the drop-oldest policy: a slow
// subscriber with a B-event buffer sees the newest buffered window after B+k
// publishes, counts the dropped events, and drains with the correct QueueDepth.
func TestHubDropOldestWhenSubscriberSlow(t *testing.T) {
	hub := logstream.NewHub(nil)

	// Slow subscriber: never reads until after all publishes land.
	slow := hub.Subscribe(context.Background(), 2)
	defer slow.Close()

	const published = 5
	for i := 0; i < published; i++ {
		hub.Publish(testEvent(i))
	}

	// The buffer (capacity 2) holds the newest two events; the oldest three were
	// dropped. QueueDepth is observable before draining.
	if got := slow.QueueDepth(); got != 2 {
		t.Errorf("QueueDepth() before drain = %d, want 2", got)
	}
	if got := slow.Dropped(); got != 3 {
		t.Errorf("Dropped() = %d, want 3", got)
	}

	for i, wantTS := range []int64{3, 4} {
		got, ok := recv(t, slow.C, time.Second)
		if !ok {
			t.Fatalf("slow subscriber: missing buffered event %d", i)
		}
		if got.Timestamp != wantTS {
			t.Fatalf("slow subscriber event %d = %+v, want timestamp %d", i, got, wantTS)
		}
	}
	if got := slow.QueueDepth(); got != 0 {
		t.Errorf("QueueDepth() after drain = %d, want 0", got)
	}
	if _, ok := recv(t, slow.C, 50*time.Millisecond); ok {
		t.Fatal("slow subscriber received more than the buffered window")
	}
}

// TestHubDropOldestDoesNotAffectFastSubscribers verifies backpressure is
// per-subscriber: a healthy subscriber reading concurrently still receives
// every published event in order while a slow one drops.
func TestHubDropOldestDoesNotAffectFastSubscribers(t *testing.T) {
	hub := logstream.NewHub(nil)

	slow := hub.Subscribe(context.Background(), 2)
	defer slow.Close()
	fast := hub.Subscribe(context.Background(), 64)
	defer fast.Close()

	const published = 5
	gotFast := make([]logstream.LogEvent, 0, published)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for len(gotFast) < published {
			select {
			case ev, ok := <-fast.C:
				if !ok {
					return
				}
				gotFast = append(gotFast, ev)
			case <-time.After(2 * time.Second):
				return
			}
		}
	}()

	for i := 0; i < published; i++ {
		hub.Publish(testEvent(i))
	}
	<-done

	if len(gotFast) != published {
		t.Fatalf("fast subscriber received %d events, want %d", len(gotFast), published)
	}
	for i, ev := range gotFast {
		if want := testEvent(i); ev != want {
			t.Fatalf("fast subscriber event %d = %+v, want %+v", i, ev, want)
		}
	}
	if got := fast.Dropped(); got != 0 {
		t.Errorf("fast subscriber Dropped() = %d, want 0", got)
	}
	// The slow subscriber still only saw its buffered window.
	if got := slow.Dropped(); got != 3 {
		t.Errorf("slow subscriber Dropped() = %d, want 3", got)
	}
}

func TestHubUnsubscribeStopsDelivery(t *testing.T) {
	hub := logstream.NewHub(nil)
	sub := hub.Subscribe(context.Background(), 4)
	sub.Close()

	if got := hub.Len(); got != 0 {
		t.Fatalf("Len() after Close = %d, want 0", got)
	}

	hub.Publish(testEvent(1)) // must not panic, must not deliver
	if _, ok := recv(t, sub.C, 50*time.Millisecond); ok {
		t.Fatal("delivery after unsubscribe")
	}
	// The channel is closed: a read yields ok=false, not a zero-value event.
	if _, ok := <-sub.C; ok {
		t.Fatal("subscriber channel must be closed after Close")
	}
}

func TestHubUnsubscribeIdempotent(t *testing.T) {
	hub := logstream.NewHub(nil)
	sub := hub.Subscribe(context.Background(), 2)
	sub.Close()
	sub.Close() // idempotent — no panic, no double-close

	orphan := hub.Subscribe(context.Background(), 2)
	orphan.Close()
	orphan.Close()
	if got := hub.Len(); got != 0 {
		t.Errorf("Len() = %d, want 0", got)
	}
}

func TestHubPublishAfterAllUnsubscribed(t *testing.T) {
	hub := logstream.NewHub(nil)
	sub := hub.Subscribe(context.Background(), 2)
	sub.Close()
	hub.Publish(testEvent(1)) // no subscribers → no-op, no panic
	if got := hub.Len(); got != 0 {
		t.Errorf("Len() = %d, want 0", got)
	}
}

// TestHubPublishNilSubscriptionNoPanic ensures a Publish racing a Close never
// sends on a closed channel: Close holds the hub write lock, which blocks every
// in-flight Publish (holding the read lock) until it has finished fanning out.
func TestHubPublishNilSubscriptionNoPanic(t *testing.T) {
	hub := logstream.NewHub(nil)
	for i := 0; i < 100; i++ {
		sub := hub.Subscribe(context.Background(), 2)
		sub.Close()
	}
	hub.Publish(testEvent(1))
	if got := hub.Len(); got != 0 {
		t.Errorf("Len() = %d, want 0", got)
	}
}

// TestHubConcurrentPublishRace hammers the hub from 8 goroutines while one
// reader drains. A large buffer guarantees no drops, so the reader must see
// exactly producers*perProducer events. Race detector must stay clean.
func TestHubConcurrentPublishRace(t *testing.T) {
	hub := logstream.NewHub(nil)
	sub := hub.Subscribe(context.Background(), 16384)
	defer sub.Close()

	const producers = 8
	const perProducer = 1000
	const total = producers * perProducer

	var wg sync.WaitGroup
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for i := 0; i < perProducer; i++ {
				hub.Publish(testEvent(base + i))
			}
		}(p * perProducer)
	}
	wg.Wait()

	count := 0
	for count < total {
		select {
		case _, ok := <-sub.C:
			if !ok {
				t.Fatal("subscriber channel closed unexpectedly")
			}
			count++
		case <-time.After(2 * time.Second):
			t.Fatalf("received %d events, want %d", count, total)
		}
	}
	if got := sub.Dropped(); got != 0 {
		t.Errorf("Dropped() = %d, want 0 (large buffer, no drops)", got)
	}
}

// TestHubConcurrentSubscribeUnsubscribeRace churns subscribe/close/publish from
// 8 goroutines and asserts the hub stays usable and leak-free. Race detector
// must stay clean.
func TestHubConcurrentSubscribeUnsubscribeRace(t *testing.T) {
	hub := logstream.NewHub(nil)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				sub := hub.Subscribe(context.Background(), 4)
				sub.Close()
				hub.Publish(testEvent(1))
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()

	if got := hub.Len(); got != 0 {
		t.Errorf("Len() after churn = %d, want 0 (all subscribers closed)", got)
	}
	hub.Publish(testEvent(1)) // still usable, no panic
}

// TestHubQueueDepth pins the QueueDepth reporting across a partial drain.
func TestHubQueueDepth(t *testing.T) {
	hub := logstream.NewHub(nil)
	sub := hub.Subscribe(context.Background(), 3)
	defer sub.Close()

	if got := sub.QueueDepth(); got != 0 {
		t.Fatalf("QueueDepth() before publish = %d, want 0", got)
	}
	hub.Publish(testEvent(1))
	hub.Publish(testEvent(2))
	if got := sub.QueueDepth(); got != 2 {
		t.Errorf("QueueDepth() after two publishes = %d, want 2", got)
	}
	if _, ok := recv(t, sub.C, time.Second); !ok {
		t.Fatal("expected to drain the first buffered event")
	}
	if got := sub.QueueDepth(); got != 1 {
		t.Errorf("QueueDepth() after one read = %d, want 1", got)
	}
}
