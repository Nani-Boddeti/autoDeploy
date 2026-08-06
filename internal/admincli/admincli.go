// Package admincli contains the testable orchestration behind the terminal-only
// administrator recovery command.
package admincli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"sort"
	"time"

	"autodeploy/internal/auth"
	"autodeploy/internal/store/postgres"
)

var ErrOperationFailed = errors.New("administrator operation failed")

type Terminal interface{ ReadPassword(string) ([]byte, error) }

type Repository interface {
	Bootstrap(context.Context, postgres.BootstrapInput) error
	LoadPasswordPolicy(context.Context) (auth.Argon2Policy, error)
	FindLoginUserByCanonicalUsername(context.Context, string) (postgres.LoginUser, error)
	ResetPassword(context.Context, string, auth.PasswordVerifier, string, [][32]byte) error
}

type Benchmark func(auth.Argon2Policy) (time.Duration, error)

type Dependencies struct {
	Repository Repository
	Terminal   Terminal
	Random     io.Reader
	Benchmark  Benchmark
}

func Bootstrap(ctx context.Context, username string, dependencies Dependencies) error {
	canonical, err := auth.CanonicalUsername(username)
	if err != nil || !validDependencies(dependencies) {
		return ErrOperationFailed
	}
	policy, err := Calibrate(dependencies.Benchmark)
	if err != nil {
		return ErrOperationFailed
	}
	password, err := promptPassword(dependencies.Terminal)
	if err != nil {
		return ErrOperationFailed
	}
	defer clear(password)
	ownerID, err := newID(dependencies.Random)
	if err != nil {
		return ErrOperationFailed
	}
	userID, err := newID(dependencies.Random)
	if err != nil || userID == ownerID {
		return ErrOperationFailed
	}
	auditID, err := newID(dependencies.Random)
	if err != nil || auditID == ownerID || auditID == userID {
		return ErrOperationFailed
	}
	verifier, err := auth.HashPassword(string(password), policy, dependencies.Random)
	if err != nil {
		return ErrOperationFailed
	}
	if err := dependencies.Repository.Bootstrap(ctx, postgres.BootstrapInput{OwnerID: ownerID, UserID: userID, Username: canonical, AuditID: auditID, Verifier: verifier, Policy: policy}); err != nil {
		return ErrOperationFailed
	}
	return nil
}

func ResetPassword(ctx context.Context, username string, ring auth.UsernameThrottleKeyRing, dependencies Dependencies) error {
	canonical, err := auth.CanonicalUsername(username)
	if err != nil || !validDependencies(dependencies) {
		return ErrOperationFailed
	}
	user, err := dependencies.Repository.FindLoginUserByCanonicalUsername(ctx, canonical)
	if err != nil || user.ID == "" {
		return ErrOperationFailed
	}
	policy, err := dependencies.Repository.LoadPasswordPolicy(ctx)
	if err != nil {
		return ErrOperationFailed
	}
	digests, err := ring.UsernameDigests(canonical)
	if err != nil {
		return ErrOperationFailed
	}
	password, err := promptPassword(dependencies.Terminal)
	if err != nil {
		return ErrOperationFailed
	}
	defer clear(password)
	verifier, err := auth.HashPassword(string(password), policy, dependencies.Random)
	if err != nil {
		return ErrOperationFailed
	}
	auditID, err := newID(dependencies.Random)
	if err != nil {
		return ErrOperationFailed
	}
	if err := dependencies.Repository.ResetPassword(ctx, user.ID, verifier, auditID, digests); err != nil {
		return ErrOperationFailed
	}
	return nil
}

func validDependencies(d Dependencies) bool {
	return d.Repository != nil && d.Terminal != nil && d.Benchmark != nil
}

func promptPassword(terminal Terminal) ([]byte, error) {
	first, err := terminal.ReadPassword("Password: ")
	if err != nil {
		return nil, err
	}
	second, err := terminal.ReadPassword("Confirm password: ")
	if err != nil {
		clear(first)
		return nil, err
	}
	defer clear(second)
	if !bytes.Equal(first, second) || auth.ValidatePasswordBytes(first) != nil {
		clear(first)
		return nil, errors.New("invalid password")
	}
	return first, nil
}

func clear(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

func newID(randomness io.Reader) (string, error) {
	if randomness == nil {
		randomness = rand.Reader
	}
	value := make([]byte, 32)
	if _, err := io.ReadFull(randomness, value); err != nil {
		return "", err
	}
	return "a1_" + base64.RawURLEncoding.EncodeToString(value), nil
}

// Calibrate evaluates the bounded ADR policy space once, then takes a small
// median sample of the closest candidate to dampen scheduling noise without
// repeatedly benchmarking all sixteen expensive combinations. Stable ties keep
// the lower-cost candidate.
func Calibrate(benchmark Benchmark) (auth.Argon2Policy, error) {
	if benchmark == nil {
		return auth.Argon2Policy{}, errors.New("nil benchmark")
	}
	type measuredPolicy struct {
		policy  auth.Argon2Policy
		samples []time.Duration
	}
	measured := make([]measuredPolicy, 0, 16)
	for _, memory := range []uint32{19 * 1024, 32 * 1024, 48 * 1024, 64 * 1024} {
		for iterations := uint32(2); iterations <= 5; iterations++ {
			candidate := auth.Argon2Policy{Revision: 1, MemoryKiB: memory, Iterations: iterations, Lanes: 1}
			elapsed, err := benchmark(candidate)
			if err != nil || elapsed < 0 {
				return auth.Argon2Policy{}, errors.New("benchmark failed")
			}
			candidateDistance := elapsed - 250*time.Millisecond
			if candidateDistance < 0 {
				candidateDistance = -candidateDistance
			}
			measured = append(measured, measuredPolicy{policy: candidate, samples: []time.Duration{candidateDistance}})
		}
	}
	sort.SliceStable(measured, func(i, j int) bool { return measured[i].samples[0] < measured[j].samples[0] })
	for index := 0; index < 3; index++ {
		for range 2 {
			duration, err := benchmark(measured[index].policy)
			if err != nil || duration < 0 {
				return auth.Argon2Policy{}, errors.New("benchmark failed")
			}
			distance := duration - 250*time.Millisecond
			if distance < 0 {
				distance = -distance
			}
			measured[index].samples = append(measured[index].samples, distance)
		}
		sort.Slice(measured[index].samples, func(i, j int) bool { return measured[index].samples[i] < measured[index].samples[j] })
	}
	chosen := measured[0]
	for _, candidate := range measured[1:3] {
		if candidate.samples[1] < chosen.samples[1] {
			chosen = candidate
		}
	}
	if chosen.policy.Validate() != nil {
		return auth.Argon2Policy{}, errors.New("benchmark failed")
	}
	return chosen.policy, nil
}
