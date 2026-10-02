// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package quotareserve

import (
	"context"
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

func newTestStore() (*Store, *fakeClock) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	return NewStore(clock.Now), clock
}

var (
	testKey    = Key{Header: "x-client-id", Value: "key-1", Model: "deepseek"}
	testParams = Params{Window: time.Minute, MaxFailurePercent: 20, MinSamples: 5}
)

func success(bytes int, input, cached uint32) Outcome {
	return Outcome{RequestBytes: bytes, InputTokens: input, CachedInputTokens: cached}
}

func TestStore_Estimate(t *testing.T) {
	t.Run("a key with no response in the window has no estimate", func(t *testing.T) {
		s, _ := newTestStore()
		_, ok := s.Estimate(testKey, 4000, testParams)
		require.False(t, ok)
	})

	t.Run("input from the observed tokens per byte, cached tokens from the observed mean", func(t *testing.T) {
		s, clock := newTestStore()
		// A fixed 8,192-token shared prefix, 4 bytes per token.
		s.Record(testKey, success(40_000, 10_000, 8192), time.Minute)
		clock.Advance(time.Second)
		s.Record(testKey, success(80_000, 20_000, 8192), time.Minute)
		clock.Advance(time.Second)

		est, ok := s.Estimate(testKey, 60_000, testParams)
		require.True(t, ok)
		require.Equal(t, Estimate{InputTokens: 15_000, CachedInputTokens: 8192}, est)
	})

	t.Run("a cache miss pulls the mean of cached tokens down", func(t *testing.T) {
		s, _ := newTestStore()
		s.Record(testKey, success(40_000, 10_000, 8192), time.Minute)
		s.Record(testKey, success(40_000, 10_000, 8192), time.Minute)
		s.Record(testKey, success(40_000, 10_000, 8192), time.Minute)
		s.Record(testKey, success(40_000, 10_000, 0), time.Minute)

		est, ok := s.Estimate(testKey, 40_000, testParams)
		require.True(t, ok)
		require.Equal(t, Estimate{InputTokens: 10_000, CachedInputTokens: 6144}, est)
	})

	t.Run("failures carry no usage and are left out of the estimate", func(t *testing.T) {
		s, _ := newTestStore()
		s.Record(testKey, success(40_000, 10_000, 8192), time.Minute)
		s.Record(testKey, Outcome{RequestBytes: 400_000, Failed: true}, time.Minute)

		est, ok := s.Estimate(testKey, 40_000, testParams)
		require.True(t, ok)
		require.Equal(t, Estimate{InputTokens: 10_000, CachedInputTokens: 8192}, est)
	})

	t.Run("only failures in the window gives no estimate", func(t *testing.T) {
		s, _ := newTestStore()
		s.Record(testKey, Outcome{RequestBytes: 40_000, Failed: true}, time.Minute)
		_, ok := s.Estimate(testKey, 40_000, testParams)
		require.False(t, ok)
	})

	t.Run("a failure burst stops the reserve", func(t *testing.T) {
		s, _ := newTestStore()
		// 4 successes, 1 failure: 20% failed, at the threshold, still reserves.
		for range 4 {
			s.Record(testKey, success(40_000, 10_000, 8192), time.Minute)
		}
		s.Record(testKey, Outcome{Failed: true}, time.Minute)
		_, ok := s.Estimate(testKey, 40_000, testParams)
		require.True(t, ok)

		// 4 successes, 2 failures: 33% failed, above the threshold.
		s.Record(testKey, Outcome{Failed: true}, time.Minute)
		_, ok = s.Estimate(testKey, 40_000, testParams)
		require.False(t, ok)
	})

	t.Run("below MinSamples the failure share is not judged", func(t *testing.T) {
		s, _ := newTestStore()
		s.Record(testKey, success(40_000, 10_000, 8192), time.Minute)
		s.Record(testKey, Outcome{Failed: true}, time.Minute)
		s.Record(testKey, Outcome{Failed: true}, time.Minute)
		_, ok := s.Estimate(testKey, 40_000, testParams)
		require.True(t, ok)
	})

	t.Run("responses older than the window are not used", func(t *testing.T) {
		s, clock := newTestStore()
		s.Record(testKey, success(40_000, 10_000, 8192), time.Minute)
		clock.Advance(30 * time.Second)
		s.Record(testKey, success(40_000, 20_000, 0), time.Minute)
		clock.Advance(31 * time.Second)

		est, ok := s.Estimate(testKey, 40_000, testParams)
		require.True(t, ok)
		require.Equal(t, Estimate{InputTokens: 20_000, CachedInputTokens: 0}, est)

		clock.Advance(30 * time.Second)
		_, ok = s.Estimate(testKey, 40_000, testParams)
		require.False(t, ok)
	})

	t.Run("a shorter estimate window than the retention reads only its own span", func(t *testing.T) {
		s, clock := newTestStore()
		s.Record(testKey, success(40_000, 10_000, 0), 2*time.Minute)
		clock.Advance(90 * time.Second)
		_, ok := s.Estimate(testKey, 40_000, testParams)
		require.False(t, ok)
		_, ok = s.Estimate(testKey, 40_000, Params{Window: 2 * time.Minute, MaxFailurePercent: 20, MinSamples: 5})
		require.True(t, ok)
	})

	t.Run("keys are separate per header value and model", func(t *testing.T) {
		s, _ := newTestStore()
		s.Record(testKey, success(40_000, 10_000, 8192), time.Minute)
		for _, k := range []Key{
			{Header: testKey.Header, Value: "key-2", Model: testKey.Model},
			{Header: testKey.Header, Value: testKey.Value, Model: "other"},
			{Header: "x-other", Value: testKey.Value, Model: testKey.Model},
		} {
			_, ok := s.Estimate(k, 40_000, testParams)
			require.False(t, ok, "%+v", k)
		}
	})

	t.Run("responses with no request bytes give no ratio", func(t *testing.T) {
		s, _ := newTestStore()
		s.Record(testKey, success(0, 10_000, 8192), time.Minute)
		_, ok := s.Estimate(testKey, 40_000, testParams)
		require.False(t, ok)
	})

	t.Run("the estimated input saturates at the token counter range", func(t *testing.T) {
		s, _ := newTestStore()
		s.Record(testKey, success(1, 4_000_000_000, 0), time.Minute)
		est, ok := s.Estimate(testKey, 10, testParams)
		require.True(t, ok)
		require.Equal(t, uint32(1<<32-1), est.InputTokens)
	})
}

