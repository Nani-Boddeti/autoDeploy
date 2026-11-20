package docker

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestBuildContainerCreatePayloadGolden pins the complete create payload
// bytes so dropping or renaming any hardening field (SecurityOpt, PidsLimit,
// CapDrop) or any pre-existing field fails loudly.
func TestBuildContainerCreatePayloadGolden(t *testing.T) {
	req := RunContainerRequest{
		Identity:      validReleaseIdentity(),
		NetworkID:     strings.Repeat("b", 64),
		CPUMillis:     500,
		MemoryBytes:   256 << 20,
		RestartPolicy: RestartPolicyUnlessStopped,
	}
	payload := buildContainerCreatePayload(req, map[string]string{"k": "v"})
	got, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"Image":"sha256:` + strings.TrimPrefix(req.Identity.ConfigDigest, "sha256:") +
		`","Labels":{"k":"v"},"HostConfig":{"NetworkMode":"` + req.NetworkID +
		`","RestartPolicy":{"Name":"unless-stopped"},"NanoCpus":500000000,"Memory":268435456,` +
		`"SecurityOpt":["no-new-privileges:true"],"PidsLimit":4096,` +
		`"CapDrop":["NET_RAW","MKNOD","SYS_CHROOT","AUDIT_WRITE","SETFCAP"]}}`
	if string(got) != want {
		t.Fatalf("create payload drifted\n got: %s\nwant: %s", got, want)
	}
}

func TestRunBuildxCapsOutputAndKeepsErrorTruncation(t *testing.T) {
	// 4 MiB of stdout must be truncated to the cap without error.
	writeFakeDocker(t, `head -c 4194304 /dev/zero | tr '\0' 'a'`)
	out, err := runBuildx(t.Context(), BuilderConfig{}, "buildx", "ls")
	if err != nil {
		t.Fatalf("oversize stdout must not error: %v", err)
	}
	if len(out) != maxBuildxOutputBytes {
		t.Fatalf("stdout len = %d, want cap %d", len(out), maxBuildxOutputBytes)
	}

	// Oversize stderr on failure: still an ErrBuilderUnavailable with the
	// message truncated to 512 bytes.
	writeFakeDocker(t, `head -c 4194304 /dev/zero | tr '\0' 'e' >&2; exit 3`)
	_, err = runBuildx(t.Context(), BuilderConfig{}, "buildx", "ls")
	if !errors.Is(err, ErrBuilderUnavailable) {
		t.Fatalf("err = %v, want ErrBuilderUnavailable", err)
	}
	if n := strings.Count(err.Error(), "e"); n > 600 {
		t.Fatalf("error text carries %d stderr bytes, want <= 512-ish", n)
	}
}
