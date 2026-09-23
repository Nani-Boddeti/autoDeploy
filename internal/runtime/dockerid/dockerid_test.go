package dockerid

import (
	"strings"
	"testing"
)

func TestContainerNameValid(t *testing.T) {
	name, err := ContainerName("attempt-1234")
	if err != nil {
		t.Fatalf("ContainerName: %v", err)
	}
	if !strings.HasPrefix(name, ContainerNamePrefix) {
		t.Fatalf("name %q missing prefix %q", name, ContainerNamePrefix)
	}
}

func TestContainerNameRejectsInvalidID(t *testing.T) {
	for _, bad := range []string{"", "../etc/passwd", "has space", "semi;colon"} {
		if _, err := ContainerName(bad); err == nil {
			t.Fatalf("ContainerName(%q) succeeded, want rejection", bad)
		}
	}
}

func TestAttemptRuntimeInstanceIDIsDecimal(t *testing.T) {
	if got := AttemptRuntimeInstanceID(42); got != "42" {
		t.Fatalf("AttemptRuntimeInstanceID(42) = %q, want %q", got, "42")
	}
}
