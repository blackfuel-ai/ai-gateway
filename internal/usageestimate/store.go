// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

// Package usageestimate accumulates the completed requests of each key over fixed
// periods and estimates from the last completed period the token usage of a new
// request of the same key.
//
// A key is the value of a request header (for example the API key identity) and
// the model the client asked for. The estimate for a new request is:
//   - input tokens: the request's body size times the input tokens per body byte
//     of the successful responses of the last completed period;
//   - cached input tokens: the mean cached input tokens per successful response
//     of that period, which fits a fixed shared prompt prefix.
//
// The store is per process: each gateway replica estimates from the requests it
// served itself.
package usageestimate

import (
	"context"
	"math"
	"sync"
	"time"
)

// Period is the length of the periods the outcomes are accumulated over. Periods
// are aligned on the clock, and the outcomes of a period are exposed during the
// next one.
const Period = 15 * time.Second

// Key identifies the requests one estimate draws on.
type Key struct {
	// Header is the name of the request header grouping the requests.
	Header string
	// Value is the value of that header.
	Value string
	// Model is the model the client asked for.
	Model string
}

// Outcome is what a request that reached an upstream ended with.
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

// Stats summarizes the outcomes of a key in the last completed period and, when
// that period holds a successful response, the estimated usage of a new request.
type Stats struct {
	// Samples is the number of outcomes in the period.
	Samples uint32
	// Failures is the number of those outcomes that failed.
	Failures uint32
	// Estimated reports whether InputTokens and CachedInputTokens hold an
	// estimate. It is false when the period holds no successful response with a
	// request body.
	Estimated bool
	// InputTokens is the estimated number of input tokens.
	InputTokens uint32
	// CachedInputTokens is the estimated number of cached input tokens.
	CachedInputTokens uint32
}

// Store holds the accumulated outcomes of every key. It is safe for concurrent use.
type Store struct {
	now  func() time.Time
	keys sync.Map // Key -> *periods
}

// periods holds the outcomes of a key in the current period and the last
// completed one.
type periods struct {
	mu       sync.Mutex
	current  aggregate
	previous aggregate
	// dead marks an entry Sweep has removed from the store; a Record that loaded
	// it before the removal starts over with a new entry.
	dead bool
}

// aggregate is the sum of the outcomes of one period.
type aggregate struct {
	// period is the index of the period since the Unix epoch.
	period    int64
	samples   uint32
	failures  uint32
	successes uint64
	bytes     uint64
	input     uint64
	cached    uint64
}

// NewStore returns an empty store reading time from now.
func NewStore(now func() time.Time) *Store {
	return &Store{now: now}
}

// Record adds the outcome of a request of key k to the current period.
func (s *Store) Record(k Key, o Outcome) {
	period := periodOf(s.now())
	for {
		v, _ := s.keys.LoadOrStore(k, &periods{current: aggregate{period: period}})
		p := v.(*periods)
		p.mu.Lock()
		if p.dead {
			p.mu.Unlock()
			continue
		}
		p.rotateLocked(period)
		a := &p.current
		a.samples++
		if o.Failed {
			a.failures++
		} else {
			a.successes++
			a.bytes += uint64(max(o.RequestBytes, 0)) //nolint:gosec // non-negative after max.
			a.input += uint64(o.InputTokens)
			a.cached += uint64(o.CachedInputTokens)
		}
		p.mu.Unlock()
		return
	}
}

// Stats returns the outcomes of key k in the last completed period and the
// estimated usage of a new request of k whose body is requestBytes long.
func (s *Store) Stats(k Key, requestBytes int) Stats {
	v, ok := s.keys.Load(k)
	if !ok {
		return Stats{}
	}
	p := v.(*periods)
	p.mu.Lock()
	p.rotateLocked(periodOf(s.now()))
	a := p.previous
	p.mu.Unlock()

	st := Stats{Samples: a.samples, Failures: a.failures}
	if a.successes == 0 || a.bytes == 0 {
		return st
	}
	// The ratio is taken in floating point: bytes times summed tokens can exceed
	// the integer range.
	input := float64(max(requestBytes, 0)) * float64(a.input) / float64(a.bytes)
	st.Estimated = true
	st.InputTokens = uint32(min(input, math.MaxUint32))
	// A mean of uint32 values fits a uint32.
	st.CachedInputTokens = uint32(a.cached / a.successes) //nolint:gosec
	return st
}

// Sweep removes the keys with no outcome in the current or the last completed
// period.
func (s *Store) Sweep() {
	period := periodOf(s.now())
	s.keys.Range(func(k, v any) bool {
		p := v.(*periods)
		p.mu.Lock()
		p.rotateLocked(period)
		if p.current.samples == 0 && p.previous.samples == 0 {
			p.dead = true
			s.keys.CompareAndDelete(k, p)
		}
		p.mu.Unlock()
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

// len returns the number of keys in the store.
func (s *Store) len() int {
	n := 0
	s.keys.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// rotateLocked moves the current period to the previous one once it is over.
// p.mu must be held.
func (p *periods) rotateLocked(period int64) {
	switch {
	case p.current.period >= period:
		return
	case p.current.period == period-1:
		p.previous = p.current
	default:
		// The period before this one held no outcome.
		p.previous = aggregate{period: period - 1}
	}
	p.current = aggregate{period: period}
}

func periodOf(t time.Time) int64 {
	return t.UnixNano() / int64(Period)
}
