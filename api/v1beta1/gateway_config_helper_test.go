// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package v1beta1

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestGatewayConfigSpec_GetUsageEstimatePeriod(t *testing.T) {
	for _, tc := range []struct {
		name     string
		spec     *GatewayConfigSpec
		expected time.Duration
	}{
		{name: "nil spec defaults to 60s", spec: nil, expected: time.Minute},
		{name: "unset defaults to 60s", spec: &GatewayConfigSpec{}, expected: time.Minute},
		{name: "configured", spec: &GatewayConfigSpec{UsageEstimatePeriod: ptr.To(gwapiv1.Duration("15s"))}, expected: 15 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := tc.spec.GetUsageEstimatePeriod()
			require.NoError(t, err)
			require.Equal(t, tc.expected, d)
		})
	}

	t.Run("malformed", func(t *testing.T) {
		_, err := (&GatewayConfigSpec{UsageEstimatePeriod: ptr.To(gwapiv1.Duration("nope"))}).GetUsageEstimatePeriod()
		require.ErrorContains(t, err, `invalid usageEstimatePeriod "nope"`)
	})
}
