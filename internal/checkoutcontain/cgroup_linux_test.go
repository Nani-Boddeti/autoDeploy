//go:build linux

package checkoutcontain

import (
	"strings"
	"testing"
)

func TestLimitsAndLeafNamesAreBounded(t *testing.T) {
	if validLimits(Limits{}) || validLimits(Limits{MemoryMax: 1, PIDsMax: 1, IOMax: 1, CPUQuota: 2, CPUPeriod: 1}) {
		t.Fatal("invalid limits accepted")
	}
	if !validLimits(Limits{MemoryMax: 1, PIDsMax: 1, IOMax: 1, CPUQuota: 1, CPUPeriod: 1}) {
		t.Fatal("valid limits rejected")
	}
	for _, name := range []string{"", ".", "..", "a/b", "a\x00b"} {
		if validLeafName(name) {
			t.Fatalf("unsafe name accepted: %q", name)
		}
	}
	if !validLeafName("checkout-0123456789abcdef") {
		t.Fatal("safe name rejected")
	}
}
func TestControlReadbackCanonicalization(t *testing.T) {
	if canonicalControl("cpu.max", "10 100\n") != "10 100" {
		t.Fatal("cpu canonicalization")
	}
	want := "8:0 rbps=11 wbps=12"
	if canonicalControl("io.max", "8:0 wbps=12 rbps=11") != want {
		t.Fatal("io order not canonical")
	}
	if canonicalControl("io.max", "8:0 rbps=11 wbps=12 riops=1") != "" {
		t.Fatal("extra io limit accepted")
	}
	if !eventsPopulatedZero("populated 0\nfrozen 0\n") || eventsPopulatedZero("populated 1\n") {
		t.Fatal("events parsing")
	}
}

func TestUniqueLeafNamesAreOpaqueAndValid(t *testing.T) {
	one, err := uniqueLeafName("checkout")
	if err != nil || !validLeafName(one) || !strings.HasPrefix(one, "checkout-") || len(one) != len("checkout-")+16 {
		t.Fatalf("invalid generated leaf %q: %v", one, err)
	}
	two, err := uniqueLeafName("checkout")
	if err != nil || one == two {
		t.Fatal("leaf names are not collision-resistant")
	}
}
