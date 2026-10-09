// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package llmcostcel

import (
	"testing"
	"testing/synctest"

	"github.com/google/cel-go/cel"
	"github.com/stretchr/testify/require"
)

func TestNewProgram(t *testing.T) {
	t.Run("invalid", func(t *testing.T) {
		_, err := NewProgram("1 +")
		require.Error(t, err)
	})
	t.Run("int", func(t *testing.T) {
		_, err := NewProgram("1 + 1")
		require.NoError(t, err)
	})
	t.Run("uint", func(t *testing.T) {
		_, err := NewProgram("uint(1) + uint(1)")
		require.NoError(t, err)
	})
	t.Run("variables", func(t *testing.T) {
		prog, err := NewProgram("model == 'cool_model' ?  (input_tokens - cached_input_tokens - cache_creation_input_tokens) * output_tokens  : total_tokens")
		require.NoError(t, err)
		v, err := EvaluateProgram(prog, "cool_model", "cool_backend", "cool_route", 200, 100, 1, 2, 3, 0)
		require.NoError(t, err)
		require.Equal(t, uint64(198), v)

		v, err = EvaluateProgram(prog, "not_cool_model", "cool_backend", "cool_route", 200, 100, 1, 2, 3, 0)
		require.NoError(t, err)
		require.Equal(t, uint64(3), v)
	})

	t.Run("uint", func(t *testing.T) {
		_, err := NewProgram("uint(1)-uint(1200)")
		require.ErrorContains(t, err, "failed to evaluate CEL expression: failed to evaluate CEL expression: unsigned integer overflow")
	})

	t.Run("ensure concurrency safety", func(t *testing.T) {
		// Ensure that the program can be evaluated concurrently.
		synctest.Test(t, func(t *testing.T) {
			for range 100 {
				go func() {
					_, err := NewProgram("model == 'cool_model' ?  input_tokens * output_tokens : total_tokens")
					require.NoError(t, err)
				}()
			}
		}) // synctest.Test waits for all goroutines to complete.
	})
}

func TestEvaluateProgram(t *testing.T) {
	t.Run("signed integer negative", func(t *testing.T) {
		prog, err := NewProgram("int(input_tokens) - int(output_tokens)")
		require.NoError(t, err)
		_, err = EvaluateProgram(prog, "cool_model", "cool_backend", "cool_route", 100, 0, 0, 2000, 3, 0)
		require.ErrorContains(t, err, "CEL expression result is negative (-1900)")
	})
	t.Run("unsigned integer overflow", func(t *testing.T) {
		prog, err := NewProgram("input_tokens - output_tokens")
		require.NoError(t, err)
		_, err = EvaluateProgram(prog, "cool_model", "cool_backend", "cool_route", 100, 0, 0, 2000, 3, 0)
		require.ErrorContains(t, err, "failed to evaluate CEL expression: unsigned integer overflow")
	})
	t.Run("reasoning_tokens variable", func(t *testing.T) {
		prog, err := NewProgram("output_tokens + reasoning_tokens")
		require.NoError(t, err)
		v, err := EvaluateProgram(prog, "cool_model", "cool_backend", "cool_route", 0, 0, 0, 100, 0, 50)
		require.NoError(t, err)
		require.Equal(t, uint64(150), v)
	})
	t.Run("ensure concurrency safety", func(t *testing.T) {
		prog, err := NewProgram("model == 'cool_model' ?  input_tokens * output_tokens : total_tokens")
		require.NoError(t, err)

		// Ensure that the program can be evaluated concurrently.
		synctest.Test(t, func(t *testing.T) {
			for range 100 {
				go func() {
					v, err := EvaluateProgram(prog, "cool_model", "cool_backend", "cool_route", 100, 0, 0, 2, 3, 0)
					require.NoError(t, err)
					require.Equal(t, uint64(200), v)
				}()
			}
		}) // synctest.Test waits for all goroutines to complete.
	})
}

// TestQuotaBucketExpressions locks in the cost expressions used by per-bucket
// quota policies: input tokens excluding cached input (underflow-guarded) and
// output tokens.
func TestQuotaBucketExpressions(t *testing.T) {
	t.Run("input tokens excluding cached, guarded", func(t *testing.T) {
		prog, err := NewProgram("input_tokens > cached_input_tokens ? input_tokens - cached_input_tokens : uint(0)")
		require.NoError(t, err)
		v, err := EvaluateProgram(prog, "m", "b", "r", 200, 150, 0, 10, 210, 0)
		require.NoError(t, err)
		require.Equal(t, uint64(50), v)

		// cached >= input never underflows thanks to the guard.
		v, err = EvaluateProgram(prog, "m", "b", "r", 100, 100, 0, 10, 110, 0)
		require.NoError(t, err)
		require.Equal(t, uint64(0), v)
	})
	t.Run("output tokens", func(t *testing.T) {
		prog, err := NewProgram("output_tokens")
		require.NoError(t, err)
		v, err := EvaluateProgram(prog, "m", "b", "r", 200, 150, 0, 42, 242, 0)
		require.NoError(t, err)
		require.Equal(t, uint64(42), v)
	})
}

