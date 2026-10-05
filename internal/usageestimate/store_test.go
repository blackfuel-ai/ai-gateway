// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package usageestimate

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// newTestStore returns a store whose clock is at the start of a period.
func newTestStore() (*Store, *fakeClock) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0).Truncate(testPeriod)}
	return NewStore(clock.Now), clock
}

const testPeriod = 15 * time.Second

var testKey = Key{Header: "x-client-id", Value: "key-a", Model: "model-a"}

func TestStore_PeriodChangeStartsOver(t *testing.T) {
	s, clock := newTestStore()
	s.Record(testKey, testPeriod, Outcome{RequestBytes: 1000, InputTokens: 250})
	clock.Advance(4 * testPeriod)
	// A request under a period 4 times as long shares no period index with the
	// outcomes accumulated so far: the key starts over.
	require.Equal(t, Stats{}, s.Stats(testKey, 4*testPeriod, 1000))
	s.Record(testKey, 4*testPeriod, Outcome{RequestBytes: 1000, InputTokens: 500})
	clock.Advance(4 * testPeriod)
	require.Equal(t, Stats{Estimated: true, InputTokens: 500, InputTokensPerByte: 0.5}, s.Stats(testKey, 4*testPeriod, 1000))
}

func TestStore_SweepUsesTheKeyPeriod(t *testing.T) {
	s, clock := newTestStore()
	s.Record(testKey, 4*testPeriod, Outcome{RequestBytes: 1000, InputTokens: 100})
	// Two short periods later, the key's own period is still the current one.
	clock.Advance(2 * testPeriod)
	s.Sweep()
	require.Equal(t, 1, s.len())
	// Two of its periods later, it has had no outcome in the current or the last
	// completed period.
	clock.Advance(6 * testPeriod)
	s.Sweep()
	require.Equal(t, 0, s.len())
}

func TestStore_ColdKey(t *testing.T) {
	s, _ := newTestStore()
	require.Equal(t, Stats{}, s.Stats(testKey, testPeriod, 1000))
}

func TestStore_CurrentPeriodIsNotExposed(t *testing.T) {
	s, clock := newTestStore()
	s.Record(testKey, testPeriod, Outcome{RequestBytes: 1000, InputTokens: 250})
	clock.Advance(testPeriod - time.Nanosecond)
	// Still accumulating: nothing is exposed until the period completes.
	require.Equal(t, Stats{}, s.Stats(testKey, testPeriod, 1000))
}

func TestStore_EstimateFromPreviousPeriod(t *testing.T) {
	s, clock := newTestStore()
	s.Record(testKey, testPeriod, Outcome{RequestBytes: 1000, InputTokens: 250, CachedInputTokens: 100})
	s.Record(testKey, testPeriod, Outcome{RequestBytes: 3000, InputTokens: 750})
	clock.Advance(testPeriod)

	// Requests of the next period are estimated from the completed one, while
	// that period accumulates on its own.
	s.Record(testKey, testPeriod, Outcome{RequestBytes: 1000, InputTokens: 1000})
	require.Equal(t, Stats{
		Estimated:          true,
		InputTokens:        500, // 2000 bytes * (1000 tokens / 4000 bytes)
		CachedInputTokens:  100, // 500 input tokens * 0.2
		InputTokensPerByte: 0.25,
		CacheRate:          0.2, // mean of 100/250 and 0/750, where the period's sums give 0.1
	}, s.Stats(testKey, testPeriod, 2000))

	// The period after exposes only what the previous one accumulated.
	clock.Advance(testPeriod)
	require.Equal(t, Stats{Estimated: true, InputTokens: 1000, InputTokensPerByte: 1}, s.Stats(testKey, testPeriod, 1000))
}

func TestStore_IdlePeriodClearsEstimate(t *testing.T) {
	s, clock := newTestStore()
	s.Record(testKey, testPeriod, Outcome{RequestBytes: 1000, InputTokens: 250})
	clock.Advance(2 * testPeriod)
	// The period before this one held no request.
	require.Equal(t, Stats{}, s.Stats(testKey, testPeriod, 1000))
}

func TestStore_KeysAreIndependent(t *testing.T) {
	s, clock := newTestStore()
	s.Record(testKey, testPeriod, Outcome{RequestBytes: 1000, InputTokens: 250})
	clock.Advance(testPeriod)

	for _, k := range []Key{
		{Header: "x-client-id", Value: "key-b", Model: "model-a"},
		{Header: "x-client-id", Value: "key-a", Model: "model-b"},
		{Header: "x-other", Value: "key-a", Model: "model-a"},
	} {
		require.Equal(t, Stats{}, s.Stats(k, testPeriod, 1000), "%+v", k)
	}
}

func TestStore_MeasuredRatios(t *testing.T) {
	s, clock := newTestStore()
	// A small request hitting the prompt cache and a large one missing it.
	s.Record(testKey, testPeriod, Outcome{RequestBytes: 1000, InputTokens: 200, CachedInputTokens: 150})
	s.Record(testKey, testPeriod, Outcome{RequestBytes: 9000, InputTokens: 1800})
	clock.Advance(testPeriod)

	st := s.Stats(testKey, testPeriod, 1000)
	require.Equal(t, uint32(200), st.InputTokens)
	// The input tokens per byte are the period's sums, 2000 tokens over 10000
	// bytes, while the cache rate is the mean of the cache rate of each request,
	// 0.75 and 0, not the 150 cached of 2000 input tokens of the period.
	require.Equal(t, 0.2, st.InputTokensPerByte)
	require.Equal(t, 0.375, st.CacheRate)
	require.Equal(t, uint32(75), st.CachedInputTokens)
	// They do not depend on the size of the request being estimated.
	require.Equal(t, st.InputTokensPerByte, s.Stats(testKey, testPeriod, 50).InputTokensPerByte)
	require.Equal(t, st.CacheRate, s.Stats(testKey, testPeriod, 50).CacheRate)
}

