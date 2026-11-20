package main

import (
	"testing"
)

func TestControlPlanePoolConfigSetsTimeouts(t *testing.T) {
	cfg, err := controlPlanePoolConfig("postgres://u:p@127.0.0.1:5432/app?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	rp := cfg.ConnConfig.RuntimeParams
	if rp["lock_timeout"] != "10000" || rp["statement_timeout"] != "60000" {
		t.Fatalf("runtime params = %v", rp)
	}
}

func TestControlPlanePoolConfigKeepsExplicitDSNValues(t *testing.T) {
	cfg, err := controlPlanePoolConfig("postgres://u:p@127.0.0.1:5432/app?sslmode=disable&lock_timeout=3000")
	if err != nil {
		t.Fatal(err)
	}
	rp := cfg.ConnConfig.RuntimeParams
	if rp["lock_timeout"] != "3000" || rp["statement_timeout"] != "60000" {
		t.Fatalf("runtime params = %v", rp)
	}
}
