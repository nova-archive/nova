package main

import (
	"errors"
	"flag"
)

// checkFlagsOnly makes every subcommand stop immediately after its flags parse
// successfully, doing no work and touching nothing.
//
// It exists for the docs-cli-parse gate (P2-M7.2, D-M7.2-10). Operator
// documentation shipped commands the binary could not execute — `node ca-init
// --out-ca-cert`, `node issue --node-id`, `node nebula-template --node-id` —
// none of which exist. That is only discoverable by running them, so CI runs
// every documented invocation through this mode.
var checkFlagsOnly bool

// errCheckFlagsOK signals "flags parsed, stopping before any work". main()
// treats it as success.
var errCheckFlagsOK = errors.New("novactl: --check-flags: ok")

// parseFlags is the single parse policy for every subcommand. Call it instead
// of fs.Parse so --check-flags reaches all of them without 26 separate guards.
func parseFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	if checkFlagsOnly {
		return errCheckFlagsOK
	}
	return nil
}
