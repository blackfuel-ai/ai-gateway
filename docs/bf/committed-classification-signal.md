# Committed-subscription quota buckets and the classification signal

Fork-local contract. Two things live here: how a subscription's quota buckets are
configured so they classify traffic without ever rejecting it, and the wire form
of the outcome they report.

## Subscription-scope buckets

A subscription is a third quota scope beside the organization and the API key. It
needs no new CRD field: a scope is a selector header plus an override-header
prefix, both expressed in an ordinary `QuotaPolicy` bucket rule.

- Selector: `x-bf-subscription-id`, matched `Distinct`, carrying the
  subscription's billing-keyed external id. Every subscription gets its own
  counters.
- Per-bucket limits: `x-bf-subscription-quota-*`, read through
  `quota.dynamicOverride.fromHeader`.

Both are stamped by the authorization service and stripped before the request
leaves the gateway; a client-supplied value never survives. Three buckets are
configured per subscription — a global one plus fresh-input and output token
rates — each on the same selector with its own override header and cost
expression. `tests/crdcel/testdata/quotapolicies/subscription-shadow-buckets.yaml`
is the worked example.

**These buckets classify and never reject.** Each carries `shadowMode: true`, so
exceeding its limit is recorded and the request is still served. A caller past
its committed budget spills over; it is never answered 429 by these buckets.
Shadow mode and a per-request limit override are usable together: the override
supplies the value the bucket is measured against, shadow mode decides that
exceeding it does not reject.

## Bucket names

`bucketRules[].name` gives a bucket a stable identifier. It is the leading token
of every descriptor key the rule renders, it names the rule's stream-done cost
metadata key, and it is set as the `name` of the rendered rate limit policy —
which is what an outcome is reported against. Without it a bucket is identified
by its position (`rule-0`), so inserting a rule above it would silently change
what an already-recorded outcome refers to.

Names are unique within one model's quota definition; `default` is reserved for
the model's default bucket and `service` for the policy's service-wide quota.

The names the subscription scope uses:

| Bucket                        | Name                           |
| ----------------------------- | ------------------------------ |
| Global                        | `subscription-global`          |
| Fresh-input tokens per minute | `subscription-fresh-input-tpm` |
| Output tokens per minute      | `subscription-output-tpm`      |

## The classification signal

The signal rides the **request-time** quota filter
(`envoy.filters.http.ratelimit/ai-gateway-quota-request`). The stream-completion
filter is fire-and-forget and unordered against the access log, so an outcome
reported there may not reach the record.

The rate limit service returns it in `RateLimitResponse.dynamic_metadata`, which
Envoy stores in the dynamic metadata namespace `envoy.filters.http.ratelimit`.
The service must run with `RESPONSE_DYNAMIC_METADATA=true`.

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
| `""` (present, empty)          | every subscription bucket within budget — in-plan                  |
| e.g. `subscription-output-tpm` | that bucket was over budget — spillover                            |
| absent / null                  | no signal arrived — the event is classified unknown, never guessed |

The gateway reports bucket names only. Mapping a set of bucket names onto a
commercial lane happens where the usage event is assembled, not here. The
client-facing response carries neither: it keeps only the numeric remaining-budget
headers.

### Not yet shipped

`shadowModeViolations` does not exist yet. The rate limit service rewrites a
shadow bucket's over-limit result to allowed inside its limiter, before the layer
that assembles response metadata can see it, so the fact is destroyed at its
source. Recording it there is a change to the rate limit service, which this
fork consumes as a pinned upstream image and does not build. Until that lands,
requests are correctly served and classified as unknown rather than misclassified.
