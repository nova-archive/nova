package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/release"
)

// `novactl node rollout authorize` (P2-M7.3, D-M7.3-6b).
//
// Migration 0019 gave every node an EXPECTED image digest and bundle-lock
// digest — what the operator authorized. Storage without a command is inert:
// nothing could write those columns, so the two-party rollout could not begin
// and the census had nothing to compare a donor's claim against.
//
// # The lock arrives as a DIGEST, not a path
//
// `--lock <file>` means nothing across a process boundary: a path is not an
// identity, and a caller who can choose the path can choose the contents. So
// this command takes the digest that reached the operator through the
// authenticated chain (out-of-band cosign verify-blob → the lock), re-hashes
// the file it was given, and refuses on any difference.
//
// The intent is checked the same way, against the lock's own payload map —
// which is the acyclic authentication graph doing its job rather than a second
// signature to manage.
//
// # Two parties
//
// This is operator-side only. The volunteer's update script holds no
// coordinator admin authority and never runs it; the sequence is: operator
// authorizes → operator drains → volunteer applies → donor reports its
// configured declaration → operator confirms match and health → operator
// undrains.

func cmdNodeRollout(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: novactl node rollout authorize --id <uuid> --lock <file> " +
			"--intent <file> --expect-lock-digest <sha256:...>")
	}
	switch args[0] {
	case "authorize":
		return cmdNodeRolloutAuthorize(args[1:])
	default:
		return fmt.Errorf("novactl node rollout: unknown subcommand %q", args[0])
	}
}

func cmdNodeRolloutAuthorize(args []string) error {
	fs := flag.NewFlagSet("node rollout authorize", flag.ContinueOnError)
	idStr := fs.String("id", "", "node id (uuid)")
	lockPath := fs.String("lock", "", "release lock file from the verified bundle")
	intentPath := fs.String("intent", "", "release intent file from the same bundle")
	expectDigest := fs.String("expect-lock-digest", "",
		"the lock digest you verified out of band (sha256:...); a path is not an identity")
	actor := fs.String("actor", "", "who is authorizing (defaults to $USER)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	pgID, err := parsePGUUID(*idStr)
	if err != nil {
		return err
	}
	for name, v := range map[string]string{
		"--lock": *lockPath, "--intent": *intentPath, "--expect-lock-digest": *expectDigest,
	} {
		if v == "" {
			return fmt.Errorf("%s is required", name)
		}
	}

	lock, err := loadVerifiedLock(*lockPath, *intentPath, *expectDigest)
	if err != nil {
		return err
	}

	node, ok := lock.Artifacts["nova-node"]
	if !ok {
		return errors.New("the lock carries no nova-node artifact; there is nothing to roll out")
	}
	imageDigest := node.Descriptor.Digest.String()

	who := *actor
	if who == "" {
		who = os.Getenv("USER")
	}
	if who == "" {
		who = "unknown"
	}

	return withNodeDB(func(ctx context.Context, q *gen.Queries) error {
		// Both digests are set TOGETHER: a rollout authorizes one topology, and
		// half-setting it would leave the census comparing a donor against a
		// mixture of two releases.
		if err := q.SetNodeExpectedArtifact(ctx, gen.SetNodeExpectedArtifactParams{
			ID:                       pgID,
			ExpectedImageDigest:      pgText(imageDigest),
			ExpectedBundleLockDigest: pgText(lock.DonorLockDigest),
			ExpectedBy:               pgText(who),
		}); err != nil {
			return err
		}

		// T1.24: authorizing what a donor may run is a privileged action.
		payload, _ := json.Marshal(map[string]any{
			"release":            lock.Version,
			"lock_digest":        *expectDigest,
			"image_digest":       imageDigest,
			"bundle_lock_digest": lock.DonorLockDigest,
			"actor":              who,
		})
		if err := q.InsertAuditLog(ctx, gen.InsertAuditLogParams{
			Action:     "node.rollout.authorize",
			TargetType: "node",
			TargetID:   *idStr,
			Payload:    payload,
		}); err != nil {
			return err
		}

		fmt.Printf("authorized %s for node %s\n", lock.Version, *idStr)
		fmt.Printf("  image digest:       %s\n", imageDigest)
		fmt.Printf("  bundle lock digest: %s\n", lock.DonorLockDigest)
		fmt.Println("next: drain the node, send the volunteer the converted bundle, and confirm")
		fmt.Println("      `novactl node list` reports the expected digests before undraining")
		return nil
	})
}

// loadVerifiedLock re-establishes the authenticated chain locally.
//
//  1. The lock's bytes must hash to the digest the operator verified out of
//     band. Anything else is a different document.
//  2. The intent's bytes must match the hash the lock records for them, which
//     is the lock's payload map doing the work a second signature would
//     otherwise have to.
//  3. Only then is the lock parsed and validated against that intent.
func loadVerifiedLock(lockPath, intentPath, expectDigest string) (release.Lock, error) {
	lockBytes, err := os.ReadFile(lockPath)
	if err != nil {
		return release.Lock{}, err
	}
	if got := release.LockDigest(lockBytes); got != expectDigest {
		return release.Lock{}, fmt.Errorf(
			"the lock at %s hashes to %s, not the %s you verified; refusing to authorize a "+
				"document you did not check", lockPath, got, expectDigest)
	}

	intentBytes, err := os.ReadFile(intentPath)
	if err != nil {
		return release.Lock{}, err
	}
	in, err := release.ParseIntent(intentBytes)
	if err != nil {
		return release.Lock{}, err
	}

	// Parse first so the payload map is available, then check the intent
	// against it and re-validate. ParseLock is where the structural rules live.
	lock, err := release.ParseLock(lockBytes, in, intentBytes)
	if err != nil {
		return release.Lock{}, err
	}
	wantIntent, ok := lock.Payload[release.MemberIntent]
	if !ok {
		return release.Lock{}, fmt.Errorf("the lock does not cover %s", release.MemberIntent)
	}
	if got := release.IntentDigest(intentBytes); got != wantIntent {
		return release.Lock{}, fmt.Errorf(
			"the intent at %s hashes to %s but the verified lock records %s", intentPath, got, wantIntent)
	}
	return lock, nil
}

// pgText is the local pgtype.Text helper (novactl has no shared one).
func pgText(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }
