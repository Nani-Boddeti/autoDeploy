package github

import "testing"

const validSHA = "0123456789abcdef0123456789abcdef01234567"

func TestSourceValidationRejectsGitInvalidRefsAndSHA(t *testing.T) {
	for _, ref := range []string{"refs/heads/main", "refs/heads/release/v1"} {
		if !ValidBranchRef(ref) {
			t.Fatalf("rejected valid ref %q", ref)
		}
	}
	for _, ref := range []string{"main", "refs/heads/", "refs/heads/a//b", "refs/heads/a..b", "refs/heads/.hidden", "refs/heads/a/.hidden", "refs/heads/a.lock", "refs/heads/a.", "refs/heads/a@{b", "refs/heads/a b", "refs/heads/a~b", "refs/heads/a\\b", "refs/heads/a\nb"} {
		if ValidBranchRef(ref) {
			t.Fatalf("accepted invalid ref %q", ref)
		}
	}
	for _, sha := range []string{validSHA, "0000000000000000000000000000000000000001"} {
		if !ValidCommitSHA(sha) {
			t.Fatalf("rejected valid SHA %q", sha)
		}
	}
	for _, sha := range []string{"", "0000000000000000000000000000000000000000", "0123456789ABCDEF0123456789abcdef01234567", "0123456789abcdef0123456789abcdef0123456g"} {
		if ValidCommitSHA(sha) {
			t.Fatalf("accepted invalid SHA %q", sha)
		}
	}
}

func TestOpaqueIDsRemainOpaqueAndBounded(t *testing.T) {
	if !ValidOpaqueID("project/legacy value.with spaces") {
		t.Fatal("opaque legacy project ID rejected")
	}
	for _, value := range []string{"", "x\x00y", "\xff", string(make([]byte, 129))} {
		if ValidOpaqueID(value) {
			t.Fatalf("accepted unsafe opaque ID %q", value)
		}
	}
}

func TestDeliveryStateMatrixAndCredentialFreeModel(t *testing.T) {
	digest := BodyDigest([]byte("body"))
	valid := []Delivery{
		{DeliveryID: "delivery", Event: EventPush, BodyDigest: digest, Verification: VerificationPending, Processing: ProcessingPending},
		{DeliveryID: "delivery", Event: EventPush, BodyDigest: digest, Verification: VerificationRejected, Processing: ProcessingRejected, Failure: FailureSignature},
		{DeliveryID: "delivery", Event: EventPush, BodyDigest: digest, Verification: VerificationVerified, Processing: ProcessingPending},
		{DeliveryID: "delivery", Event: EventPush, BodyDigest: digest, Verification: VerificationVerified, Processing: ProcessingProcessed},
		{DeliveryID: "delivery", Event: EventPush, BodyDigest: digest, Verification: VerificationVerified, Processing: ProcessingRejected, Failure: FailureUnauthorized},
	}
	for _, d := range valid {
		if err := d.Validate(); err != nil {
			t.Fatalf("valid delivery %#v: %v", d, err)
		}
	}
	for _, d := range []Delivery{
		{DeliveryID: "delivery", Event: EventPush, Verification: VerificationPending, Processing: ProcessingPending},
		{DeliveryID: "delivery", Event: EventPush, BodyDigest: digest, Verification: VerificationVerified, Processing: ProcessingRejected},
		{DeliveryID: "delivery", Event: EventPush, BodyDigest: digest, Verification: VerificationRejected, Processing: ProcessingPending, Failure: FailureSignature},
		{DeliveryID: "delivery", Event: EventPush, BodyDigest: digest, Verification: VerificationVerified, Processing: ProcessingProcessed, Failure: FailureMalformed},
	} {
		if d.Validate() == nil {
			t.Fatalf("invalid delivery accepted %#v", d)
		}
	}
	if err := (Headers{DeliveryID: "delivery", Event: EventPush, Signature: "sha256=secret"}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestPushAndLifecycleEventMatrices(t *testing.T) {
	if err := (Push{InstallationID: 1, RepositoryID: 2, OwnerID: 3, Ref: "refs/heads/main", CommitSHA: validSHA}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (Lifecycle{Event: EventInstallation, InstallationID: 1, OwnerID: 2, Action: "suspend"}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (Lifecycle{Event: EventInstallationRepositories, InstallationID: 1, RepositoryID: 2, OwnerID: 3, Action: "removed"}).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, event := range []Lifecycle{{Event: EventInstallation, InstallationID: 1, RepositoryID: 2, OwnerID: 3, Action: "suspend"}, {Event: EventInstallationRepositories, InstallationID: 1, OwnerID: 3, Action: "added"}, {Event: EventPush, InstallationID: 1, OwnerID: 3, Action: "created"}} {
		if event.Validate() == nil {
			t.Fatalf("invalid lifecycle accepted %#v", event)
		}
	}
}
