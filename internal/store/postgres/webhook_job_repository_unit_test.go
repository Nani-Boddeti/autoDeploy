package postgres

import (
	"testing"

	"autodeploy/internal/github"
)

func TestWebhookIDsDeterministicAndDomainSeparated(t *testing.T) {
	target := webhookTarget{ownerID: "owner", projectID: "project", environmentID: "environment", serverID: "server"}
	push := github.Push{RepositoryID: 201, Ref: "refs/heads/main", CommitSHA: "0123456789abcdef0123456789abcdef01234567"}
	deploymentID, jobKey := webhookIDs(target, push)
	againDeploymentID, againJobKey := webhookIDs(target, push)
	if deploymentID != againDeploymentID || jobKey != againJobKey {
		t.Fatalf("IDs are not deterministic: %q/%q != %q/%q", deploymentID, jobKey, againDeploymentID, againJobKey)
	}
	if len(deploymentID) != 44 || len(jobKey) != 28 || string(deploymentID)[:4] != "whd_" || jobKey[:4] != "whj_" {
		t.Fatalf("unsafe ID bounds or prefixes: %q %q", deploymentID, jobKey)
	}
	// These two vectors collide under delimiter-free concatenation but not under
	// the length-prefixed encoding used by webhookIDs.
	leftDeploymentID, leftJobKey := webhookIDs(webhookTarget{ownerID: "ab", projectID: "c", environmentID: "d", serverID: "e"}, push)
	rightDeploymentID, rightJobKey := webhookIDs(webhookTarget{ownerID: "a", projectID: "bc", environmentID: "d", serverID: "e"}, push)
	if leftDeploymentID == rightDeploymentID || leftJobKey == rightJobKey {
		t.Fatal("length-prefixed identity collision")
	}
}
