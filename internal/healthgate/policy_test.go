package healthgate

import "testing"

func TestDefaultsMatchADR0005(t *testing.T) {
	d := DefaultPolicy()
	want := Policy{SuccessThreshold: 3, IntervalSeconds: 2, TimeoutSeconds: 2, DeadlineSeconds: 60, TCPPort: 0}
	if d != want {
		t.Fatalf("DefaultPolicy() = %+v, want %+v", d, want)
	}
	if !d.IsDefault() || !d.Valid() {
		t.Fatalf("default policy must be default and valid: %+v", d)
	}
}

func TestWithDefaultsFillsOnlyZeroNumericFields(t *testing.T) {
	if got := (Policy{}).WithDefaults(); got != DefaultPolicy() {
		t.Fatalf("zero policy WithDefaults() = %+v, want defaults", got)
	}
	got := Policy{SuccessThreshold: 5, TCPPort: 80}.WithDefaults()
	want := Policy{SuccessThreshold: 5, IntervalSeconds: 2, TimeoutSeconds: 2, DeadlineSeconds: 60, TCPPort: 80}
	if got != want {
		t.Fatalf("partial WithDefaults() = %+v, want %+v", got, want)
	}
	// Negative values are never silently defaulted: they stay invalid.
	if (Policy{SuccessThreshold: -1}).WithDefaults().Valid() {
		t.Fatal("negative threshold must stay invalid after WithDefaults")
	}
}

func TestIsDefault(t *testing.T) {
	if !(Policy{}).IsDefault() {
		t.Fatal("zero policy is the default after defaulting")
	}
	for name, p := range map[string]Policy{
		"threshold": {SuccessThreshold: 4},
		"interval":  {IntervalSeconds: 3},
		"timeout":   {TimeoutSeconds: 1},
		"deadline":  {DeadlineSeconds: 61},
		"tcp":       {TCPPort: 80},
	} {
		if p.IsDefault() {
			t.Fatalf("%s: non-default policy reported default", name)
		}
	}
}

func TestFieldValidatorsBounds(t *testing.T) {
	cases := []struct {
		name     string
		fn       func(int) bool
		accepted []int
		rejected []int
	}{
		{"threshold", ValidSuccessThreshold, []int{1, 3, 10}, []int{-1, 0, 11}},
		{"interval", ValidIntervalSeconds, []int{1, 2, 30}, []int{-1, 0, 31}},
		{"timeout", ValidTimeoutSeconds, []int{1, 2, 30}, []int{-1, 0, 31}},
		{"deadline", ValidDeadlineSeconds, []int{10, 60, 600}, []int{-1, 0, 9, 601}},
		{"tcp port", ValidTCPPort, []int{1, 80, 65535}, []int{-1, 0, 65536}},
	}
	for _, c := range cases {
		for _, v := range c.accepted {
			if !c.fn(v) {
				t.Errorf("%s: %d rejected, want accepted", c.name, v)
			}
		}
		for _, v := range c.rejected {
			if c.fn(v) {
				t.Errorf("%s: %d accepted, want rejected", c.name, v)
			}
		}
	}
}

func TestPolicyValidCrossRules(t *testing.T) {
	base := DefaultPolicy()
	ok := []Policy{
		base,
		{SuccessThreshold: 1, IntervalSeconds: 1, TimeoutSeconds: 1, DeadlineSeconds: 10},
		{SuccessThreshold: 10, IntervalSeconds: 30, TimeoutSeconds: 30, DeadlineSeconds: 600},
		{SuccessThreshold: 3, IntervalSeconds: 5, TimeoutSeconds: 5, DeadlineSeconds: 15}, // deadline == threshold*interval
		{SuccessThreshold: 3, IntervalSeconds: 2, TimeoutSeconds: 2, DeadlineSeconds: 60, TCPPort: 65535},
	}
	for _, p := range ok {
		if !p.Valid() {
			t.Errorf("Valid(%+v) = false, want true", p)
		}
	}
	bad := map[string]Policy{
		"timeout above interval":       {SuccessThreshold: 3, IntervalSeconds: 2, TimeoutSeconds: 3, DeadlineSeconds: 60},
		"deadline below threshold*int": {SuccessThreshold: 10, IntervalSeconds: 30, TimeoutSeconds: 2, DeadlineSeconds: 299},
		"deadline one below product":   {SuccessThreshold: 3, IntervalSeconds: 5, TimeoutSeconds: 5, DeadlineSeconds: 14},
		"zero value (strict)":          {},
		"tcp port too large":           {SuccessThreshold: 3, IntervalSeconds: 2, TimeoutSeconds: 2, DeadlineSeconds: 60, TCPPort: 65536},
		"tcp port negative":            {SuccessThreshold: 3, IntervalSeconds: 2, TimeoutSeconds: 2, DeadlineSeconds: 60, TCPPort: -1},
	}
	for name, p := range bad {
		if p.Valid() {
			t.Errorf("%s: Valid(%+v) = true, want false", name, p)
		}
	}
}
