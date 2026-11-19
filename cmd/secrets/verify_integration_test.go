//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"

	secretcrypto "autodeploy/internal/crypto"
	"autodeploy/internal/secret"
	"autodeploy/internal/store/postgres"
	"autodeploy/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	plainOne = "PLAINTEXT-ONE-do-not-print-7f3a"
	plainTwo = "PLAINTEXT-TWO-do-not-print-9c1b"
)

type verifyEnv struct {
	pool       *pgxpool.Pool
	generation string
	ring       secretcrypto.KeyRing
	service    *secret.Service
}

func verifyRing(t *testing.T, active string, ids ...string) secretcrypto.KeyRing {
	t.Helper()
	keys := map[string][]byte{}
	for i, id := range ids {
		keys[id] = bytes.Repeat([]byte{byte(i + 1)}, 32)
	}
	ring, err := secretcrypto.NewKeyRing(active, keys)
	if err != nil {
		t.Fatal(err)
	}
	return ring
}

func newVerifyEnv(t *testing.T) verifyEnv {
	t.Helper()
	dsn := os.Getenv("AUTODEPLOY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AUTODEPLOY_TEST_DATABASE_URL is required for PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if err = migrations.Apply(ctx, conn); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err = pool.Exec(ctx, `TRUNCATE secret_audit_events, secret_versions, secret_heads, secret_wrap_invocations RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	var generation string
	if err = pool.QueryRow(ctx, `SELECT id FROM secret_database_generation WHERE singleton=true`).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if err = postgres.NewKeyRepository(pool).InitializeActiveLedger(ctx, "kek-a", generation); err != nil {
		t.Fatal(err)
	}
	ring := verifyRing(t, "kek-a", "kek-a", "kek-b")
	return verifyEnv{pool: pool, generation: generation, ring: ring, service: secret.NewService(postgres.NewSecretRepository(pool), ring, nil)}
}

// worstCaseTerminalActor is the longest actor terminalActor can produce (32-byte
// operator and 32-byte process user); the audit row must accept it.
const worstCaseTerminalActor = "terminal:" + "oooooooooooooooooooooooooooooooo" + "(as " + "pppppppppppppppppppppppppppppppp" + ")"

func (e verifyEnv) put(t *testing.T, id, versionID, value string) secret.Scope {
	t.Helper()
	s := secret.Scope{OwnerID: "owner", ProjectID: "project", EnvironmentID: "environment", SecretID: id, Purpose: "runtime", DeliveryScope: "runtime"}
	if _, err := e.service.CreateWithContext(context.Background(), s, versionID, secret.OperationContext{AuditID: "audit-" + versionID, ActorID: worstCaseTerminalActor}, []byte(value)); err != nil {
		t.Fatal(err)
	}
	return s
}

// verify runs the real command path (production wiring over a real pool) and
// returns everything an operator could see.
func (e verifyEnv) verify(t *testing.T, ring secretcrypto.KeyRing, generation string) (stdout string, err error) {
	t.Helper()
	return e.run(t, command{action: "verify"}, ring, generation)
}

// run executes a command through the real production wiring over a fresh pool.
func (e verifyEnv) run(t *testing.T, c command, ring secretcrypto.KeyRing, generation string) (stdout string, err error) {
	t.Helper()
	deps := secretDependencies{
		loadDatabase: func() (string, error) { return "unused", nil },
		loadRing:     func() (secretcrypto.KeyRing, string, error) { return ring, generation, nil },
		// executeWith closes the operator's pool, so each run gets its own.
		open: func(ctx context.Context, _ string) (secretOperator, error) {
			pool, err := pgxpool.New(ctx, os.Getenv("AUTODEPLOY_TEST_DATABASE_URL"))
			if err != nil {
				return nil, err
			}
			return postgresSecretOperator{pool: pool}, nil
		},
		actor: func() string { return "terminal:tester" },
		newID: func(p string) (string, error) { return p + "-x", nil },
	}
	var out bytes.Buffer
	err = runWithOutput(c, func() (terminalSession, error) {
		if c.action == "verify" {
			t.Fatal("verify must not need a terminal")
		}
		return fakeTerminal{}, nil
	},
		func(ctx context.Context, c command) (string, error) { return executeWith(ctx, c, deps) }, &out)
	all := out.String()
	if err != nil {
		all += "\n" + errorMessage(err) + "\n" + err.Error()
	}
	for _, p := range []string{plainOne, plainTwo} {
		for _, form := range []string{p, base64.StdEncoding.EncodeToString([]byte(p)), base64.RawStdEncoding.EncodeToString([]byte(p)), base64.URLEncoding.EncodeToString([]byte(p)), base64.RawURLEncoding.EncodeToString([]byte(p)), hex.EncodeToString([]byte(p))} {
			if strings.Contains(all, form) {
				t.Fatalf("output leaked a plaintext form")
			}
		}
	}
	return out.String(), err
}

func (e verifyEnv) counts(t *testing.T) string {
	t.Helper()
	var s string
	err := e.pool.QueryRow(context.Background(), `SELECT (SELECT coalesce(string_agg(kek_id||':'||reserved_invocations||':'||updated_at::text, ',' ORDER BY kek_id),'') FROM secret_wrap_invocations)||'|'||(SELECT count(*) FROM secret_audit_events)||'|'||(SELECT count(*) FROM secret_versions)||'|'||(SELECT count(*) FROM secret_heads)||'|'||(SELECT coalesce(string_agg(md5(v::text),','),'') FROM secret_versions v)`).Scan(&s)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (e verifyEnv) corrupt(t *testing.T, versionID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `ALTER TABLE secret_versions DISABLE TRIGGER secret_versions_reject_update`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = e.pool.Exec(ctx, `ALTER TABLE secret_versions ENABLE TRIGGER secret_versions_reject_update`)
	})
	if _, err := e.pool.Exec(ctx, `UPDATE secret_versions SET ciphertext = set_byte(ciphertext, 0, get_byte(ciphertext,0) # 1) WHERE id=$1`, versionID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `ALTER TABLE secret_versions ENABLE TRIGGER secret_versions_reject_update`); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyCanaryEndToEnd(t *testing.T) {
	e := newVerifyEnv(t)
	e.put(t, "secret-one", "version-one", plainOne)
	two := e.put(t, "secret-two", "version-two", plainTwo)

	before := e.counts(t)
	out, err := e.verify(t, e.ring, e.generation)
	if err != nil || out != "secret verify completed checked=2 failed=0\n" {
		t.Fatalf("healthy: %q %v", out, err)
	}
	if after := e.counts(t); after != before {
		t.Fatalf("verify changed rows\nbefore=%s\nafter=%s", before, after)
	}

	// A non-head (superseded) version stays decryptable for frozen retries and is checked.
	if _, err = e.service.ReplaceWithContext(context.Background(), two, "version-two-b", secret.OperationContext{AuditID: "audit-two-b", ActorID: "terminal:tester"}, []byte(plainTwo)); err != nil {
		t.Fatal(err)
	}
	if out, err = e.verify(t, e.ring, e.generation); err != nil || out != "secret verify completed checked=3 failed=0\n" {
		t.Fatalf("superseded included: %q %v", out, err)
	}
	// A revoked (compromised) version is refused by every read path, so it is not checked.
	if _, err = e.service.ReplaceCompromisedWithContext(context.Background(), two, "version-two-c", secret.OperationContext{AuditID: "audit-two-c", ActorID: "terminal:tester"}, []byte(plainTwo)); err != nil {
		t.Fatal(err)
	}
	if out, err = e.verify(t, e.ring, e.generation); err != nil || out != "secret verify completed checked=3 failed=0\n" {
		t.Fatalf("revoked excluded: %q %v", out, err)
	}

	// Missing KEK.
	noKEK := verifyRing(t, "kek-b", "kek-b")
	before = e.counts(t)
	out, err = e.verify(t, noKEK, e.generation)
	var vf verifyFailedError
	if !errors.As(err, &vf) || !strings.HasPrefix(out, "secret verify completed checked=3 failed=3\n") || strings.Count(out, "reason=kek_missing") != 3 || !strings.Contains(out, "failed version=version-one kek=kek-a reason=kek_missing") {
		t.Fatalf("kek missing: %q %v", out, err)
	}
	if e.counts(t) != before {
		t.Fatal("failing verify changed rows")
	}

	// Corrupt exactly one version.
	e.corrupt(t, "version-one")
	out, err = e.verify(t, e.ring, e.generation)
	if !errors.As(err, &vf) || out != "secret verify completed checked=3 failed=1\nfailed version=version-one kek=kek-a reason=decrypt_failed\n" {
		t.Fatalf("corrupt: %q %v", out, err)
	}

	// Generation mismatch is a setup failure with no report lines.
	out, err = e.verify(t, e.ring, "ffffffffffffffffffff")
	if !errors.Is(err, secret.ErrGenerationMismatch) || out != "" || errors.As(err, &vf) {
		t.Fatalf("generation: %q %v", out, err)
	}
}

func TestRewrapAndCanRetireRefuseGenerationMismatch(t *testing.T) {
	e := newVerifyEnv(t)
	e.put(t, "secret-one", "version-one", plainOne)
	// kek-b is active so kek-a is the old key with one version still on it.
	rotRing := verifyRing(t, "kek-b", "kek-a", "kek-b")
	if err := postgres.NewKeyRepository(e.pool).InitializeActiveLedger(context.Background(), "kek-b", e.generation); err != nil {
		t.Fatal(err)
	}
	const wrong = "ffffffffffffffffffff"
	before := e.counts(t)

	for _, c := range []command{
		{action: "rewrap", oldKEK: "kek-a", batch: secret.DefaultRewrapBatchSize},
		{action: "can-retire", oldKEK: "kek-a"},
	} {
		out, err := e.run(t, c, rotRing, wrong)
		if !errors.Is(err, secret.ErrGenerationMismatch) || out != "" || errorMessage(err) != "secret operation failed: database generation does not match the key manifest" {
			t.Fatalf("%+v: out=%q err=%v msg=%q", c, out, err, errorMessage(err))
		}
		if e.counts(t) != before {
			t.Fatalf("%+v changed rows or the wrap ledger", c)
		}
	}

	// Matching generation keeps the existing behaviour.
	if out, err := e.run(t, command{action: "can-retire", oldKEK: "kek-a"}, rotRing, e.generation); !errors.Is(err, secret.ErrRetirement) || out != "" {
		t.Fatalf("can-retire matched: %q %v", out, err)
	}
	out, err := e.run(t, command{action: "rewrap", oldKEK: "kek-a", batch: secret.DefaultRewrapBatchSize}, rotRing, e.generation)
	if err != nil || !strings.HasPrefix(out, "secret rewrap completed selected=1 rewrapped=1 conflicts=0") {
		t.Fatalf("rewrap matched: %q %v", out, err)
	}
	if e.counts(t) == before {
		t.Fatal("matched rewrap changed nothing")
	}
}
