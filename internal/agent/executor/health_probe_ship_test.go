package executor

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"autodeploy/internal/agent/logship"
	"autodeploy/internal/runtime/docker"
)

// probePath runs one failing-gate attempt through a real Execute path with a
// capturing log shipper and returns the shipped health stream plus the exact
// "health gate: failed" line that path must ship.
type probePath struct {
	name string
	run  func(t *testing.T, gate docker.HealthGateResult) (health, failedLine string)
}

func shippingFactory(t *testing.T, sender logship.Sender) *logship.Factory {
	t.Helper()
	f, err := logship.NewFactory(logship.Config{Sender: sender, Logger: slog.New(slog.DiscardHandler), BatchInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func probePaths() []probePath {
	return []probePath{
		{"executor", func(t *testing.T, gate docker.HealthGateResult) (string, string) {
			sender := &captureSender{}
			e := shippingExecutor(t, &fakeControlPlane{secrets: fullSecrets()}, fakeBuildFunc(okBuildResult(), nil, nil), sender)
			e.docker.(*fakeDocker).gateResult = gate
			lease := testLease()
			if out := e.Execute(context.Background(), lease); out.Succeeded || out.FailureClass == "" {
				t.Fatalf("outcome = %+v, want a failed attempt", out)
			}
			return sender.text("health"), fmt.Sprintf("health gate: failed (reason=%s class=%s)", gate.Reason, docker.ClassifyHealthGateResult(gate))
		}},
		{"stop_then_start", func(t *testing.T, gate docker.HealthGateResult) (string, string) {
			sender := &captureSender{}
			r := newSTSRig(t, true)
			r.e.logs = shippingFactory(t, sender)
			r.dk.gate = gate
			r.e.Execute(context.Background(), r.lease)
			return sender.text("health"), fmt.Sprintf("health gate: failed (reason=%s)", gate.Reason)
		}},
		{"rollback", func(t *testing.T, gate docker.HealthGateResult) (string, string) {
			sender := &captureSender{}
			r := newRollbackRig(t, "blue_green", true, false)
			r.e.logs = shippingFactory(t, sender)
			r.dk.gate = gate
			if out := r.e.Execute(context.Background(), r.lease); out.Succeeded {
				t.Fatalf("outcome = %+v, want a failed attempt", out)
			}
			return sender.text("health"), fmt.Sprintf("health gate: failed (reason=%s class=%s)", gate.Reason, docker.ClassifyHealthGateResult(gate))
		}},
	}
}

func failedGate(probe string) docker.HealthGateResult {
	return docker.HealthGateResult{Reason: docker.HealthGateFailureDeadlineExceeded, LastProbe: probe}
}

// The failed line and the new last-probe line are pinned character for
// character: the control plane parses both from the shipped health stream.
func TestHealthGateFailureShipsLastProbeAfterFailedLine(t *testing.T) {
	for _, p := range probePaths() {
		t.Run(p.name, func(t *testing.T) {
			health, failed := p.run(t, failedGate("status=503; tcp ok"))
			want := failed + "\nhealth gate: last probe: status=503; tcp ok\n"
			if !strings.Contains(health, want) {
				t.Fatalf("health stream = %q, want it to contain %q", health, want)
			}
		})
	}
}

func TestHealthGateFailedLineFormatsArePinned(t *testing.T) {
	gate := failedGate("")
	class := docker.ClassifyHealthGateResult(gate)
	for _, p := range probePaths() {
		t.Run(p.name, func(t *testing.T) {
			_, failed := p.run(t, gate)
			var want string
			if p.name == "stop_then_start" {
				want = "health gate: failed (reason=deadline_exceeded)"
			} else {
				want = "health gate: failed (reason=deadline_exceeded class=" + string(class) + ")"
			}
			if failed != want {
				t.Fatalf("failed line = %q, want %q", failed, want)
			}
		})
	}
}

// A probe that is not an allowlisted shape is withheld on every path,
// including rollback where the shipper's redaction set is empty.
func TestHealthGateLastProbeOutsideAllowlistIsWithheld(t *testing.T) {
	for _, p := range probePaths() {
		t.Run(p.name, func(t *testing.T) {
			health, failed := p.run(t, failedGate("body=runtime-secret-value"))
			if strings.Contains(health, "runtime-secret-value") {
				t.Fatalf("secret leaked into the shipped health stream: %q", health)
			}
			if want := failed + "\nhealth gate: last probe: probe detail withheld\n"; !strings.Contains(health, want) {
				t.Fatalf("health stream = %q, want it to contain %q", health, want)
			}
		})
	}
}

func TestSafeProbe(t *testing.T) {
	unchanged := []string{
		"status=200",
		"status=200; tcp ok",
		`error: Get "http://172.18.0.5:8080/healthz": dial tcp 172.18.0.5:8080: connect: connection refused`,
		"tcp error: dial tcp 172.18.0.5:8080: i/o timeout",
	}
	for _, in := range unchanged {
		if got := safeProbe(in); got != in {
			t.Errorf("safeProbe(%q) = %q, want unchanged", in, got)
		}
	}
	withheld := map[string]string{
		"secret value":     "body=runtime-secret-value",
		"trailing text":    "status=200 secret",
		"script":           "error: something <script>",
		"three segments":   "status=200; tcp ok; tcp ok",
		"400 byte error":   "error: " + strings.Repeat("a", 393) + "connection refused",
		"disallowed char":  "error: dial tcp 10.0.0.1:80: pa$$word connection refused",
		"unknown suffix":   "tcp error: dial tcp 10.0.0.1:80: something odd",
		"bad status range": "status=600",
		"one bad segment":  "status=200; body=secret",
	}
	for name, in := range withheld {
		if got := safeProbe(in); got != "probe detail withheld" {
			t.Errorf("%s: safeProbe(%q) = %q, want withheld", name, in, got)
		}
	}
}

func TestHealthGateEmptyProbeShipsNoLastProbeLine(t *testing.T) {
	for _, probe := range []string{"", "  \t\r\n "} {
		for _, p := range probePaths() {
			t.Run(fmt.Sprintf("%s/%q", p.name, probe), func(t *testing.T) {
				health, failed := p.run(t, failedGate(probe))
				if !strings.Contains(health, failed+"\n") {
					t.Fatalf("health stream = %q, want the failed line %q", health, failed)
				}
				if strings.Contains(health, "last probe") {
					t.Fatalf("health stream = %q, want no last-probe line", health)
				}
			})
		}
	}
}

func TestHealthGateMultilineProbeShipsOneLine(t *testing.T) {
	for _, p := range probePaths() {
		t.Run(p.name, func(t *testing.T) {
			health, _ := p.run(t, failedGate("status=500;\ntcp ok\r\n"))
			if want := "health gate: last probe: status=500; tcp ok\n"; !strings.Contains(health, want) {
				t.Fatalf("health stream = %q, want %q", health, want)
			}
		})
	}
}

func TestHealthGateLongMultibyteProbeIsBoundedAndValidUTF8(t *testing.T) {
	probe := strings.Repeat("é", 150) // 300 bytes, 2 bytes per rune
	for _, p := range probePaths() {
		t.Run(p.name, func(t *testing.T) {
			health, _ := p.run(t, failedGate(probe))
			var got string
			for _, line := range strings.Split(health, "\n") {
				if rest, ok := strings.CutPrefix(line, "health gate: last probe: "); ok {
					got = rest
				}
			}
			// The allowlist withholds non-ASCII detail, so the 200-byte rune
			// cut is defence in depth that this shape never reaches.
			if got != "probe detail withheld" || len(got) > 200 || !utf8.ValidString(got) {
				t.Fatalf("shipped probe = %d bytes valid=%t %q, want the withheld marker", len(got), utf8.ValidString(got), got)
			}
		})
	}
}
