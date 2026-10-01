package forgeops

import (
	"sync"
	"time"
)

// DeliveryQueue is a small in-process worker goroutine + buffered channel, so delivery never
// blocks the caller that reported the error and never depends on the host app having any
// particular job backend configured. Ported from
// gems/forge_ops_tracker/lib/forge_ops_tracker/delivery_queue.rb.
//
// The worker goroutine is started lazily, on first push, via sync.Once rather than in
// NewDeliveryQueue: idiomatic lazy init in Go, though for a different reason than the Ruby gem
// and Python client's own lazy start: those exist specifically so a prefork server (Puma,
// Gunicorn) forking worker processes after this module has already loaded doesn't leave an
// eagerly-started thread dead in every forked child. A Go program essentially never forks itself
// at the application level (net/http concurrency is goroutines, not child processes) so that
// specific hazard doesn't apply here; sync.Once is simply the standard, cheap way to defer work
// until it's actually needed.
type DeliveryQueue struct {
	configuration *Configuration
	client        *Client
	queue         chan queuedDelivery
	once          sync.Once
}

// queuedDelivery is one payload plus the Client method that sends it: an event goes to
// Client.Deliver, while a change or change snapshot (see change_tracking.go) goes to its own
// endpoint through the same worker, so none of them ever needs a goroutine of its own.
type queuedDelivery struct {
	payload map[string]any
	deliver func(map[string]any) bool
}

func NewDeliveryQueue(configuration *Configuration, client *Client) *DeliveryQueue {
	size := configuration.QueueSize
	if size < 1 {
		size = 1
	}
	return &DeliveryQueue{
		configuration: configuration,
		client:        client,
		queue:         make(chan queuedDelivery, size),
	}
}

// Push enqueues payload for delivery, returning false (and dropping it) if the queue is already
// full rather than blocking the caller.
func (q *DeliveryQueue) Push(payload map[string]any) bool {
	return q.push(queuedDelivery{payload: payload, deliver: q.client.Deliver})
}

// pushTo is Push for a payload that goes somewhere other than the events endpoint.
func (q *DeliveryQueue) pushTo(deliver func(map[string]any) bool, payload map[string]any) bool {
	return q.push(queuedDelivery{payload: payload, deliver: deliver})
}

func (q *DeliveryQueue) push(item queuedDelivery) bool {
	q.once.Do(func() { go q.run() })

	select {
	case q.queue <- item:
		return true
	default:
		q.configuration.Logger.Debugf("delivery queue full, dropping event")
		return false
	}
}

// Flush waits until everything queued before this call has been delivered (or given up on: a
// failed delivery counts as done, the same as on the worker itself), at most timeout, and reports
// whether it got there in time. It queues a marker behind those payloads and waits for the worker
// to reach it, so the channel's own ordering is what guarantees they all went first, and payloads
// queued after this call never keep it waiting.
func (q *DeliveryQueue) Flush(timeout time.Duration) bool {
	done := make(chan struct{})
	marker := queuedDelivery{deliver: func(map[string]any) bool {
		close(done)
		return true
	}}
	q.once.Do(func() { go q.run() })
	return sendAndWait(q.queue, marker, done, timeout)
}

// sendAndWait queues marker on queue (waiting for room if it's full) and then waits for done to
// close, all within timeout. Shared by DeliveryQueue.Flush and SpanQueue.Flush.
func sendAndWait[T any](queue chan T, marker T, done chan struct{}, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case queue <- marker:
	case <-timer.C:
		return false
	}
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

func (q *DeliveryQueue) run() {
	for item := range q.queue {
		q.deliverSafely(item)
	}
}

// deliverSafely guards a single delivery, not the whole loop: one bad delivery must not kill the
// worker for every event after it. Client.Deliver already turns every failure mode of its own into
// a plain false return rather than a panic; this is a second, redundant layer of safety around it.
func (q *DeliveryQueue) deliverSafely(item queuedDelivery) {
	defer func() {
		if r := recover(); r != nil {
			q.configuration.Logger.Debugf("delivery worker error: %v", r)
		}
	}()
	item.deliver(item.payload)
}
