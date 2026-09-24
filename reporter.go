package forgeops

// Reporter ties Configuration, EventBuilder, and DeliveryQueue together into the one thing callers
// actually need: report an error. Mirrors gems/forge_ops_tracker's ErrorSubscriber#report: never
// panics back into the caller. An error reporter that can itself crash the host app while
// reporting an error is the worst possible failure mode, so every path here is guarded.
type Reporter struct {
	configuration *Configuration
	eventBuilder  *EventBuilder
	deliveryQueue *DeliveryQueue
}

func NewReporter(configuration *Configuration, eventBuilder *EventBuilder, deliveryQueue *DeliveryQueue) *Reporter {
	return &Reporter{configuration: configuration, eventBuilder: eventBuilder, deliveryQueue: deliveryQueue}
}

func (r *Reporter) Report(err error, context map[string]any, user map[string]any, pcs []uintptr, breadcrumbs []Breadcrumb) {
	r.report(err, context, user, pcs, breadcrumbs, traceFields{})
}

// report is Report plus where the error happened (see traceFieldsForError): trace_id,
// transaction_name and endpoint, each left out when empty, as all three are outside a request.
// Added after EventBuilder.Build has scrubbed the payload: structured fields, never PII-scrubbed,
// the same exemption user gets.
func (r *Reporter) report(err error, context map[string]any, user map[string]any, pcs []uintptr, breadcrumbs []Breadcrumb, trace traceFields) {
	defer func() {
		if p := recover(); p != nil {
			r.configuration.Logger.Debugf("report failed: %v", p)
		}
	}()

	if err == nil || !r.configuration.IsEnabled() {
		return
	}

	payload := r.eventBuilder.Build(err, context, user, pcs, breadcrumbs)
	if trace.transactionName != "" {
		payload["transaction_name"] = trace.transactionName
	}
	if trace.endpoint != "" {
		payload["endpoint"] = trace.endpoint
	}
	if trace.traceID != "" {
		payload["trace_id"] = trace.traceID
	}
	r.deliveryQueue.Push(payload)
}