func TestStore_SamplesPerKeyAreBounded(t *testing.T) {
	s, _ := newTestStore()
	for range maxSamplesPerKey + 10 {
		s.Record(testKey, success(40_000, 10_000, 0), time.Minute)
	}
	s.Record(testKey, success(40_000, 20_000, 0), time.Minute)
	w, ok := s.keys.Load(testKey)
	require.True(t, ok)
	require.Len(t, w.(*window).samples, maxSamplesPerKey)
	est, ok := s.Estimate(testKey, 40_000, testParams)
	require.True(t, ok)
	require.Equal(t, uint32(10_002), est.InputTokens)
}

func TestStore_Sweep(t *testing.T) {
	s, clock := newTestStore()
	s.Record(testKey, success(40_000, 10_000, 8192), time.Minute)
	other := Key{Header: testKey.Header, Value: "key-2", Model: testKey.Model}
	s.Record(other, success(40_000, 10_000, 8192), 2*time.Minute)

	clock.Advance(61 * time.Second)
	s.Sweep()
	_, ok := s.keys.Load(testKey)
	require.False(t, ok, "idle for longer than its retention")
	_, ok = s.keys.Load(other)
	require.True(t, ok, "still within its retention")

	// A key swept away is recreated by the next response.
	s.Record(testKey, success(40_000, 10_000, 8192), time.Minute)
	_, ok = s.Estimate(testKey, 40_000, testParams)
	require.True(t, ok)
}

func TestStore_ConcurrentUse(t *testing.T) {
	s, clock := newTestStore()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			k := Key{Header: "x-client-id", Value: string(rune('a' + i%2)), Model: "m"}
			for range 500 {
				s.Record(k, success(40_000, 10_000, 8192), time.Minute)
				_, _ = s.Estimate(k, 40_000, testParams)
				if i == 0 {
					clock.Advance(100 * time.Millisecond)
					s.Sweep()
				}
			}
		}()
	}
	wg.Wait()
	_, ok := s.Estimate(Key{Header: "x-client-id", Value: "a", Model: "m"}, 40_000, testParams)
	require.True(t, ok)
}

func TestStore_RunSweepsUntilCancelled(t *testing.T) {
	s, clock := newTestStore()
	s.Record(testKey, success(40_000, 10_000, 8192), time.Minute)
	clock.Advance(2 * time.Minute)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		s.Run(ctx, time.Millisecond)
		close(done)
	}()
	require.Eventually(t, func() bool {
		_, ok := s.keys.Load(testKey)
		return !ok
	}, 5*time.Second, time.Millisecond)
	cancel()
	<-done
}
