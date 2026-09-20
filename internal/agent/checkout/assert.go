package checkout

import "autodeploy/internal/gitcheckout"

// *Lease must keep satisfying gitcheckout.Lease; a break on either side
// fails the build here rather than at cmd/agent's call site, mirroring the
// assertion gitcheckout.go itself carries for *githubapp.CheckoutLease.
var _ gitcheckout.Lease = (*Lease)(nil)
