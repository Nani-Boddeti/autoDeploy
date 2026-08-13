package gitcheckout

import (
	"errors"
	"testing"

	"autodeploy/internal/checkoutcontain"
	"autodeploy/internal/checkoutquota"
)

func TestNewProductionRunnerValidatesAndWiresConcreteClients(t *testing.T) {
	c := ProductionConfig{
		QuotaSocketPath: "/run/autodeploy/checkout-quota.sock",
		SpawnSocketPath: "/run/autodeploy/checkout-exec.sock",
		AgentGID:        1234,
		QuotaLimits:     checkoutquota.Limits{Bytes: 1, Inodes: 1},
		SpawnLimits:     checkoutcontain.SpawnLimits{TimeoutSeconds: 1, MaxOutput: 1},
	}
	r, err := NewProductionRunner(c)
	if err != nil {
		t.Fatalf("NewProductionRunner: %v", err)
	}
	if _, ok := r.config.Quota.(checkoutquota.Client); !ok {
		t.Fatalf("quota client type = %T", r.config.Quota)
	}
	if _, ok := r.config.Spawn.(checkoutcontain.SpawnClient); !ok {
		t.Fatalf("spawn client type = %T", r.config.Spawn)
	}
	for _, bad := range []ProductionConfig{
		{},
		{QuotaSocketPath: "relative", SpawnSocketPath: c.SpawnSocketPath, AgentGID: c.AgentGID, QuotaLimits: c.QuotaLimits, SpawnLimits: c.SpawnLimits},
		{QuotaSocketPath: c.QuotaSocketPath, SpawnSocketPath: c.QuotaSocketPath, AgentGID: c.AgentGID, QuotaLimits: c.QuotaLimits, SpawnLimits: c.SpawnLimits},
		{QuotaSocketPath: c.QuotaSocketPath, SpawnSocketPath: c.SpawnSocketPath, QuotaLimits: c.QuotaLimits, SpawnLimits: c.SpawnLimits},
		{QuotaSocketPath: c.QuotaSocketPath, SpawnSocketPath: c.SpawnSocketPath, AgentGID: c.AgentGID, SpawnLimits: c.SpawnLimits},
		{QuotaSocketPath: c.QuotaSocketPath, SpawnSocketPath: c.SpawnSocketPath, AgentGID: c.AgentGID, QuotaLimits: c.QuotaLimits, SpawnLimits: checkoutcontain.SpawnLimits{TimeoutSeconds: 301, MaxOutput: 1}},
	} {
		if _, err := NewProductionRunner(bad); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("invalid config error = %v", err)
		}
	}
}
