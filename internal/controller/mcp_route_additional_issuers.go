// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package controller

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	egv1a1 "github.com/envoyproxy/gateway/api/v1alpha1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	aigv1b1 "github.com/envoyproxy/ai-gateway/api/v1beta1"
)

const (
	// mcpJWTProviderName is the name of the JWT provider of the issuer of a MCPRoute. The one of the
	// additional issuer at position N, counting from 1, has "-N" appended. The names are documented in
	// the API: they are what a SecurityPolicy merged with the generated one refers to.
	mcpJWTProviderName = "mcp-jwt-provider"

	// additionalIssuerRetryInterval is the delay before the JWKS URI of an additional issuer is looked up again.
	// It doubles up to additionalIssuerMaxRetryInterval, since each retry is a full reconciliation of the route
	// that also refreshes the configuration of its Gateways.
	additionalIssuerRetryInterval    = time.Minute
	additionalIssuerMaxRetryInterval = 15 * time.Minute
	// additionalIssuerDiscoveryTimeout bounds the lookups of all the additional issuers of a route within one
	// reconciliation. It is short since the reconciliation is itself retried and other routes wait for it.
	additionalIssuerDiscoveryTimeout = 5 * time.Second

	// maxJWKSURILength is the maximum length Envoy Gateway accepts for the URI of a remote JWKS.
	maxJWKSURILength = 253
)

// additionalIssuerJWTProviders returns the JWT providers of the additional issuers: copies of primary
// with the name, the issuer and the keys replaced.
//
// An issuer whose JWKS URI cannot be determined never affects the others. It keeps the remote JWKS it has
// in current, the providers of the existing SecurityPolicy, if any, as issuer does when its own lookup fails.
// Otherwise it is left out, so that its tokens are refused. Either way it is described in the second result.
func (c *MCPRouteController) additionalIssuerJWTProviders(
	ctx context.Context, issuers []aigv1b1.MCPRouteOAuthIssuer, primary *egv1a1.JWTProvider, current []egv1a1.JWTProvider,
) (providers []egv1a1.JWTProvider, undetermined []string) {
	jwksURIs := make([]string, len(issuers))
	errs := make([]error, len(issuers))
	discoveryCtx, cancel := context.WithTimeout(ctx, additionalIssuerDiscoveryTimeout)
	defer cancel()
	var wg sync.WaitGroup
	for i := range issuers {
		wg.Go(func() { jwksURIs[i], errs[i] = additionalIssuerJWKSURI(discoveryCtx, &issuers[i]) })
	}
	wg.Wait()

	for i := range issuers {
		issuer := &issuers[i]
		var remoteJWKS *egv1a1.RemoteJWKS
		if err := errs[i]; err == nil {
			remoteJWKS = c.newRemoteJWKS(ctx, jwksURIs[i])
		} else {
			c.logger.Error(err, "failed to determine the JWKS URI of an additional issuer", "issuer", issuer.Issuer)
			// Only fall back to what was in use for this very issuer, and only for a lookup that can
			// succeed later: an explicit URI replaces what was in use before.
			if issuer.JWKSURI == nil {
				remoteJWKS = currentRemoteJWKS(current, issuer.Issuer)
			}
			if remoteJWKS == nil {
				undetermined = append(undetermined, issuer.Issuer+" (tokens refused)")
				continue
			}
			undetermined = append(undetermined, issuer.Issuer+" (last known JWKS URI kept)")
		}
		provider := primary.DeepCopy()
		provider.Name = fmt.Sprintf("%s-%d", mcpJWTProviderName, i+1)
		provider.Issuer = issuer.Issuer
		provider.LocalJWKS = nil
		provider.RemoteJWKS = remoteJWKS
		providers = append(providers, *provider)
	}
	return providers, undetermined
}

// additionalIssuerJWKSURI returns the JWKS URI of the issuer, discovered from its metadata unless explicit.
func additionalIssuerJWKSURI(ctx context.Context, issuer *aigv1b1.MCPRouteOAuthIssuer) (string, error) {
	jwksURI := ptr.Deref(issuer.JWKSURI, "")
	if issuer.JWKSURI == nil {
		metadata, err := fetchOAuthAuthServerMetadataChecked(ctx, issuer.Issuer, "", additionalIssuerDiscoveryTimeout,
			func(metadata *OAuthAuthServerMetadata) error {
				// The document must be that of the issuer, see https://datatracker.ietf.org/doc/html/rfc8414#section-3.3
				if metadata.Issuer != issuer.Issuer {
					return fmt.Errorf("the document is that of issuer %q", metadata.Issuer)
				}
				if metadata.JwksURI == "" {
					return errors.New("missing jwks_uri")
				}
				return nil
			})
		if err != nil {
			return "", err
		}
		jwksURI = metadata.JwksURI
	}
	return jwksURI, validateJWKSURI(jwksURI)
}

// additionalIssuerRetry is the last delay given to a generation of a route.
type additionalIssuerRetry struct {
	generation int64
	delay      time.Duration
}

// nextAdditionalIssuerRetry returns the delay before the lookup of the additional issuers of a route is retried.
func (c *MCPRouteController) nextAdditionalIssuerRetry(route types.NamespacedName, generation int64) time.Duration {
	next := additionalIssuerRetry{generation: generation, delay: additionalIssuerRetryInterval}
	if v, ok := c.additionalIssuerRetries.Load(route); ok {
		if last := v.(additionalIssuerRetry); last.generation == generation {
			next.delay = min(2*last.delay, additionalIssuerMaxRetryInterval)
		}
	}
	c.additionalIssuerRetries.Store(route, next)
	return next.delay
}

// validateJWKSURI rejects the URIs that would make the whole SecurityPolicy invalid, and with it the
// authentication of every issuer.
func validateJWKSURI(jwksURI string) error {
	if len(jwksURI) > maxJWKSURILength {
		return fmt.Errorf("JWKS URI is longer than %d characters", maxJWKSURILength)
	}
	u, err := url.ParseRequestURI(jwksURI)
	if err != nil {
		return fmt.Errorf("invalid JWKS URI: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("invalid JWKS URI %q: the scheme must be http or https", jwksURI)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("invalid JWKS URI %q: missing host", jwksURI)
	}
	if port := u.Port(); port != "" {
		if n, err := strconv.ParseUint(port, 10, 16); err != nil || n == 0 {
			return errors.New("invalid JWKS URI: port out of range")
		}
	}
	return nil
}

// currentRemoteJWKS returns a copy of the remote JWKS in use for the issuer, or nil if there is none.
func currentRemoteJWKS(current []egv1a1.JWTProvider, issuer string) *egv1a1.RemoteJWKS {
	i := slices.IndexFunc(current, func(p egv1a1.JWTProvider) bool { return p.Issuer == issuer })
	if i < 0 {
		return nil
	}
	return current[i].RemoteJWKS.DeepCopy()
}

// undeterminedIssuersMessage is appended to the status message of a route accepted in spite of them.
func undeterminedIssuersMessage(undetermined []string) string {
	if len(undetermined) == 0 {
		return ""
	}
	return "; JWKS URI not found for additional issuers: " + strings.Join(undetermined, ", ")
}
