// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

// Package llmcostcel provides functions to create and evaluate CEL programs to calculate costs.
//
// This exists as a separate package to be used both in the controller to validate the expression
// and in the external processor to evaluate the expression.
package llmcostcel

import (
	"fmt"
	"math"

	"github.com/google/cel-go/cel"
)

const (
	celModelNameKey                = "model"
	celBackendKey                  = "backend"
	celRouteNameKey                = "route_name"
	celInputTokensKey              = "input_tokens"
	celCachedInputTokensKey        = "cached_input_tokens"         // #nosec G101
	celCacheCreationInputTokensKey = "cache_creation_input_tokens" // #nosec G101
	celOutputTokensKey             = "output_tokens"
	celTotalTokensKey              = "total_tokens"
	celReasoningTokensKey          = "reasoning_tokens"
	celInputTokensPerByteKey       = "input_tokens_per_byte" // #nosec G101
	celCacheRateKey                = "cache_rate"
)

var (
	// env is the environment of the cost expressions.
	env *cel.Env
	// estimateEnv is the environment of the usage estimate expressions: the cost
	// variables and the measured ratios.
	estimateEnv *cel.Env
)

func init() {
	var err error
	env, err = cel.NewEnv(
		cel.Variable(celModelNameKey, cel.StringType),
		cel.Variable(celBackendKey, cel.StringType),
		cel.Variable(celRouteNameKey, cel.StringType),
		cel.Variable(celInputTokensKey, cel.UintType),
		cel.Variable(celCachedInputTokensKey, cel.UintType),
		cel.Variable(celCacheCreationInputTokensKey, cel.UintType),
		cel.Variable(celOutputTokensKey, cel.UintType),
		cel.Variable(celTotalTokensKey, cel.UintType),
		cel.Variable(celReasoningTokensKey, cel.UintType),
	)
	if err != nil {
		panic(fmt.Sprintf("cannot create CEL environment: %v", err))
	}
	estimateEnv, err = env.Extend(
		cel.Variable(celInputTokensPerByteKey, cel.DoubleType),
		cel.Variable(celCacheRateKey, cel.DoubleType),
	)
	if err != nil {
		panic(fmt.Sprintf("cannot create usage estimate CEL environment: %v", err))
	}
}

// NewProgram creates a new CEL program from the given expression.
func NewProgram(expr string) (prog cel.Program, err error) {
	ast, issues := env.Compile(expr)
	if issues != nil && issues.Err() != nil {
		err = issues.Err()
		return nil, fmt.Errorf("cannot compile CEL expression: %w", err)
	}
	prog, err = env.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("cannot create CEL program: %w", err)
	}

	// Sanity check by evaluating the expression with some dummy values.
	_, err = EvaluateProgram(prog, "dummy", "dummy", "dummy", 0, 0, 0, 0, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to evaluate CEL expression: %w", err)
	}
	return prog, nil
}

// EvaluateProgram evaluates the given CEL program with the given variables.
func EvaluateProgram(prog cel.Program, modelName, backend, routeName string, inputTokens, cachedInputTokens, cacheCreationInputTokens, outputTokens, totalTokens, reasoningTokens uint32) (uint64, error) {
	out, _, err := prog.Eval(map[string]any{
		celModelNameKey:                modelName,
		celBackendKey:                  backend,
		celRouteNameKey:                routeName,
		celInputTokensKey:              inputTokens,
		celCachedInputTokensKey:        cachedInputTokens,
		celCacheCreationInputTokensKey: cacheCreationInputTokens,
		celOutputTokensKey:             outputTokens,
		celTotalTokensKey:              totalTokens,
		celReasoningTokensKey:          reasoningTokens,
	})
	if err != nil || out == nil {
		return 0, fmt.Errorf("failed to evaluate CEL expression: %w", err)
	}

	switch out.Type() {
	case cel.IntType:
		result := out.Value().(int64)
		if result < 0 {
			return 0, fmt.Errorf("CEL expression result is negative (%d)", result)
		}
		return uint64(result), nil
	case cel.UintType:
		return out.Value().(uint64), nil
	default:
		return 0, fmt.Errorf("CEL expression result is not an integer, got %v", out.Type())
	}
}

// EstimateInputs are the variables of a usage estimate expression. The backend
// and the route name are always empty: no route is selected at admission.
type EstimateInputs struct {
	Model                    string
	InputTokens              uint32
	CachedInputTokens        uint32
	CacheCreationInputTokens uint32
	OutputTokens             uint32
	TotalTokens              uint32
	ReasoningTokens          uint32
	// InputTokensPerByte is the input tokens divided by the size of the request bodies.
	InputTokensPerByte float64
	// CacheRate is the cached input tokens divided by the input tokens, between 0 and 1.
	CacheRate float64
}

// NewEstimateProgram creates a CEL program computing a usage estimate. The
// expression must return an int, a uint or a double.
func NewEstimateProgram(expr string) (cel.Program, error) {
	ast, issues := estimateEnv.Compile(expr)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("cannot compile CEL expression: %w", issues.Err())
	}
	switch t := ast.OutputType(); t {
	case cel.IntType, cel.UintType, cel.DoubleType:
	default:
		return nil, fmt.Errorf("CEL expression must return an int, uint or double, got %v", t)
	}
	prog, err := estimateEnv.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("cannot create CEL program: %w", err)
	}
	// Sanity check by evaluating the expression on zero usage. Only an evaluation
	// error is rejected: a result that is not finite at zero, such as a division by
	// a ratio, can be valid on actual values.
	if _, _, err = prog.Eval(estimateActivation(EstimateInputs{Model: "dummy"})); err != nil {
		return nil, fmt.Errorf("failed to evaluate CEL expression: %w", err)
	}
	return prog, nil
}

// EvaluateEstimateProgram evaluates a program created by NewEstimateProgram. The
// result must be a finite number that is not negative.
func EvaluateEstimateProgram(prog cel.Program, in EstimateInputs) (float64, error) {
	out, _, err := prog.Eval(estimateActivation(in))
	if err != nil || out == nil {
		return 0, fmt.Errorf("failed to evaluate CEL expression: %w", err)
	}
	var result float64
	switch out.Type() {
	case cel.IntType:
		result = float64(out.Value().(int64))
	case cel.UintType:
		result = float64(out.Value().(uint64))
	case cel.DoubleType:
		result = out.Value().(float64)
	default:
		return 0, fmt.Errorf("CEL expression result is not a number, got %v", out.Type())
	}
	if math.IsNaN(result) || math.IsInf(result, 0) {
		return 0, fmt.Errorf("CEL expression result is not finite (%v)", result)
	}
	if result < 0 {
		return 0, fmt.Errorf("CEL expression result is negative (%v)", result)
	}
	return result, nil
}

func estimateActivation(in EstimateInputs) map[string]any {
	return map[string]any{
		celModelNameKey:                in.Model,
		celBackendKey:                  "",
		celRouteNameKey:                "",
		celInputTokensKey:              in.InputTokens,
		celCachedInputTokensKey:        in.CachedInputTokens,
		celCacheCreationInputTokensKey: in.CacheCreationInputTokens,
		celOutputTokensKey:             in.OutputTokens,
		celTotalTokensKey:              in.TotalTokens,
		celReasoningTokensKey:          in.ReasoningTokens,
		celInputTokensPerByteKey:       in.InputTokensPerByte,
		celCacheRateKey:                in.CacheRate,
	}
}
