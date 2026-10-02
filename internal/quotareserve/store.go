// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

// Package quotareserve keeps the recently completed requests of each estimate
// key and estimates from them the token usage of a new request, for the
// admission reserve of quota buckets.
//
// An estimate for a new request of the same key and model is:
//   - input tokens: the request's body size times the input tokens per byte of
//     the successful responses in the window;
//   - cached input tokens: the mean cached input tokens per successful response
//     in the window, which fits a fixed shared prompt prefix.
//
// The store is per process: each gateway replica estimates from the requests it
// served itself.
package quotareserve

import (
	"context"
	"math"
	"sync"
	"time"
)

// maxSamplesPerKey bounds the samples kept per key, dropping the oldest, so a
// single key's memory stays bounded whatever its request rate.
const maxSamplesPerKey = 4096

// Key identifies the requests whose completed responses one estimate draws on:
// the value of the estimate header and the model the client asked for.
type Key struct {
	Header string
	Value  string
	Model  string
}

// Outcome is what an admitted request ended with.
type Outcome struct {
	// RequestBytes is the size of the request body.
	RequestBytes int
	// InputTokens and CachedInputTokens are the usage the response reported.
	InputTokens       uint32
	CachedInputTokens uint32
	// Failed marks a request that ended without a successful response: an
	// error status, a response without usage, or a stream aborted early.
	Failed bool
}

// Params selects which recent outcomes an estimate draws on.
type Params struct {
	// Window is how far back the outcomes go.
	Window time.Duration
	// MaxFailurePercent is the share of outcomes in the window that may be
	// failures; above it there is no estimate.
	MaxFailurePercent uint32
	// MinSamples is the number of outcomes the window must hold before
	// MaxFailurePercent applies.
	MinSamples uint32
}

// Estimate is the estimated usage of a new request.
type Estimate struct {
	InputTokens       uint32
	CachedInputTokens uint32
}

// Store holds the recent outcomes of every key. It is safe for concurrent use.
type Store struct {
	now  func() time.Time
	keys sync.Map // Key -> *window
}

type window struct {
	mu sync.Mutex
	// samples are in arrival order.
	samples []sample
	// retain is the longest window any Record call asked this key to keep.
	retain time.Duration
	// dead marks a window Sweep has removed from the store; a Record that
	// loaded it before the removal starts over with a new window.
	dead bool
}

type sample struct {
	at time.Time
	Outcome
}

// NewStore returns an empty store reading time from now.
func NewStore(now func() time.Time) *Store {
	return &Store{now: now}
}

// Record adds the outcome of a request of key k, kept for at least retain.
func (s *Store) Record(k Key, o Outcome, retain time.Duration) {
	now := s.now()
	for {
		v, _ := s.keys.LoadOrStore(k, &window{})
		w := v.(*window)
		w.mu.Lock()
		if w.dead {
			w.mu.Unlock()
			continue
		}
		w.retain = max(w.retain, retain)
		w.pruneLocked(now)
		if len(w.samples) == maxSamplesPerKey {
			w.samples = append(w.samples[:0], w.samples[1:]...)
		}
		w.samples = append(w.samples, sample{at: now, Outcome: o})
		w.mu.Unlock()
		return
	}
}

// Estimate returns the estimated usage of a new request of key k whose body is
// requestBytes long. It returns false when the window holds no successful
// response with a request size, or when more than p.MaxFailurePercent of the
// window's outcomes failed.
func (s *Store) Estimate(k Key, requestBytes int, p Params) (Estimate, bool) {
	v, ok := s.keys.Load(k)
	if !ok {
		return Estimate{}, false
	}
	w := v.(*window)
	since := s.now().Add(-p.Window)

	var total, failed, successes, sumInput, sumCached uint64
	var sumBytes float64
	w.mu.Lock()
	for i := len(w.samples) - 1; i >= 0 && w.samples[i].at.After(since); i-- {
		o := &w.samples[i].Outcome
		total++
		if o.Failed {
			failed++
			continue
		}
		successes++
		sumBytes += float64(max(o.RequestBytes, 0))
		sumInput += uint64(o.InputTokens)
		sumCached += uint64(o.CachedInputTokens)
	}
	w.mu.Unlock()

	if total >= uint64(p.MinSamples) && failed*100 > uint64(p.MaxFailurePercent)*total {
		return Estimate{}, false
	}
	if successes == 0 || sumBytes == 0 {
		return Estimate{}, false
	}
	// The ratio is taken in floating point: bytes times summed tokens can exceed
	// the integer range.
	input := float64(max(requestBytes, 0)) * float64(sumInput) / sumBytes
	return Estimate{
		InputTokens: uint32(min(input, math.MaxUint32)),
		// A mean of uint32 values fits a uint32.
		CachedInputTokens: uint32(sumCached / successes), //nolint:gosec
	}, true
}

// Sweep removes the keys whose every outcome is older than their retention.
func (s *Store) Sweep() {
	now := s.now()
	s.keys.Range(func(k, v any) bool {
		w := v.(*window)
		w.mu.Lock()
		w.pruneLocked(now)
		if len(w.samples) == 0 {
			w.dead = true
			s.keys.CompareAndDelete(k, w)
		}
		w.mu.Unlock()
		return true
	})
}

// Run sweeps the store every interval until ctx is done.
func (s *Store) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Sweep()
		}
	}
}

// pruneLocked drops the samples older than the retention. w.mu must be held.
func (w *window) pruneLocked(now time.Time) {
	since := now.Add(-w.retain)
	i := 0
	for i < len(w.samples) && !w.samples[i].at.After(since) {
		i++
	}
	if i > 0 {
		w.samples = append(w.samples[:0], w.samples[i:]...)
	}
}
