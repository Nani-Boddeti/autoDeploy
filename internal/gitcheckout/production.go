package gitcheckout

import (
	"path/filepath"

	"autodeploy/internal/checkoutcontain"
	"autodeploy/internal/checkoutquota"
)

// ProductionConfig is the complete, fixed configuration for the unprivileged
// checkout caller. Both endpoints must be root-owned local helper sockets.
// It deliberately exposes no executable, checkout path, or generic command.
type ProductionConfig struct {
	QuotaSocketPath string
	SpawnSocketPath string
	AgentGID        uint32
	QuotaLimits     checkoutquota.Limits
	SpawnLimits     checkoutcontain.SpawnLimits
}

// NewProductionRunner wires the only production implementations of the
// privileged boundaries. Unsupported platforms remain fail-closed because the
// concrete clients return their unavailable errors; there is no local fallback.
func NewProductionRunner(c ProductionConfig) (*Runner, error) {
	if !validProductionConfig(c) {
		return nil, ErrInvalidConfig
	}
	return NewRunner(RunnerConfig{
		Quota: checkoutquota.Client{
			SocketPath: c.QuotaSocketPath,
			AgentGID:   c.AgentGID,
		},
		Spawn: checkoutcontain.SpawnClient{
			SocketPath: c.SpawnSocketPath,
			AgentGID:   c.AgentGID,
			MaxOutput:  c.SpawnLimits.MaxOutput,
		},
		QuotaLimits: c.QuotaLimits,
		SpawnLimits: c.SpawnLimits,
	})
}

func validProductionConfig(c ProductionConfig) bool {
	return c.AgentGID != 0 &&
		filepath.IsAbs(c.QuotaSocketPath) &&
		filepath.IsAbs(c.SpawnSocketPath) &&
		c.QuotaSocketPath != c.SpawnSocketPath &&
		c.QuotaLimits.Bytes != 0 && c.QuotaLimits.Inodes != 0 &&
		c.SpawnLimits.TimeoutSeconds != 0 && c.SpawnLimits.TimeoutSeconds <= 300 &&
		c.SpawnLimits.MaxOutput != 0 && c.SpawnLimits.MaxOutput <= 1<<20
}
