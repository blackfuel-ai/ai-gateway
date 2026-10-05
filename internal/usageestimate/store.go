// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

// Package usageestimate accumulates the successful responses of each key over
// fixed periods and estimates from the last completed period the token usage of a new
// request of the same key.
//
// A key is the value of a request header (for example the API key identity) and
// the model the client asked for. The estimate for a new request is:
//   - input tokens: the request's body size times the input tokens per body byte
//     of the successful responses of the last completed period;
//   - cached input tokens: the mean cached input tokens per successful response
//     of that period, which fits a fixed shared prompt prefix.
//
// The measured ratios of that period are exposed as well: its input tokens per
// body byte, and its cache rate, the share of its input tokens that were cached.
//
// Periods have the length the caller passes, are aligned on the clock, and the
// outcomes of a period are exposed during the next one. A key accumulated under
// another period length starts over.
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

// Key identifies the requests one estimate draws on.
type Key struct {
	// Header is the name of the request header grouping the requests.
	Header string
	// Value is the value of that header.
	Value string
	// Model is the model the client asked for.
	Model string
}

// Outcome is the usage a successful response reported for its request.
type Outcome struct {
	// RequestBytes is the size of the request body.
	RequestBytes int
	// InputTokens and CachedInputTokens are the usage the response reported.
	InputTokens       uint32
	CachedInputTokens uint32
}

// Stats is the estimated usage of a new request of a key, and the measured
// ratios it is drawn from, when the last completed period holds a successful
// response.
type Stats struct {
	// Estimated reports whether InputTokens, CachedInputTokens,
	// InputTokensPerByte and CacheRate hold a value. It is false when the period
	// holds no successful response with a request body.
	Estimated bool
	// InputTokens is the estimated number of input tokens.
	InputTokens uint32
	// CachedInputTokens is the estimated number of cached input tokens.
	CachedInputTokens uint32
	// InputTokensPerByte is the input tokens of the successful responses of the
	// period divided by the size of their request bodies.
	InputTokensPerByte float64
	// CacheRate is the cached input tokens of the successful responses of the
	// period divided by their input tokens, between 0 and 1. It is 0 when they
	// reported no input tokens.
	CacheRate float64
}

// Store holds the accumulated outcomes of every key. It is safe for concurrent use.
type Store struct {
	now  func() time.Time
	keys sync.Map // Key -> *periods
}

// periods holds the outcomes of a key in the current period and the last
// completed one.
type periods struct {
	mu sync.Mutex
	// length is the length of the periods the outcomes were accumulated over.
	length   time.Duration
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
	successes uint64
	bytes     uint64
	input     uint64
	cached    uint64
}

// NewStore returns an empty store reading time from now.
func NewStore(now func() time.Time) *Store {
	return &Store{now: now}
}

// Record adds a successful response of key k to the current period of the
// given length.
func (s *Store) Record(k Key, length time.Duration, o Outcome) {
	now := s.now()
	for {
		v, _ := s.keys.LoadOrStore(k, &periods{length: length, current: aggregate{period: periodOf(now, length)}})
		p := v.(*periods)
		p.mu.Lock()
		if p.dead {
			p.mu.Unlock()
			continue
		}
		p.rotateLocked(now, length)
		a := &p.current
		a.successes++
		a.bytes += uint64(max(o.RequestBytes, 0)) //nolint:gosec // non-negative after max.
		a.input += uint64(o.InputTokens)
		a.cached += uint64(o.CachedInputTokens)
		p.mu.Unlock()
		return
	}
}

// Stats returns the measured ratios of key k in the last completed period of the
// given length and the estimated usage of a new request of k whose body is
// requestBytes long.
func (s *Store) Stats(k Key, length time.Duration, requestBytes int) Stats {
	v, ok := s.keys.Load(k)
	if !ok {
		return Stats{}
	}
	p := v.(*periods)
	p.mu.Lock()
	p.rotateLocked(s.now(), length)
	a := p.previous
	p.mu.Unlock()

	if a.successes == 0 || a.bytes == 0 {
		return Stats{}
	}
	var st Stats
	// The ratios are taken in floating point: bytes times summed tokens can exceed
	// the integer range. The estimate multiplies before dividing, so a request of
	// a measured size is estimated at exactly its measured tokens.
	st.InputTokensPerByte = float64(a.input) / float64(a.bytes)
	if a.input > 0 {
		st.CacheRate = float64(a.cached) / float64(a.input)
	}
	input := float64(max(requestBytes, 0)) * float64(a.input) / float64(a.bytes)
	st.Estimated = true
	st.InputTokens = uint32(min(input, math.MaxUint32))
	// A mean of uint32 values fits a uint32.
	st.CachedInputTokens = uint32(a.cached / a.successes) //nolint:gosec
	return st
}

// Sweep removes the keys with no outcome in the current or the last completed
// period, each by the length of its own periods.
func (s *Store) Sweep() {
	now := s.now()
	s.keys.Range(func(k, v any) bool {
		p := v.(*periods)
		p.mu.Lock()
		p.rotateLocked(now, p.length)
		if p.current.successes == 0 && p.previous.successes == 0 {
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
// Outcomes accumulated under another period length share no period index with
// this one, so the key starts over. p.mu must be held.
func (p *periods) rotateLocked(now time.Time, length time.Duration) {
	period := periodOf(now, length)
	switch {
	case p.length != length:
		p.length = length
		p.previous = aggregate{period: period - 1}
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

func periodOf(t time.Time, length time.Duration) int64 {
	return t.UnixNano() / int64(length)
}
