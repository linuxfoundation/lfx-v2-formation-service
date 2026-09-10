// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package mock

import "sync"

// Recorder counts the port calls made against a double, so a test can assert
// what a code path costs rather than describe it.
//
// It exists for the event path. A project event reconciles one project, and the
// reads that path makes are the ones a full-catalogue republish multiplies by
// every project in the platform — so "this costs one query" is a claim worth
// pinning in a test, because it is invisible in a diff and only shows up as
// database load under exactly the burst nobody is watching for.
//
// Embedded by value in each double rather than shared between them, which keeps
// every constructor's signature as it was. The consequence is one ledger per
// double, so call names are qualified by the port they belong to and a caller
// wanting the whole picture merges them with Ledger.
//
// The zero value works: the map is built on first use.
type Recorder struct {
	mu    sync.Mutex
	calls map[string]int
}

// record counts one call to name.
func (r *Recorder) record(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.calls == nil {
		r.calls = map[string]int{}
	}
	r.calls[name]++
}

// Calls returns a copy of the ledger, so a caller reading it cannot race the
// double it came from.
func (r *Recorder) Calls() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make(map[string]int, len(r.calls))
	for name, count := range r.calls {
		out[name] = count
	}
	return out
}

// ResetCalls empties the ledger.
//
// What makes a per-event budget measurable at all: the arrangement a test needs
// before it can measure — seeding a template, creating the checklist the project
// already has — is itself made of port calls, and counting those would drown the
// one event being measured. A test arranges, resets, then acts.
func (r *Recorder) ResetCalls() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.calls = nil
}

// CallRecorder is what Recorder gives the double it is embedded in, so Ledger
// and ResetAll can take any mixture of them.
type CallRecorder interface {
	Calls() map[string]int
	ResetCalls()
}

// Ledger merges the ledgers of several doubles into one.
func Ledger(sources ...CallRecorder) map[string]int {
	out := map[string]int{}
	for _, source := range sources {
		for name, count := range source.Calls() {
			out[name] += count
		}
	}
	return out
}

// ResetAll empties every ledger, which is how a test draws the line between
// arranging and measuring.
func ResetAll(sources ...CallRecorder) {
	for _, source := range sources {
		source.ResetCalls()
	}
}

// Total sums a ledger's counts.
func Total(ledger map[string]int) int {
	sum := 0
	for _, count := range ledger {
		sum += count
	}
	return sum
}
