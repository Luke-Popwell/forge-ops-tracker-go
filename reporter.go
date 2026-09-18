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
	defer func() {
		if p := recover(); p != nil {
			r.configuration.Logger.Debugf("report failed: %v", p)
		}
	}()

	if err == nil || !r.configuration.IsEnabled() {
		return
	}

	payload := r.eventBuilder.Build(err, context, user, pcs, breadcrumbs)
	r.deliveryQueue.Push(payload)
}
