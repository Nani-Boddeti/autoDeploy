package checkoutquota

import (
	"errors"
	"testing"
	"time"
)

func TestValidateSweep(t *testing.T) {
	cases := []struct {
		name          string
		ttl, interval time.Duration
		ok            bool
	}{
		{"defaults", DefaultReclaimTTL, DefaultSweepInterval, true},
		{"minimums", MinReclaimTTL, MinSweepInterval, true},
		{"ttl below floor", MinReclaimTTL - time.Second, DefaultSweepInterval, false},
		{"2h59m rejected", 2*time.Hour + 59*time.Minute, DefaultSweepInterval, false},
		{"3h accepted", 3 * time.Hour, DefaultSweepInterval, true},
		{"interval below floor", DefaultReclaimTTL, MinSweepInterval - time.Second, false},
		{"zero ttl", 0, DefaultSweepInterval, false},
		{"zero interval", DefaultReclaimTTL, 0, false},
		{"negative", -time.Hour, -time.Minute, false},
	}
	for _, c := range cases {
		err := ValidateSweep(c.ttl, c.interval)
		if (err == nil) != c.ok {
			t.Errorf("%s: err=%v want ok=%v", c.name, err, c.ok)
		}
		if err != nil && !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err=%v is not ErrInvalid", c.name, err)
		}
	}
	if MinReclaimTTL != 3*time.Hour {
		t.Fatalf("floor must stay above max build (2h) + checkout: %v", MinReclaimTTL)
	}
	if DefaultReclaimTTL != 6*time.Hour || DefaultSweepInterval != 10*time.Minute {
		t.Fatalf("defaults drifted from owner decision D3: %v %v", DefaultReclaimTTL, DefaultSweepInterval)
	}
}
