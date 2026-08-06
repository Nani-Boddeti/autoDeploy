package auth

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"

	"golang.org/x/crypto/argon2"
)

func TestUsernameAndPasswordPolicies(t *testing.T) {
	name, err := CanonicalUsername("  Admin_User-1  ")
	if err != nil || name != "admin_user-1" {
		t.Fatalf("canonical username = %q, %v", name, err)
	}
	for _, value := range []string{"ab", strings.Repeat("a", 65), "-abc", ".abc", " a\tbc", "ab c", "ab\u00a0c", "\u0430bc", "ab!"} {
		if _, err := CanonicalUsername(value); err == nil {
			t.Errorf("accepted invalid username %q", value)
		}
	}
	for _, value := range []string{strings.Repeat("a", 15), strings.Repeat("\u00e9", 7) + "a", "\u5bc6\u7801 with unicode 123", strings.Repeat("x", 1024)} {
		if err := ValidatePassword(value); err != nil {
			t.Errorf("valid password rejected: %v", err)
		}
	}
	if _, err := CanonicalUsername("abc"); err != nil {
		t.Fatal(err)
	}
	if _, err := CanonicalUsername(strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := CanonicalUsername(strings.Repeat("A", 1024)); err == nil {
		t.Fatal("accepted oversized uppercase username")
	}
	for _, value := range []string{strings.Repeat("\u00e9", 7), strings.Repeat("x", 1025), string([]byte{0xff})} {
		if err := ValidatePassword(value); err == nil {
			t.Errorf("invalid password accepted")
		}
	}
}

func TestPasswordsRoundTripAndParserDefenses(t *testing.T) {
	policy := Argon2Policy{Revision: 2, MemoryKiB: 19 * 1024, Iterations: 2, Lanes: 1}
	password := strings.Repeat("p", 15)
	phc, err := HashPassword(password, policy, bytes.NewReader(bytes.Repeat([]byte{7}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	result, err := VerifyPassword(password, phc, policy)
	if err != nil || !result.Valid || result.RehashNeeded {
		t.Fatalf("verify = %+v, %v", result, err)
	}
	if _, err := HashPassword(password, policy, bytes.NewReader(nil)); err == nil {
		t.Fatal("accepted exhausted salt randomness")
	}
	if result, err = VerifyPassword(password+"x", phc, policy); err != nil || result.Valid {
		t.Fatalf("wrong password = %+v, %v", result, err)
	}
	weaker := Argon2Policy{Revision: 3, MemoryKiB: 20 * 1024, Iterations: 2, Lanes: 1}
	if result, err = VerifyPassword(password, phc, weaker); err != nil || !result.RehashNeeded {
		t.Fatalf("rehash = %+v, %v", result, err)
	}
	for _, malformed := range []string{
		"$argon2id$v=19$m=19456,t=2,p=1$bad$bad$tail", "argon2id$v=19$m=19456,t=2,p=1$bad$bad",
		strings.Replace(phc.PHC, "v=19", "v=18", 1), strings.Replace(phc.PHC, "m=19456,t=2,p=1", "m=19456,t=2,p=1,m=19456", 1),
		strings.Replace(phc.PHC, "m=19456", "m=019456", 1), strings.Replace(phc.PHC, "m=19456", "m=262145", 1),
		strings.Replace(phc.PHC, "t=2", "t=11", 1), strings.Replace(phc.PHC, "p=1", "p=5", 1),
		strings.Replace(phc.PHC, "BwcHB", "_wcHB", 1),
		strings.Replace(phc.PHC, "$", "$=", 1),
	} {
		if _, err := VerifyPassword(password, PasswordVerifier{PHC: malformed, PolicyRevision: policy.Revision}, policy); err == nil {
			t.Errorf("accepted malformed PHC %q", malformed)
		}
	}
	if _, err := VerifyPassword(password, PasswordVerifier{PHC: "$argon2id$v=19$m=262145,t=2,p=1$" + base64.RawStdEncoding.EncodeToString(make([]byte, 16)) + "$" + base64.RawStdEncoding.EncodeToString(make([]byte, 32)), PolicyRevision: policy.Revision}, policy); err == nil {
		t.Error("accepted memory ceiling")
	}
	legacyPHC := formatPHC(8, 1, 1, bytes.Repeat([]byte{1}, 16), argon2.IDKey([]byte(password), bytes.Repeat([]byte{1}, 16), 1, 8, 1, 32))
	if result, err := VerifyPassword(password, PasswordVerifier{PHC: legacyPHC, PolicyRevision: 1}, policy); err != nil || !result.Valid || !result.RehashNeeded {
		t.Fatalf("legacy = %+v, %v", result, err)
	}
	if result, err := VerifyPassword(password, PasswordVerifier{PHC: phc.PHC, PolicyRevision: 1}, policy); err != nil || !result.RehashNeeded {
		t.Fatalf("revision rehash = %+v, %v", result, err)
	}
	if result, err := VerifyPassword(password, PasswordVerifier{PHC: phc.PHC, PolicyRevision: policy.Revision + 1}, policy); err == nil || result.Valid {
		t.Fatalf("future policy revision = %+v, %v", result, err)
	}
	dummy, err := BuildDummyPasswordVerifier(policy)
	if err != nil || dummy.verifier.PolicyRevision != policy.Revision || !strings.Contains(dummy.verifier.PHC, "m=19456,t=2,p=1") {
		t.Fatalf("dummy build: %+v, %v", dummy, err)
	}
	if err := VerifyDummyPassword("autodeploy-non-account-dummy", dummy); err != nil {
		t.Fatalf("dummy public phrase: %v", err)
	}
	strongerPolicy := Argon2Policy{Revision: 4, MemoryKiB: 20 * 1024, Iterations: 3, Lanes: 1}
	strongerDummy, err := BuildDummyPasswordVerifier(strongerPolicy)
	if err != nil || strongerDummy.policy != strongerPolicy {
		t.Fatalf("bound dummy policy: %+v, %v", strongerDummy.policy, err)
	}
	if err := VerifyDummyPassword(password, strongerDummy); err != nil {
		t.Fatalf("bound non-minimum dummy verification: %v", err)
	}
	if _, err := VerifyPassword(password, PasswordVerifier{PHC: strings.Repeat("x", maxPHCBytes+1), PolicyRevision: policy.Revision}, policy); err == nil {
		t.Fatal("accepted oversized PHC")
	}
	if err := ValidatePasswordVerifier(phc); err != nil {
		t.Fatalf("storage validation rejected verifier: %v", err)
	}
	if err := ValidatePasswordVerifier(PasswordVerifier{PHC: phc.PHC}); err == nil {
		t.Fatal("storage validation accepted a zero policy revision")
	}
	if err := ValidatePasswordVerifierForPolicy(phc, policy); err != nil {
		t.Fatalf("exact policy validation rejected verifier: %v", err)
	}
	weakCurrent := PasswordVerifier{PHC: legacyPHC, PolicyRevision: policy.Revision}
	if err := ValidatePasswordVerifier(weakCurrent); err != nil {
		t.Fatalf("legacy loading validation rejected bounded verifier: %v", err)
	}
	if err := ValidatePasswordVerifierForPolicy(weakCurrent, policy); err == nil {
		t.Fatal("new write validation accepted weak verifier labelled current")
	}
}

func TestParsePHCBoundsWithoutHashing(t *testing.T) {
	salt := base64.RawStdEncoding.EncodeToString(make([]byte, argonSaltBytes))
	hash := base64.RawStdEncoding.EncodeToString(make([]byte, argonHashBytes))
	for _, value := range []string{
		"$argon2id$v=19$m=8,t=1,p=1$" + salt + "$" + hash,
		"$argon2id$v=19$m=262144,t=10,p=4$" + salt + "$" + hash,
	} {
		if _, err := parsePHC(value); err != nil {
			t.Fatalf("rejected bounded verifier: %v", err)
		}
	}
	for _, value := range []string{
		"$argon2id$v=19$t=1,m=8,p=1$" + salt + "$" + hash,
		"$argon2id$v=19$m=8,p=1,t=1$" + salt + "$" + hash,
		"$argon2id$v=19$m=08,t=1,p=1$" + salt + "$" + hash,
	} {
		if _, err := parsePHC(value); err == nil {
			t.Fatalf("accepted noncanonical verifier %q", value)
		}
	}
}

func TestSessionsCSRFAndAuthorization(t *testing.T) {
	raw := bytes.Repeat([]byte{9}, 32)
	token, err := ParseSessionToken("v1." + base64.RawURLEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token.String(), "v1.") {
		t.Fatal("missing token version")
	}
	if _, err := ParseSessionToken(token.String() + "="); err == nil {
		t.Fatal("accepted noncanonical session token")
	}
	digest, err := SessionTokenDigest(token)
	if err != nil {
		t.Fatal(err)
	}
	var nonceForDigest LoginNonce
	copy(nonceForDigest[:], raw)
	if digest == LoginNonceDigest(nonceForDigest) {
		t.Fatal("digest domains collided")
	}
	expectedSessionDigest := sha256.Sum256(append([]byte("autodeploy/session-token/v1\x00"), raw...))
	if digest != expectedSessionDigest {
		t.Fatal("unexpected session digest")
	}
	if _, err := SessionTokenDigest(SessionToken("v1.bad")); err == nil {
		t.Fatal("digested malformed typed token")
	}
	if _, err := ParseSessionToken(strings.Repeat("x", 1024)); err == nil {
		t.Fatal("accepted oversized session token")
	}
	if _, err := NewSessionToken(bytes.NewReader(nil)); err == nil {
		t.Fatal("accepted exhausted session randomness")
	}
	if DefaultSessionPolicy().Validate() != nil {
		t.Fatal("default policy invalid")
	}
	if err := (SessionPolicy{}).Validate(); err == nil {
		t.Fatal("accepted invalid policy")
	}
	p := Principal{UserID: "u", OwnerID: "o", Role: RoleOwner, AuthRevision: 1, SessionID: "s"}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []Principal{
		{OwnerID: "o", Role: RoleOwner, AuthRevision: 1, SessionID: "s"},
		{UserID: "u", Role: RoleOwner, AuthRevision: 1, SessionID: "s"},
		{UserID: "u", OwnerID: "o", Role: Role("viewer"), AuthRevision: 1, SessionID: "s"},
		{UserID: "u", OwnerID: "o", Role: RoleOwner, SessionID: "s"},
		{UserID: "u", OwnerID: "o", Role: RoleOwner, AuthRevision: 1},
	} {
		if err := invalid.Validate(); err == nil {
			t.Fatalf("accepted invalid principal %+v", invalid)
		}
	}
	if !CanAccess(p, "o", OperationAdmin) || CanAccess(p, "other", OperationAdmin) || CanAccess(p, "", OperationAdmin) {
		t.Fatal("owner authorization incorrect")
	}
	if CanAccess(p, "o", Operation("unsupported")) {
		t.Fatal("accepted unsupported operation")
	}
	keys := CSRFKeyRing{ActiveVersion: "k1", Keys: map[string][]byte{"k1": bytes.Repeat([]byte{1}, 32), "old": bytes.Repeat([]byte{2}, 32)}}
	origin := "https://admin.example.test"
	sessionCSRF, err := NewSessionCSRF(keys, "s", origin)
	if err != nil || !ValidateSessionCSRF(keys, sessionCSRF, "s", origin) || ValidateSessionCSRF(keys, sessionCSRF, "x", origin) || ValidateSessionCSRF(keys, sessionCSRF, "s", "https://evil.test") {
		t.Fatalf("session csrf failure: %v", err)
	}
	oldKeys := keys
	oldKeys.ActiveVersion = "old"
	oldToken, err := NewSessionCSRF(oldKeys, "s", origin)
	if err != nil || !ValidateSessionCSRF(keys, oldToken, "s", origin) {
		t.Fatalf("retained csrf key: %v", err)
	}
	if _, err := NewSessionCSRF(CSRFKeyRing{ActiveVersion: "bad.key", Keys: map[string][]byte{"bad.key": bytes.Repeat([]byte{1}, 32)}}, "s", origin); err == nil {
		t.Fatal("accepted invalid csrf key version")
	}
	if _, err := NewSessionCSRF(keys, "", origin); err == nil {
		t.Fatal("accepted empty csrf session context")
	}
	nonce, err := NewLoginNonce(bytes.NewReader(bytes.Repeat([]byte{3}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	if parsed, err := ParseLoginNonce(nonce.String()); err != nil || LoginNonceDigest(parsed) != LoginNonceDigest(nonce) {
		t.Fatalf("nonce parse/digest: %v", err)
	}
	partial := bytes.NewReader(bytes.Repeat([]byte{1}, 31))
	if failed, err := NewLoginNonce(partial); err == nil || failed != (LoginNonce{}) {
		t.Fatal("partial nonce failure leaked bytes")
	}
	loginCSRF, err := NewLoginCSRF(keys, nonce, origin)
	if err != nil || !ValidateLoginCSRF(keys, loginCSRF, nonce, origin) || ValidateLoginCSRF(keys, loginCSRF, nonce, "https://evil.test") || ValidateLoginCSRF(keys, sessionCSRF, nonce, origin) || ValidateSessionCSRF(keys, loginCSRF, "s", origin) || ValidateLoginCSRF(CSRFKeyRing{ActiveVersion: "k2", Keys: map[string][]byte{"k2": bytes.Repeat([]byte{1}, 32)}}, loginCSRF, nonce, origin) {
		t.Fatalf("login csrf failure: %v", err)
	}
	if _, err := ParseLoginNonce(nonce.String() + "="); err == nil {
		t.Fatal("accepted noncanonical login nonce")
	}
	if ValidateSessionCSRF(keys, strings.Repeat("x", 1024), "s", origin) || ValidateLoginCSRF(keys, "v1.bad.bad", nonce, origin) {
		t.Fatal("accepted malformed csrf token")
	}
	if _, err := NewSessionCSRF(CSRFKeyRing{ActiveVersion: "k", Keys: map[string][]byte{"k": {1}}}, "s", origin); err == nil {
		t.Fatal("accepted short csrf key")
	}
}

func TestCanonicalHTTPSOrigin(t *testing.T) {
	for input, want := range map[string]string{
		"HTTPS://Admin.Example.test:443/":  "https://admin.example.test",
		"https://admin.example.test:8443":  "https://admin.example.test:8443",
		"https://admin.example.test:0443":  "https://admin.example.test",
		"https://admin.example.test:08443": "https://admin.example.test:8443",
		"https://[2001:db8::1]:443":        "https://[2001:db8::1]",
	} {
		if got, err := CanonicalHTTPSOrigin(input); err != nil || got != want {
			t.Fatalf("origin %q = %q, %v", input, got, err)
		}
	}
	for _, input := range []string{"", "null", "https:opaque", "http://admin.example.test", "https://admin.example.test:", "https://admin.example.test:0", "https://admin.example.test:65536", "https://admin.example.test:bad", "https://admin.example.test/path", "https://admin.example.test?x", "https://m\u00fcnich.example"} {
		if _, err := CanonicalHTTPSOrigin(input); err == nil {
			t.Errorf("accepted invalid origin %q", input)
		}
	}
}
