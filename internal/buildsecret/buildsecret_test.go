package buildsecret

import "testing"

func TestIDIndependentOfInputOrder(t *testing.T) {
	orders := [][]string{{"B", "A", "C"}, {"C", "B", "A"}, {"A", "C", "B"}}
	want := map[string]string{"A": "build-secret-0", "B": "build-secret-1", "C": "build-secret-2"}
	for _, names := range orders {
		for n, w := range want {
			if got, ok := ID(names, n); !ok || got != w {
				t.Fatalf("ID(%v,%q)=%q,%v want %q", names, n, got, ok, w)
			}
		}
	}
}

func TestIDShiftsWhenEarlierNameAdded(t *testing.T) {
	before := []string{"NPM_TOKEN", "PIP_TOKEN"}
	after := []string{"NPM_TOKEN", "PIP_TOKEN", "AAA_FIRST"}
	if id, _ := ID(before, "NPM_TOKEN"); id != "build-secret-0" {
		t.Fatalf("before: %q", id)
	}
	if id, _ := ID(after, "NPM_TOKEN"); id != "build-secret-1" {
		t.Fatalf("after: %q", id)
	}
	if id, _ := ID(after, "AAA_FIRST"); id != "build-secret-0" {
		t.Fatalf("first: %q", id)
	}
	// A name sorting last does not shift the others.
	if id, _ := ID(append(before, "ZZZ"), "NPM_TOKEN"); id != "build-secret-0" {
		t.Fatalf("append last shifted: %q", id)
	}
}

func TestIDByteOrderAndUnknown(t *testing.T) {
	names := []string{"a", "B", "_x"} // byte order: B(0x42) < _(0x5f) < a(0x61)
	for n, w := range map[string]string{"B": "build-secret-0", "_x": "build-secret-1", "a": "build-secret-2"} {
		if got, _ := ID(names, n); got != w {
			t.Fatalf("%q=%q want %q", n, got, w)
		}
	}
	if _, ok := ID(names, "missing"); ok {
		t.Fatal("unknown name must not get an id")
	}
}

func TestIDDoesNotMutateInput(t *testing.T) {
	names := []string{"B", "A"}
	ID(names, "A")
	if names[0] != "B" || names[1] != "A" {
		t.Fatalf("input mutated: %v", names)
	}
}