func TestNewEstimateProgram(t *testing.T) {
	for _, expr := range []string{
		"input_tokens",
		"input_tokens > cached_input_tokens ? input_tokens - cached_input_tokens : uint(0)",
		"cache_rate",
		"input_tokens_per_byte",
		"double(input_tokens) * cache_rate",
	} {
		t.Run(expr, func(t *testing.T) {
			_, err := NewEstimateProgram(expr)
			require.NoError(t, err)
		})
	}
	t.Run("invalid", func(t *testing.T) {
		_, err := NewEstimateProgram("cache_rate +")
		require.ErrorContains(t, err, "cannot compile CEL expression")
	})
	t.Run("unknown variable", func(t *testing.T) {
		_, err := NewEstimateProgram("tokens_per_second")
		require.ErrorContains(t, err, "cannot compile CEL expression")
	})
	t.Run("not a number", func(t *testing.T) {
		_, err := NewEstimateProgram("model")
		require.ErrorContains(t, err, "CEL expression must return an int, uint or double, got string")
	})
	t.Run("not finite at zero usage", func(t *testing.T) {
		// Division by zero is valid until evaluated on actual values.
		_, err := NewEstimateProgram("input_tokens_per_byte / cache_rate")
		require.NoError(t, err)
	})
	t.Run("cost programs do not see the ratios", func(t *testing.T) {
		_, err := NewProgram("cache_rate")
		require.ErrorContains(t, err, "cannot compile CEL expression")
	})
}

func TestEvaluateEstimateProgram(t *testing.T) {
	in := EstimateInputs{
		Model:              "cool_model",
		InputTokens:        200,
		CachedInputTokens:  50,
		OutputTokens:       7,
		TotalTokens:        207,
		InputTokensPerByte: 0.25,
		CacheRate:          0.4,
	}
	for _, tc := range []struct {
		expr string
		want float64
	}{
		{expr: "input_tokens", want: 200},
		{expr: "int(input_tokens) - int(cached_input_tokens)", want: 150},
		{expr: "model == 'cool_model' ? total_tokens + output_tokens : uint(0)", want: 214},
		{expr: "cache_rate", want: 0.4},
		{expr: "input_tokens_per_byte", want: 0.25},
		{expr: "double(input_tokens) * cache_rate", want: 80},
	} {
		t.Run(tc.expr, func(t *testing.T) {
			prog, err := NewEstimateProgram(tc.expr)
			require.NoError(t, err)
			v, err := EvaluateEstimateProgram(prog, in)
			require.NoError(t, err)
			require.Equal(t, tc.want, v)
		})
	}
	for _, tc := range []struct {
		expr    string
		wantErr string
	}{
		{expr: "int(cached_input_tokens) - int(input_tokens)", wantErr: "CEL expression result is negative (-150)"},
		{expr: "cache_rate - 1.0", wantErr: "CEL expression result is negative (-0.6)"},
		{expr: "cache_rate / 0.0", wantErr: "CEL expression result is not finite (+Inf)"},
		{expr: "(cache_rate - 0.4) / 0.0", wantErr: "CEL expression result is not finite (NaN)"},
		{expr: "model", wantErr: "CEL expression result is not a number, got string"},
	} {
		t.Run(tc.expr, func(t *testing.T) {
			prog, err := estimateEnvProgram(t, tc.expr)
			require.NoError(t, err)
			_, err = EvaluateEstimateProgram(prog, in)
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
	t.Run("ensure concurrency safety", func(t *testing.T) {
		prog, err := NewEstimateProgram("double(input_tokens) * cache_rate")
		require.NoError(t, err)
		synctest.Test(t, func(t *testing.T) {
			for range 100 {
				go func() {
					v, err := EvaluateEstimateProgram(prog, in)
					require.NoError(t, err)
					require.Equal(t, float64(80), v)
				}()
			}
		})
	})
}

// estimateEnvProgram compiles expr in the estimate environment without the
// sanity evaluation of NewEstimateProgram, which rejects some failing expressions.
func estimateEnvProgram(t *testing.T, expr string) (cel.Program, error) {
	t.Helper()
	ast, issues := estimateEnv.Compile(expr)
	require.NoError(t, issues.Err())
	return estimateEnv.Program(ast)
}
