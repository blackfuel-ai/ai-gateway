// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package v1beta1

import (
	"fmt"
	"time"
)

// defaultUsageEstimatePeriod is the length of the usage estimate periods when not specified.
const defaultUsageEstimatePeriod = time.Minute

// GetUsageEstimatePeriod returns the configured usage estimate period, or the default
// when not configured.
func (s *GatewayConfigSpec) GetUsageEstimatePeriod() (time.Duration, error) {
	if s == nil || s.UsageEstimatePeriod == nil {
		return defaultUsageEstimatePeriod, nil
	}
	d, err := time.ParseDuration(string(*s.UsageEstimatePeriod))
	if err != nil {
		return 0, fmt.Errorf("invalid usageEstimatePeriod %q: %w", *s.UsageEstimatePeriod, err)
	}
	return d, nil
}
