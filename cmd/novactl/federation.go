package main

import "fmt"

// cmdFederation dispatches `novactl federation <subcommand>` — the operator's
// federation bootstrap and diagnosis surface (P2-M7.2).
func cmdFederation(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: novactl federation <init|doctor|compose-policy>")
	}
	switch args[0] {
	case "compose-policy":
		return cmdFederationComposePolicy(args[1:])
	case "--help", "-h", "help":
		fmt.Println("usage: novactl federation <init|doctor|compose-policy>")
		fmt.Println()
		fmt.Println("  init            bootstrap or adopt the federation PKI and overlay identity")
		fmt.Println("  doctor          prove readiness before issuing any donor invite")
		fmt.Println("  compose-policy  verify CA custody in a rendered compose definition")
		return nil
	default:
		return fmt.Errorf("novactl federation: unknown subcommand %q", args[0])
	}
}
