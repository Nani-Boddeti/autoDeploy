package postgres

import "strings"

// Release-operation states that close an activation. They are the ONLY
// states the rule below names.
const (
	activationStateSucceeded = "succeeded"
	activationStateFailed    = "failed"
)

// activationStateSwitchAuthorized is the SOURCE state of the block routine's
// transition (switch_authorized -> reconciliation_required, see
// blockActivationForReconciliation). Naming a single open state is correct
// HERE, and only here, because it is a transition's source state, not a
// stand-in for "unresolved" (that rule is unresolvedActivationSQL and stays
// terminal-based). Every place that asks "can the block be recorded from this
// state?" must use this constant: the block UPDATE's WHERE, the sweeper
// diversion's no-op branch, recordBlockInFreshTransaction's guard, and
// ExpireBatch's discovery exclusion (which excludes an unresolved operation
// whose state is NOT this one, i.e. exactly "the diversion would be a
// no-op"). It must stay in step with the block routine's UPDATE: if that
// UPDATE ever accepts another source state, this changes with it.
const activationStateSwitchAuthorized = "switch_authorized"

// activationTerminalStates is the single definition of "resolved".
var activationTerminalStates = []string{activationStateSucceeded, activationStateFailed}

// unresolvedActivationSQL is the SQL form of unresolvedActivation, for WHERE
// clauses over release_operations (unqualified kind/state columns). It is
// generated from activationTerminalStates, so the Go and SQL forms cannot
// drift (TestUnresolvedActivationSQLAgreesWithGo asserts it for every state in
// release_operations_state_check). It matches migration 000021's unique index
// predicate.
//
// The whole fragment is parenthesized so it is safe to concatenate after OR as
// well as AND.
var unresolvedActivationSQL = "(kind='activate' AND state NOT IN (" + quotedList(activationTerminalStates) + "))"

// unresolvedActivation reports whether an activate operation in the given
// state is still unresolved: the host may have switched, or may still switch,
// and the environment must not be touched until it resolves.
//
// The rule is stated over the TERMINAL states, not over the open ones. Today
// switch_authorized and reconciliation_required are both unresolved, but
// enumerating them is the defect this predicate exists to remove: a guard
// naming one open state silently misses the other, and an open state added
// later would be silently missed by an open-list. Deriving "unresolved" from
// "not terminal" makes any new non-terminal state unresolved by default.
func unresolvedActivation(state string) bool {
	for _, terminal := range activationTerminalStates {
		if state == terminal {
			return false
		}
	}
	return true
}

func quotedList(values []string) string {
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = "'" + v + "'"
	}
	return strings.Join(quoted, ",")
}
