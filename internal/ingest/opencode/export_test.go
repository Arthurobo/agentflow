package opencode

// ConsumerCount exposes how many event-stream consumers are running.
func (i *Ingest) ConsumerCount() int { return i.consumerCount() }
