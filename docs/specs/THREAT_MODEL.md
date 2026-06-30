
---

## P2-M5 amendment — failure-domain & repair-forgery defenses now code-enforced

**Sybil / failure-domain concentration.** Placement anti-affinity and the
concentration metrics (per-node Gini, per-dimension largest-share / normalized
entropy) trust a dimension value **only** when `nodes.operator_verified_at` is set;
NULL/unverified values collapse into a single `unknown` bucket **before** grouping, so
a donor cannot manufacture diversity by self-declaring `failure_domain`/`provider`/
`asn`/`region`. Probationary/unverified donors cannot be the sole or second copy of
`important` data (T1.30). `federation.concentrated` / `.homogeneous` surface skew to
the operator (placement is never refused purely for homogeneity).

**Repair-grant forgery / replay / misrouting.** A donor↔donor repair grant is an
Ed25519 token bound to (`source_node_id`, `dest_node_id`, `cid`, source assignment
generation, `max_bytes`, `jti`, `not_before/after`). The source server verifies the
signature against the coordinator's current public key, that **it** is the named
source, that the **caller's verified cert** is the named dest (a grant minted for
donor B cannot be replayed by donor C), boot-floor + single-use `jti`, and streams
exactly `byte_size`. The destination additionally refuses a grant whose `dest_*`
binding does not match the change it is processing **before** fetching — no ack/fail
ambiguity. A short/corrupt source body fails the destination's re-import CID verify ⇒
no ack.
