package githubapp

import (
	"context"
	"testing"
)

// Completion is an installation operation and may occur minutes after the
// callback. It must not activate repository authority from an App permission
// snapshot that is no longer valid.
func TestQASetupCompleteRevalidatesAppPermissions(t *testing.T) {
	store := &setupStoreStub{pending: testProvider().installation}
	provider := testProvider()
	provider.app.Permissions = map[string]string{"metadata": "read"}
	service, _ := testService(t, store, provider)

	err := service.Complete(context.Background(), "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE", testPrincipal())
	if err == nil {
		t.Fatal("completion activated authority after required contents permission was removed")
	}
	if provider.appCalls != 1 {
		t.Fatalf("completion did not revalidate App identity and permissions: GetApp calls=%d", provider.appCalls)
	}
	if store.completed != 0 {
		t.Fatal("invalid App permission snapshot reached durable completion")
	}
}