func TestStore_CachedInputTokensScaleWithTheRequest(t *testing.T) {
	s, clock := newTestStore()
	// Large requests sharing a 20000-token cached prefix.
	s.Record(testKey, testPeriod, Outcome{RequestBytes: 100_000, InputTokens: 25_000, CachedInputTokens: 20_000})
	s.Record(testKey, testPeriod, Outcome{RequestBytes: 100_000, InputTokens: 25_000, CachedInputTokens: 20_000})
	clock.Advance(testPeriod)

	// A small request is estimated with the measured cache rate of its own input
	// tokens, never with more cached tokens than input tokens.
	st := s.Stats(testKey, testPeriod, 4000)
	require.Equal(t, uint32(1000), st.InputTokens)
	require.Equal(t, uint32(800), st.CachedInputTokens)
}

func TestStore_NoInputTokens(t *testing.T) {
	s, clock := newTestStore()
	// Successes reporting zero input tokens give a zero ratio and cache rate.
	s.Record(testKey, testPeriod, Outcome{RequestBytes: 1000})
	clock.Advance(testPeriod)
	require.Equal(t, Stats{Estimated: true}, s.Stats(testKey, testPeriod, 1000))

	// They have no cache rate of their own and leave the mean of the others.
	s.Record(testKey, testPeriod, Outcome{RequestBytes: 1000})
	s.Record(testKey, testPeriod, Outcome{RequestBytes: 1000, InputTokens: 100, CachedInputTokens: 50})
	clock.Advance(testPeriod)
	require.Equal(t, 0.5, s.Stats(testKey, testPeriod, 1000).CacheRate)
}

func TestStore_CacheRateIsAtMostOne(t *testing.T) {
	s, clock := newTestStore()
	// A response reporting more cached than input tokens counts as fully cached.
	s.Record(testKey, testPeriod, Outcome{RequestBytes: 1000, InputTokens: 100, CachedInputTokens: 150})
	clock.Advance(testPeriod)
	st := s.Stats(testKey, testPeriod, 1000)
	require.Equal(t, 1.0, st.CacheRate)
	require.Equal(t, uint32(100), st.CachedInputTokens)
}

func TestStore_NoBytes(t *testing.T) {
	s, clock := newTestStore()
	// A success with an empty body gives no tokens-per-byte ratio.
	s.Record(testKey, testPeriod, Outcome{RequestBytes: 0, InputTokens: 10})
	clock.Advance(testPeriod)
	require.Equal(t, Stats{}, s.Stats(testKey, testPeriod, 1000))
}

func TestStore_SameSizeEstimatesTheSameTokens(t *testing.T) {
	s, clock := newTestStore()
	// 43/77 and 23/43 are not exact in floating point: multiplying by the rounded
	// ratios would truncate the estimates to 42 and 22.
	s.Record(testKey, testPeriod, Outcome{RequestBytes: 77, InputTokens: 43, CachedInputTokens: 23})
	clock.Advance(testPeriod)
	st := s.Stats(testKey, testPeriod, 77)
	require.Equal(t, uint32(43), st.InputTokens)
	require.Equal(t, uint32(23), st.CachedInputTokens)
}

func TestStore_ClampsToUint32(t *testing.T) {
	s, clock := newTestStore()
	s.Record(testKey, testPeriod, Outcome{RequestBytes: 1, InputTokens: math.MaxUint32})
	clock.Advance(testPeriod)
	st := s.Stats(testKey, testPeriod, math.MaxInt32)
	require.True(t, st.Estimated)
	require.Equal(t, uint32(math.MaxUint32), st.InputTokens)
}

func TestStore_Sweep(t *testing.T) {
	s, clock := newTestStore()
	s.Record(testKey, testPeriod, Outcome{RequestBytes: 1000, InputTokens: 100})
	clock.Advance(testPeriod)
	s.Sweep()
	// The completed period is still exposed.
	require.Equal(t, 1, s.len())

	clock.Advance(testPeriod)
	s.Sweep()
	require.Equal(t, 0, s.len())

	// A swept key starts over.
	s.Record(testKey, testPeriod, Outcome{RequestBytes: 1000, InputTokens: 100})
	clock.Advance(testPeriod)
	require.Equal(t, uint32(100), s.Stats(testKey, testPeriod, 1000).InputTokens)
}

func TestStore_ConcurrentRecordAndSweep(_ *testing.T) {
	s, clock := newTestStore()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			k := Key{Header: "h", Value: string(rune('a' + i)), Model: "m"}
			for range 500 {
				s.Record(k, testPeriod, Outcome{RequestBytes: 10, InputTokens: 5})
				_ = s.Stats(k, testPeriod, 10)
			}
		})
	}
	wg.Go(func() {
		for range 500 {
			clock.Advance(testPeriod / 10)
			s.Sweep()
		}
	})
	wg.Wait()
}

func TestStore_Run(t *testing.T) {
	s, clock := newTestStore()
	s.Record(testKey, testPeriod, Outcome{RequestBytes: 1000, InputTokens: 100})
	clock.Advance(2 * testPeriod)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		s.Run(ctx, time.Millisecond)
		close(done)
	}()
	require.Eventually(t, func() bool { return s.len() == 0 }, 5*time.Second, time.Millisecond)
	cancel()
	<-done
}
