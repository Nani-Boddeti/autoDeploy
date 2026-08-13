//go:build linux

// checkout-quota-service is deliberately not a general quota management CLI.
// It is started by the host supervisor with a fixed root, socket, state file,
// agent UID and bounds. It serves only the descriptor-based quota protocol.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"

	"autodeploy/internal/checkoutquota"
)

func main() {
	initialize := flag.Bool("initialize", false, "explicitly create a pristine quota ledger")
	initialProjectID := flag.Uint("initial-project-id", 0, "required nonzero project-ID high-water floor with -initialize")
	flag.Parse()
	if flag.NArg() != 9 || (*initialize && (*initialProjectID == 0 || uint64(*initialProjectID) > uint64(^uint32(0)))) || (!*initialize && *initialProjectID != 0) {
		fmt.Fprintln(os.Stderr, "invalid checkout quota service configuration")
		os.Exit(2)
	}
	args := flag.Args()
	agentUID, e1 := strconv.ParseUint(args[3], 10, 32)
	agentGID, e2 := strconv.ParseUint(args[4], 10, 32)
	checkoutUID, e3 := strconv.ParseUint(args[5], 10, 32)
	checkoutGID, e4 := strconv.ParseUint(args[6], 10, 32)
	bytes, e5 := strconv.ParseUint(args[7], 10, 64)
	inodes, e6 := strconv.ParseUint(args[8], 10, 64)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil || e5 != nil || e6 != nil || agentUID == 0 || agentGID == 0 || checkoutUID == 0 || checkoutGID == 0 || agentUID == checkoutUID || agentGID == checkoutGID {
		fmt.Fprintln(os.Stderr, "invalid checkout quota service configuration")
		os.Exit(2)
	}
	svc, err := checkoutquota.New(checkoutquota.Config{CheckoutRoot: args[0], SocketPath: args[1], StatePath: args[2], AgentUID: uint32(agentUID), AgentGID: uint32(agentGID), CheckoutUID: uint32(checkoutUID), CheckoutGID: uint32(checkoutGID), MaxBytes: bytes, MaxInodes: inodes, Initialize: *initialize, InitialProjectID: uint32(*initialProjectID)})
	if err != nil {
		fmt.Fprintln(os.Stderr, "checkout quota service unavailable")
		os.Exit(1)
	}
	if *initialize {
		// Initialization is an operator-only, one-shot action. It deliberately
		// does not leave a listening privileged service behind.
		return
	}
	if err = svc.Serve(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "checkout quota service unavailable")
		os.Exit(1)
	}
}
