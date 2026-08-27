package forgeops

import "sync"

// DeliveryQueue is a small in-process worker goroutine + buffered channel, so delivery never
// blocks the caller that reported the error and never depends on the host app having any
// particular job backend configured. Ported from
// gems/forge_ops_tracker/lib/forge_ops_tracker/delivery_queue.rb.
//
// The worker goroutine is started lazily, on first push, via sync.Once rather than in
// NewDeliveryQueue -- idiomatic lazy init in Go, though for a different reason than the Ruby gem
// and Python client's own lazy start: those exist specifically so a prefork server (Puma,
// Gunicorn) forking worker processes after this module has already loaded doesn't leave an
// eagerly-started thread dead in every forked child. A Go program essentially never forks itself
// at the application level -- net/http concurrency is goroutines, not child processes -- so that
// specific hazard doesn't apply here; sync.Once is simply the standard, cheap way to defer work
// until it's actually needed.
type DeliveryQueue struct {
	configuration *Configuration
	client        *Client
	queue         chan map[string]any
	once          sync.Once
}

func NewDeliveryQueue(configuration *Configuration, client *Client) *DeliveryQueue {
	size := configuration.QueueSize
	if size < 1 {
		size = 1
	}
	return &DeliveryQueue{
		configuration: configuration,
		client:        client,
		queue:         make(chan map[string]any, size),
	}
}

// Push enqueues payload for delivery, returning false (and dropping it) if the queue is already
// full rather than blocking the caller.
func (q *DeliveryQueue) Push(payload map[string]any) bool {
	q.once.Do(func() { go q.run() })

	select {
	case q.queue <- payload:
		return true
	default:
		q.configuration.Logger.Debugf("delivery queue full, dropping event")
		return false
	}
}

func (q *DeliveryQueue) run() {
	for payload := range q.queue {
		q.deliverSafely(payload)
	}
}

// deliverSafely guards a single delivery, not the whole loop -- one bad delivery must not kill the
// worker for every event after it. Client.Deliver already turns every failure mode of its own into
// a plain false return rather than a panic; this is a second, redundant layer of safety around it.
func (q *DeliveryQueue) deliverSafely(payload map[string]any) {
	defer func() {
		if r := recover(); r != nil {
			q.configuration.Logger.Debugf("delivery worker error: %v", r)
		}
	}()
	q.client.Deliver(payload)
}
