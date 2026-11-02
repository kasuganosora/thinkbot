// Package route is the L1 routing pipeline.
//
// L1 is a second pipeline above the existing execution pipeline. It does not
// recall stored notes, and its only tools open, preempt, or cancel an
// execution pipeline. Preempt injects the user message into a running
// execution pipeline and leaves that pipeline's reply stream as the single
// writer. Opening a pipeline is how every other intent, including a progress
// question, is followed up.
package route
