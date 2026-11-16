package healthgate

// ADR 0005's default HTTP health-gate policy, the single definition every
// layer uses (migration 000028's column defaults, the store, and later the
// web form and agent loop). internal/runtime/docker/health.go still holds its
// own copies until step 22-1d wires the frozen policy into the gate.
const (
	DefaultSuccessThreshold = 3
	DefaultIntervalSeconds  = 2
	DefaultTimeoutSeconds   = 2
	DefaultDeadlineSeconds  = 60
)

// CapabilityHealthPolicy is the claim-request capability name (wire value) by
// which an agent says it understands the lease's health_policy object. Only an
// agent that sends it is given a job whose frozen policy is non-default.
const CapabilityHealthPolicy = "health_policy"

// Bounds mirror migration 000028's CHECK constraints exactly.
const (
	MinSuccessThreshold = 1
	MaxSuccessThreshold = 10
	MinIntervalSeconds  = 1
	MaxIntervalSeconds  = 30
	MinTimeoutSeconds   = 1
	MaxTimeoutSeconds   = 30
	MinDeadlineSeconds  = 10
	MaxDeadlineSeconds  = 600
	MinTCPPort          = 1
	MaxTCPPort          = 65535
)

// Policy is one environment's tunable health-gate policy (step 22-1). The
// HTTP gate stays mandatory; TCPPort, when set, adds a TCP dial check.
// TCPPort 0 means "no TCP check" (stored as SQL NULL); 0 is never a valid
// port, so the zero value is unambiguous.
type Policy struct {
	SuccessThreshold int
	IntervalSeconds  int
	TimeoutSeconds   int
	DeadlineSeconds  int
	TCPPort          int
}

// DefaultPolicy returns ADR 0005's default policy (no TCP check).
func DefaultPolicy() Policy {
	return Policy{
		SuccessThreshold: DefaultSuccessThreshold,
		IntervalSeconds:  DefaultIntervalSeconds,
		TimeoutSeconds:   DefaultTimeoutSeconds,
		DeadlineSeconds:  DefaultDeadlineSeconds,
	}
}

// WithDefaults replaces each numeric field that is exactly 0 with its
// default, so a caller that never set a policy gets ADR 0005's behavior and
// never "0 successes / 0 seconds". Negative values are left untouched so
// they still fail Valid.
func (p Policy) WithDefaults() Policy {
	if p.SuccessThreshold == 0 {
		p.SuccessThreshold = DefaultSuccessThreshold
	}
	if p.IntervalSeconds == 0 {
		p.IntervalSeconds = DefaultIntervalSeconds
	}
	if p.TimeoutSeconds == 0 {
		p.TimeoutSeconds = DefaultTimeoutSeconds
	}
	if p.DeadlineSeconds == 0 {
		p.DeadlineSeconds = DefaultDeadlineSeconds
	}
	return p
}

// IsDefault reports whether the policy, after defaulting zero fields, equals
// DefaultPolicy. Digests append policy parts only when this is false.
func (p Policy) IsDefault() bool { return p.WithDefaults() == DefaultPolicy() }

func ValidSuccessThreshold(n int) bool { return n >= MinSuccessThreshold && n <= MaxSuccessThreshold }
func ValidIntervalSeconds(n int) bool  { return n >= MinIntervalSeconds && n <= MaxIntervalSeconds }
func ValidTimeoutSeconds(n int) bool   { return n >= MinTimeoutSeconds && n <= MaxTimeoutSeconds }
func ValidDeadlineSeconds(n int) bool  { return n >= MinDeadlineSeconds && n <= MaxDeadlineSeconds }

// ValidTCPPort reports whether port is a usable TCP port (1..65535). "No TCP
// check" is Policy.TCPPort == 0, handled by Policy.Valid, not here.
func ValidTCPPort(port int) bool { return port >= MinTCPPort && port <= MaxTCPPort }

// Valid mirrors migration 000028's CHECKs exactly and does NOT apply
// defaults: call WithDefaults first for input that may be unset.
func (p Policy) Valid() bool {
	return ValidSuccessThreshold(p.SuccessThreshold) &&
		ValidIntervalSeconds(p.IntervalSeconds) &&
		ValidTimeoutSeconds(p.TimeoutSeconds) &&
		ValidDeadlineSeconds(p.DeadlineSeconds) &&
		(p.TCPPort == 0 || ValidTCPPort(p.TCPPort)) &&
		p.TimeoutSeconds <= p.IntervalSeconds &&
		p.DeadlineSeconds >= p.SuccessThreshold*p.IntervalSeconds
}
