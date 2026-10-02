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
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0).Truncate(Period)}
	return NewStore(clock.Now), clock
}

var testKey = Key{Header: "x-client-id", Value: "key-a", Model: "model-a"}

func TestStore_ColdKey(t *testing.T) {
	s, _ := newTestStore()
	require.Equal(t, Stats{}, s.Stats(testKey, 1000))
}

func TestStore_CurrentPeriodIsNotExposed(t *testing.T) {
	s, clock := newTestStore()
	s.Record(testKey, Outcome{RequestBytes: 1000, InputTokens: 250})
	clock.Advance(Period - time.Nanosecond)
	// Still accumulating: nothing is exposed until the period completes.
	require.Equal(t, Stats{}, s.Stats(testKey, 1000))
}

func TestStore_EstimateFromPreviousPeriod(t *testing.T) {
	s, clock := newTestStore()
	s.Record(testKey, Outcome{RequestBytes: 1000, InputTokens: 250, CachedInputTokens: 100})
	s.Record(testKey, Outcome{RequestBytes: 3000, InputTokens: 750, CachedInputTokens: 300})
	clock.Advance(Period)

	// Requests of the next period are estimated from the completed one, while
	// that period accumulates on its own.
	s.Record(testKey, Outcome{RequestBytes: 1000, InputTokens: 1000})
	require.Equal(t, Stats{
		Samples:           2,
		Estimated:         true,
		InputTokens:       500, // 2000 bytes * (1000 tokens / 4000 bytes)
		CachedInputTokens: 200, // mean of 100 and 300
	}, s.Stats(testKey, 2000))

	// The period after exposes only what the previous one accumulated.
	clock.Advance(Period)
	require.Equal(t, Stats{Samples: 1, Estimated: true, InputTokens: 1000}, s.Stats(testKey, 1000))
}

func TestStore_IdlePeriodClearsEstimate(t *testing.T) {
	s, clock := newTestStore()
	s.Record(testKey, Outcome{RequestBytes: 1000, InputTokens: 250})
	clock.Advance(2 * Period)
	// The period before this one held no request.
	require.Equal(t, Stats{}, s.Stats(testKey, 1000))
}

func TestStore_KeysAreIndependent(t *testing.T) {
	s, clock := newTestStore()
	s.Record(testKey, Outcome{RequestBytes: 1000, InputTokens: 250})
	clock.Advance(Period)

	for _, k := range []Key{
		{Header: "x-client-id", Value: "key-b", Model: "model-a"},
		{Header: "x-client-id", Value: "key-a", Model: "model-b"},
		{Header: "x-other", Value: "key-a", Model: "model-a"},
	} {
		require.Equal(t, Stats{}, s.Stats(k, 1000), "%+v", k)
	}
}

func TestStore_Failures(t *testing.T) {
	s, clock := newTestStore()
	s.Record(testKey, Outcome{RequestBytes: 1000, Failed: true})
	clock.Advance(Period)
	require.Equal(t, Stats{Samples: 1, Failures: 1}, s.Stats(testKey, 1000))

	s.Record(testKey, Outcome{RequestBytes: 1000, InputTokens: 400, CachedInputTokens: 50})
	s.Record(testKey, Outcome{RequestBytes: 500, Failed: true})
	s.Record(testKey, Outcome{RequestBytes: 500, Failed: true})
	clock.Advance(Period)
	require.Equal(t, Stats{
		Samples:           3,
		Failures:          2,
		Estimated:         true,
		InputTokens:       400,
		CachedInputTokens: 50,
	}, s.Stats(testKey, 1000))
}

func TestStore_NoBytes(t *testing.T) {
	s, clock := newTestStore()
	// A success with an empty body gives no tokens-per-byte ratio.
	s.Record(testKey, Outcome{RequestBytes: 0, InputTokens: 10})
	clock.Advance(Period)
	require.Equal(t, Stats{Samples: 1}, s.Stats(testKey, 1000))
}

func TestStore_ClampsToUint32(t *testing.T) {
	s, clock := newTestStore()
	s.Record(testKey, Outcome{RequestBytes: 1, InputTokens: math.MaxUint32})
	clock.Advance(Period)
	st := s.Stats(testKey, math.MaxInt32)
	require.True(t, st.Estimated)
	require.Equal(t, uint32(math.MaxUint32), st.InputTokens)
}

func TestStore_Sweep(t *testing.T) {
	s, clock := newTestStore()
	s.Record(testKey, Outcome{RequestBytes: 1000, InputTokens: 100})
	clock.Advance(Period)
	s.Sweep()
	// The completed period is still exposed.
	require.Equal(t, 1, s.len())

	clock.Advance(Period)
	s.Sweep()
	require.Equal(t, 0, s.len())

	// A swept key starts over.
	s.Record(testKey, Outcome{RequestBytes: 1000, InputTokens: 100})
	clock.Advance(Period)
	require.Equal(t, uint32(1), s.Stats(testKey, 1000).Samples)
}

func TestStore_ConcurrentRecordAndSweep(_ *testing.T) {
	s, clock := newTestStore()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			k := Key{Header: "h", Value: string(rune('a' + i)), Model: "m"}
			for range 500 {
				s.Record(k, Outcome{RequestBytes: 10, InputTokens: 5})
				_ = s.Stats(k, 10)
			}
		})
	}
	wg.Go(func() {
		for range 500 {
			clock.Advance(Period / 10)
			s.Sweep()
		}
	})
	wg.Wait()
}

func TestStore_Run(t *testing.T) {
	s, clock := newTestStore()
	s.Record(testKey, Outcome{RequestBytes: 1000, InputTokens: 100})
	clock.Advance(2 * Period)

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
