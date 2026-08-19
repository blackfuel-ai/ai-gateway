# Classifying quota buckets and the classification signal

Two things live here: how a quota scope's buckets are configured so they classify
traffic without ever rejecting it, and the wire form of the outcome they report.
The worked example is a per-subscription scope, but the mechanism is generic: any
selector header and any override-header prefix make a scope.

## Classifying buckets

A scope needs no dedicated CRD field. It is a selector header plus an
override-header prefix, both expressed in an ordinary `QuotaPolicy` bucket rule.

- Selector: a request header (for a subscription scope, one carrying the
  subscription's id), matched `Distinct`, so every distinct value gets its own
  counters.
- Per-bucket limits: headers read through `quota.dynamicOverride.fromHeader`,
  one per bucket.

A bucket's name and its override header are two different vocabularies. The name
is the signal's vocabulary, chosen for what a recorded outcome should say; the
header follows whatever grammar the upstream authorization component uses to
stamp limits. The pairing is the contract between the two. The worked example:

| Bucket name                    | Override header                  | Counts                              |
| ------------------------------ | -------------------------------- | ----------------------------------- |
| `subscription-global`          | `<prefix>-subscription-req-1m`    | requests                            |
| `subscription-fresh-input-tpm` | `<prefix>-subscription-intok-1m`  | input tokens, cached input excluded |
| `subscription-output-tpm`      | `<prefix>-subscription-outtok-1m` | output tokens                       |

Selector and overrides alike are stamped by a trusted upstream component and
stripped before the request leaves the gateway; a client-supplied value must
never survive. All buckets of one scope sit on the same selector, so one
subject's counters move together. The fresh-input bucket charges
`input_tokens > cached_input_tokens ? input_tokens - cached_input_tokens : uint(0)`.
The subtraction is there because `input_tokens` carries the provider's
prompt-token count, which includes cached input; charging it directly would bill
a cache hit against the budget the bucket exists to keep free of them. The guard
is there because the counters are unsigned: a runtime reporting more cached input
than input makes the bare subtraction fail to evaluate, and a cost expression that
fails to evaluate aborts the request's whole dynamic-metadata struct — every
bucket's charge, the routing context and the served model with it, so the usage
record for that request goes missing rather than arriving wrong. Neither term is
redundant. `tests/crdcel/testdata/quotapolicies/subscription-shadow-buckets.yaml`
is the worked example.

**Classifying buckets never reject.** Each carries `shadowMode: true`, so
exceeding its limit is recorded and the request is still served. Shadow mode and
a per-request limit override are usable together: the override supplies the value
the bucket is measured against, shadow mode decides that exceeding it does not
reject.

## Bucket names

`bucketRules[].name` gives a bucket a stable identifier. It is the leading token
of every descriptor key the rule renders, it names the rule's stream-done cost
metadata key, and it is set as the `name` of the rendered rate limit policy —
which is what an outcome is reported against. Without it a bucket is identified
by its position (`rule-0`), so inserting a rule above it would silently change
what an already-recorded outcome refers to.

Names are unique within one model's quota definition; `default` is reserved for
the model's default bucket and `service` for the policy's service-wide quota.

## The classification signal

The signal rides the **request-time** quota filter
(`envoy.filters.http.ratelimit/ai-gateway-quota-request`). The stream-completion
filter is fire-and-forget and unordered against the access log, so an outcome
reported there may not reach the record.

The rate limit service returns it in `RateLimitResponse.dynamic_metadata`, which
Envoy stores verbatim in the dynamic metadata namespace
`envoy.filters.http.ratelimit`. The service must run with
`RESPONSE_DYNAMIC_METADATA=true`.

Fields in that struct:

| Field                  | Meaning                                                                                  |
| ---------------------- | ---------------------------------------------------------------------------------------- |
| `descriptors`          | one entry per descriptor sent, each listing its `key=value` pairs                        |
| `quotaModeViolations`  | indices into `descriptors` of quota-mode buckets that were over budget — already shipped |
| `shadowModeViolations` | **names** of shadow buckets that were over budget — _not yet shipped, see below_         |

`shadowModeViolations` is the classification signal. It is a list of bucket names,
not descriptor positions, so a stored outcome keeps its meaning across config
changes. Empty or absent-with-the-struct-present means every shadow bucket was
within budget.

### Access log and usage record

The consuming field name is **`genai_quota_exceeded_buckets`**, a string holding
the comma-joined `shadowModeViolations` list:

| Value                          | Meaning                                                            |
| ------------------------------ | ------------------------------------------------------------------ |
| `""` (present, empty)          | every classifying bucket within budget                             |
| e.g. `subscription-output-tpm` | that bucket was over budget                                        |
| absent / null                  | no signal arrived — the event is classified unknown, never guessed |

The gateway reports bucket names only. Mapping a set of bucket names onto a
commercial meaning happens where the usage event is assembled, not here.

### Bucket names on the client response

The rate limit service echoes each policy's `name` in
`descriptor_statuses[].current_limit.name`, and the quota filters enable
`X-RateLimit-*` headers (draft version 03), so Envoy renders every bucket with a
current limit — shadow buckets included — into the response's
`X-RateLimit-Limit` quota-policy list as `<limit>;w=<window>;name="<bucket>"`.
Bucket names are therefore visible to the client unless a downstream layer strips
those headers. Choose names that are acceptable to expose, or strip the headers.

## Headers that must survive to be worth producing

The selector and the override headers have to reach Envoy's rate limit filter for
these buckets to work. If one is dropped in between — an authorization filter
that does not forward it, a tier that strips it — the failure is silent and
asymmetric:

- selector missing → the descriptor is never generated, the bucket does not
  apply, and no outcome is reported: the request classifies unknown.
- override missing but selector present → the bucket silently falls back to its
  static limit, which is why the worked example sets that fallback to the
  protocol maximum. A low fallback there would report a request as over budget on
  a propagation failure rather than on real consumption.

Neither case rejects a request. Both are worth alerting on, because a bucket that
is configured, produced and consumed but dropped in transit looks exactly like a
subject with no traffic.

### Not yet shipped

`shadowModeViolations` does not exist yet. The rate limit service rewrites a
shadow bucket's over-limit result to allowed inside its limiter, before the layer
that assembles response metadata can see it, so the fact is destroyed at its
source. Recording it is a change to the rate limit service, which this repository
consumes as a pinned upstream image and does not build. Until that lands,
requests are correctly served and classified as unknown rather than
misclassified.
