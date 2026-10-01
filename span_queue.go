package forgeops

import (
	"sync"
	"time"
)

// SpanQueue is the same shape and reasoning as DeliveryQueue: a small in-process worker goroutine
// plus a bounded channel, so delivering a captured trace never adds latency to the very request it
// was just measuring, the one thing that would be actively counterproductive here (blocking an
// already-slow request even longer just to report that it was slow). One whole trace (a trace id
// plus every span belonging to it) is one queue entry, delivered as one POST, not batched over a
// time window the way PerformanceFlusher is: a trace is already a complete, immediately relevant
// unit the moment a request finishes.
type SpanQueue struct {
	configuration *Configuration
	client        *Client
	queue         chan queuedTrace
	once          sync.Once
}

// queuedTrace is one whole trace, or (with done set) the marker Flush waits on.
type queuedTrace struct {
	trace map[string]any
	done  chan struct{}
}

func NewSpanQueue(configuration *Configuration, client *Client) *SpanQueue {
	size := configuration.QueueSize
	if size < 1 {
		size = 1
	}
	return &SpanQueue{configuration: configuration, client: client, queue: make(chan queuedTrace, size)}
}

// Push enqueues one whole trace, returning false (and dropping it) if the queue is full rather
// than blocking the caller: a burst of several slow requests at once must never stall a request
// that is already slow.
func (q *SpanQueue) Push(trace map[string]any) bool {
	q.once.Do(func() { go q.run() })

	select {
	case q.queue <- queuedTrace{trace: trace}:
		return true
	default:
		q.configuration.Logger.Debugf("span delivery queue full, dropping trace")
		return false
	}
}

// Flush waits until every trace queued before this call has been delivered, at most timeout: the
// same marker approach as DeliveryQueue.Flush.
func (q *SpanQueue) Flush(timeout time.Duration) bool {
	done := make(chan struct{})
	q.once.Do(func() { go q.run() })
	return sendAndWait(q.queue, queuedTrace{done: done}, done, timeout)
}

func (q *SpanQueue) run() {
	for item := range q.queue {
		if item.done != nil {
			close(item.done)
			continue
		}
		q.deliverSafely(item.trace)
	}
}

func (q *SpanQueue) deliverSafely(trace map[string]any) {
	defer func() {
		if r := recover(); r != nil {
			q.configuration.Logger.Debugf("span delivery worker error: %v", r)
		}
	}()
	q.client.DeliverSpans(trace)
}
