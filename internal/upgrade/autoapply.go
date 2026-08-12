package upgrade

import (
	"fmt"
	"strings"

	"github.com/nova-archive/nova/internal/db/migrations"
)

// The entrypoint's auto-apply decision (P2-M7.3, D-M7.3-9a).
//
// Nova's coordinator entrypoint has always run `migrate up` unattended. That is
// fine for the additive migrations it has seen so far and wrong for the ones it
// has not: 0003 drops two tables, 0009 takes a write lock on a corpus-scale
// index. An unattended container is not a person who read the release notes.
//
// So the entrypoint auto-applies only when the WHOLE range is safe on every
// dimension, and stops with the exact command to run otherwise. Safe is a
// conjunction, not a majority.

// AutoApplyDecision is the answer, with the reason it was reached. The reason
// is not decoration: the operator sees it in container logs and has to be able
// to act on it without reading this source.
type AutoApplyDecision struct {
	Apply bool
	// Reason states why, in one sentence.
	Reason string
	// Command is what the operator should run when Apply is false. Empty when
	// there is nothing to do.
	Command string
}

// AutoApplicable decides whether a range may be applied unattended.
//
// Every dimension must permit it:
//
//   - OldBinaryCompatible — a container that restarts on the old image must
//     still work, because that is what a failed deploy looks like;
//   - OnlineApplicable and !RequiresMaintenance — an unattended apply has no
//     window and no one watching;
//   - !RestoreToRevert — nothing that can only be undone from a backup runs
//     without a person who knows a backup exists;
//   - no procedures — an instruction nobody read is an instruction nobody
//     followed.
//
// A BootstrapOnly range is a fresh install: there is no old binary and nothing
// to revert to, so 0001's vacuous obligations must not block one.
func AutoApplicable(b migrations.Boundary) AutoApplyDecision {
	if b.From == b.To {
		return AutoApplyDecision{Apply: false, Reason: "the schema is already at the target"}
	}

	parts := []string{fmt.Sprintf("migrate apply --to %d", b.To)}
	for _, p := range b.Procedures {
		parts = append(parts, "--acknowledge "+ObligationID(p))
	}
	cmd := strings.Join(parts, " ")

	if b.BootstrapOnly && b.From == 0 {
		// A fresh install. 0001 declares RestoreToRevert and no compatible
		// predecessor because reverting it means an empty database — true, and
		// irrelevant when there is nothing to revert TO. Blocking here would
		// mean no Nova deployment could ever start unattended.
		return AutoApplyDecision{Apply: true,
			Reason: fmt.Sprintf("fresh install: applying the full range (0, %d]", b.To)}
	}

	var blockers []string
	if !b.OldBinaryCompatible {
		blockers = append(blockers, fmt.Sprintf(
			"the range is not compatible with %s, so a restart on the previous image would "+
				"fail against the new schema", strings.Join(b.RelativeTo, ", ")))
	}
	if !b.OnlineApplicable {
		blockers = append(blockers, "it cannot be applied while the coordinator serves")
	}
	if b.RequiresMaintenance {
		blockers = append(blockers, "it needs a maintenance window, and an unattended container "+
			"cannot schedule one")
	}
	if b.RestoreToRevert {
		blockers = append(blockers, "reverting it requires restoring from backup")
	}
	if len(b.Procedures) > 0 {
		blockers = append(blockers, fmt.Sprintf("it requires %d operator procedure(s)",
			len(b.Procedures)))
	}

	if len(blockers) == 0 {
		return AutoApplyDecision{Apply: true,
			Reason: fmt.Sprintf("(%d, %d] is old-binary-compatible, online and maintenance-free",
				b.From, b.To)}
	}
	return AutoApplyDecision{
		Apply:   false,
		Reason:  "not applying unattended because " + strings.Join(blockers, "; "),
		Command: cmd,
	}
}

// ParseBool is the strict boolean the entrypoint uses.
//
// `strconv.ParseBool` would be fine; `os.Getenv(x) == "true"` is what tends to
// get written instead, and under it NOVA_MIGRATE_ON_START=flase silently means
// false — the operator believes they turned auto-apply ON and the container
// quietly stops migrating. A typo has to be an error, not a default.
func ParseBool(name, raw string, fallback bool) (bool, error) {
	if raw == "" {
		return fallback, nil
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "t", "true", "yes", "y", "on":
		return true, nil
	case "0", "f", "false", "no", "n", "off":
		return false, nil
	default:
		return false, fmt.Errorf("%s=%q is not a boolean. Accepted: true/false, yes/no, on/off, "+
			"1/0. Refusing rather than guessing: a typo that reads as `false` turns off the "+
			"thing you thought you turned on", name, raw)
	}
}
