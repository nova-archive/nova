# Releasing Nova

How a Nova release is cut, who approves it, what the workflow refuses, and what
has to be configured on GitHub for any of it to work.

This document is for the operator who dispatches the release. `docs/UPGRADING.md`
is for the operator who consumes one.

---

## The shape of it, in one paragraph

Feature work merges to `main` and CI tests it. **Nothing publishes
automatically.** When a release is wanted, its reviewed intent is merged as a
normal PR, and then a human manually dispatches `release.yml` for an exact
commit and version. The workflow builds three images once, signs and attests
them, runs the gates the intent's claims require **against those exact images**,
assembles and signs the lock, and stops at a protected environment for a human
to approve. Approval promotes the version tags, creates the Git tag, publishes
the release, and then runs the post-publication transition gate — which does not
change the release whatever it concludes.

Manual is a decision, not an omission. Nova's schema, its federation protocol
and its mixed-donor compatibility window mean a release is a set of
**compatibility promises** to volunteers running software the operator does not
control. A promise nobody chose to make is not one worth keeping.

---

## The four completion states

A published release is not a finished milestone. The ROADMAP tracks four states
and each needs its own recorded commit:

| State | Reached when | Recorded by |
|---|---|---|
| 1 — code complete | the milestone's work is merged | the merge |
| 2 — RC verified | the `lock` job succeeds; evidence exists for every claim | a commit recording the evidence statement digests |
| 3 — release published | `promote` succeeds | a commit recording the lock digest and published descriptors |
| 4 — transition evidence | the post-publication gate passes | a commit flipping the ROADMAP row to ✅ |

State 4 is separate because it **cannot** be folded back. The lock is signed
before publication; re-cutting it to add a post-publication result would make
the signature cover a different set of claims than the one that was reviewed.

---

## Per-release procedure

### 1. Merge the feature work

Normally. CI runs, nothing publishes.

### 2. Prepare the release PR

One PR containing:

- `releases/intent/vX.Y.Z.json` — the reviewed decision. Claims and the gate
  that must prove each, target schema, platforms, supported predecessors,
  capability profiles, digest-pinned sidecars. **No digests of Nova's own
  artifacts**: they do not exist yet, and stamping the version changes them.
- Regenerated `internal/release/catalog/catalog_gen.go` — `make catalog`.
- Regenerated compatibility matrix in `docs/UPGRADING.md` — `make release-docs`.
- Any migration obligations for new migrations.
- Release notes, if they are not the UPGRADING.md entry.

Before pushing, rehearse offline:

```bash
make release-gates
```

That runs the whole document pipeline for both the real intent and a synthetic
second release, interrupts every external write and asserts a retry converges,
and lints the workflow with a digest-pinned actionlint. It is hermetic — no
Docker, no network — and it is where a broken intent should be discovered.

### 3. Merge and let CI finish

Wait for every required check on `main`. The release workflow re-runs the static
gates, but it does not re-run the full suite, and a red `main` is not a release
candidate.

### 4. Rehearse on GitHub

Actions → **release** → *Run workflow*. Select **`main`** as the branch — not the
candidate commit, not a tag — and supply:

| Input | Value |
|---|---|
| `candidate_sha` | the full 40-character SHA now on `main` |
| `version` | `vX.Y.Z`, matching the checked-in intent |
| `dry_run` | **true** |
| `transition_runner` | leave as `ubuntu-latest` unless the gate needs a self-hosted label |

The branch selector matters. A `workflow_dispatch` run may execute from any
branch the workflow exists on, and GitHub's `workflow_ref` — the Cosign
certificate identity — carries the ref as well as the path. A run from anywhere
else mints certificates under an identity no operator's policy admits, on images
that look released. The workflow's first step refuses, before the checkout.

**`dry_run: true` is a rehearsal, not a no-op.** It pushes candidate images to a
candidate tag, signs them, and mints real Sigstore certificates that are in the
public transparency log permanently. What it skips is promotion, the Git tag and
publication — the three writes that constitute "a release exists". For a
rehearsal that touches nothing at all, use `make release-gates`.

### 5. Dispatch for real

Same inputs, `dry_run: false`.

### 6. Approve

The run pauses at the protected `release` environment. Before approving, read
the **run summary** the `lock` job wrote. It carries, in this order:

- the lock digest, the intent digest, the donor-lock digest, the source commit;
- every artifact's full descriptor — repository, digest, media type, size,
  platforms;
- every claim, its gate, its runner tier, its outcome, and the statement digest
  it is bound to;
- what each gate exercised: scenarios, capabilities, duration;
- what approving does.

The question you are answering is: *are these the artifacts, and is there
evidence behind every claim?* Everything needed to answer it is on that page.

### 7. Watch it finish

Promotion, tag, publication, then the post-publication transition gate — all in
the same run.

The transition gate is in this run deliberately. An `on: release: published`
workflow would not fire at all: events created with `GITHUB_TOKEN` do not
trigger further workflow runs, so the gate would silently never run and the
milestone would sit at state 3 looking finished.

### 8. If the transition gate fails

- **The release stays published.** It is real and verified; what has not been
  demonstrated is the transition to it.
- Completion remains **state 3**.
- Re-run **only** that job. Everything before it is done.
- The lock is **never** re-cut.

### 9. Record the ledger commits

States 2, 3 and 4 each get their own commit. The transition job prints the
statement digests for the state-4 commit in its summary.

---

## What the workflow refuses

Every one of these is a refusal rather than a warning, because the result of
getting it wrong is a signed document that says something untrue.

**Before anything is built**

- a run from any ref other than `refs/heads/main`;
- a `candidate_sha` that is not a full 40-character SHA, or is not an ancestor
  of `origin/main`;
- a version that is not canonical SemVer, or that already exists as a Git tag or
  a GitHub Release;
- a version with no checked-in intent, or an intent that does not validate;
- a compiled-in catalog that has drifted from the intent;
- **a claim bound to a gate whose coverage cannot prove it**, or to a
  post-publication gate;
- **any pre-publication gate whose coverage is still a placeholder** — the
  intended shape rather than an executed result.

**During the build**

- a signature, provenance attestation or **SBOM attestation** that does not
  verify under the operator-facing policy;
- an image whose published platform set is not exactly what the intent declares
  — in both directions. A missing platform is a donor that cannot run the
  release; an extra one is a platform the matrix never mentions and no gate
  exercised;
- an index with no child descriptors, or a child with no platform.

**At the lock**

- a claim with no evidence statement;
- evidence from a different gate, a cheaper runner tier, a skipped run, a
  different build, or **a different reviewed decision with the same version
  string**;
- evidence that does not record a capability its claim is conditional on;
- a payload map that covers the lock, its Sigstore bundle or its certificate —
  the cycle the whole authentication graph is shaped to avoid.

**At promotion and publication**

- an OCI version tag that already resolves to anything other than the locked
  descriptor;
- an OCI tag that **cannot be read** — an unreadable tag is not a missing tag,
  and creating it anyway is how a retry becomes an overwrite;
- a promoted tag whose read-back digest, media type or size differs from the
  locked one (this is `imagetools create` rewrapping a single-platform manifest
  in a new index, and it is why the lock records descriptors rather than
  digests);
- a Git tag that already points at another commit;
- a release that already exists targeting another commit;
- an attached asset whose bytes differ from the one being uploaded.

An asset whose upload never *completed* is replaced rather than refused: nobody
can have downloaded it.

---

## Retrying an interrupted release

Re-dispatch with the same inputs. Every external write is no-overwrite with
exact-match resume, and `make release-publish-injection` interrupts the sequence
at every point on every PR and asserts the retry converges to the same state an
uninterrupted run would have produced.

**Nothing rebuilds.** Promotion adds a tag to a digest that already exists; a
rebuild would produce a digest the lock never committed to. The fake `docker` in
the injection gate refuses to build for exactly this reason, so a resumed
release that tried to would fail on a laptop rather than in production.

---

## Required GitHub configuration

None of this can be expressed in YAML, and none of it can be tested locally.

**Branch protection on `main`** — required status checks, no force-push. The
workflow verifies the candidate is an ancestor of `origin/main`; that means
something only if `main` is protected.

**A `release` environment** with required reviewers, and "prevent self-review"
enabled where the team is larger than one. It is configured on the repository,
not in the workflow file, so it cannot be removed by editing the same file in
the same PR that would abuse it.

**Immutable releases**, so GitHub locks the tag and assets after publication.
Nova's own verification does not depend on this — the lock is signed and every
asset is hashed — but it removes a class of after-the-fact edit entirely.

**A `v*` tag ruleset** permitting the release workflow and blocking humans from
creating or moving release tags.

**`GITHUB_TOKEN` permissions** sufficient for `contents: write` (tag and
release), `packages: write` (OCI tags), `attestations: write`, and `id-token:
write` (OIDC, which is what makes the Cosign identity meaningful).

**All three GHCR packages linked to the repository and publicly readable.**
Donors pull anonymously; a private package makes the release undeployable for
exactly the people it is for. `nova-admin` is a new third package and may need a
one-time push before its visibility can be set.

---

## What cannot be proven before you push

OIDC, environment approval, GHCR permissions and immutable-release behaviour are
GitHub's, and no local harness reproduces them. The honest boundary is:

**complete locally → PR → CI → merge → rehearsal dispatch → production dispatch.**

Everything on the left of that boundary is covered by `make release-gates`.
Everything to the right of it is discovered by dispatching, which is why the
rehearsal exists and why `dry_run: true` is the default.

---

## Donors are never updated automatically

Publication makes a verified update *available*. Each donor operator chooses
when to take it, and the coordinator's compatibility window — declared in the
intent, exercised by `mixed-fleet-e2e` against every predecessor still inside it
— is what makes that choice safe rather than merely permitted.
