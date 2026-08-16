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

A bucket's name and its override header are two different vocabularies and do not
read alike. The names below are the signal's vocabulary, chosen for what a
recorded outcome should say; the headers follow the authorization service's
existing `x-bf-*-quota-<metric>-<window>` grammar. The pairing is the contract:

| Bucket name                    | Override header                     | Counts        |
| ------------------------------ | ----------------------------------- | ------------- |
| `subscription-global`          | `x-bf-subscription-quota-req-1m`    | requests      |
| `subscription-fresh-input-tpm` | `x-bf-subscription-quota-intok-1m`  | input tokens  |
| `subscription-output-tpm`      | `x-bf-subscription-quota-outtok-1m` | output tokens |

Selector and overrides alike are stamped by the authorization service and
stripped before the request leaves the gateway; a client-supplied value never
survives. All three buckets sit on the same selector, so one subscription's three
counters move together.
`tests/crdcel/testdata/quotapolicies/subscription-shadow-buckets.yaml` is the
worked example.

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

The subscription scope's three names are listed with their override headers
above.

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

### What this file does not name

The lane is not named here. The authorization service stamps it on `x-bf-lane`,
already in production, and that header is the one lane name — degraded traffic
carries lane `4` on it today. Nothing in the quota path introduces a second lane
header or a second spelling of the concept, and nothing here reads one: this file
owns the bucket-level outcome, and the lane is derived from it downstream.

## Headers that must survive to be worth producing

`x-bf-subscription-id` and the three override headers above have to reach
Envoy's rate limit filter for these buckets to work. If one is dropped in between
— an authorization filter that does not forward it, a tier that strips it — the
failure is silent and asymmetric:

- selector missing → the descriptor is never generated, the bucket does not
  apply, and no outcome is reported: the request classifies unknown.
- override missing but selector present → the bucket silently falls back to its
  static limit, which is why the worked example sets that fallback to the
  protocol maximum. A low fallback there would report a request as over budget on
  a propagation failure rather than on real consumption.

Neither case rejects a request. Both are worth alerting on, because a bucket that
is configured, produced and consumed but dropped in transit looks exactly like a
subscription with no traffic.

### Not yet shipped

`shadowModeViolations` does not exist yet. The rate limit service rewrites a
shadow bucket's over-limit result to allowed inside its limiter, before the layer
that assembles response metadata can see it, so the fact is destroyed at its
source. Recording it there is a change to the rate limit service, which this
fork consumes as a pinned upstream image and does not build. Until that lands,
requests are correctly served and classified as unknown rather than
misclassified.

That change is tracked as BLA-3691, which builds to the field names and
vocabulary above rather than defining its own.
