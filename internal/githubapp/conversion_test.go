package githubapp

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	sentinelClientSecret  = "SENTINEL-CLIENT-SECRET-0123"
	sentinelWebhookSecret = "SENTINEL-WEBHOOK-SECRET-0123"
)

func testKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

func validPayload(t *testing.T, pemText string) map[string]any {
	return map[string]any{
		"id": 42, "slug": "autodeploy-cp", "client_id": "Iv1.abc", "name": "AutoDeploy cp",
		"html_url": "https://github.com/apps/autodeploy-cp",
		"owner":    map[string]any{"id": 7, "login": "acme", "type": "Organization", "extra": "ok"},
		"pem":      pemText, "webhook_secret": sentinelWebhookSecret, "client_secret": sentinelClientSecret,
		"node_id": "X", "unknown_field": []int{1},
	}
}

func serve(t *testing.T, status int, body []byte) (*ManifestClient, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return newManifestClient(srv.URL), &hits
}

func mustJSON(t *testing.T, v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestConvertManifestCodeHappyAndRequestShape(t *testing.T) {
	pemText := testKeyPEM(t)
	gotCh := make(chan *http.Request, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCh <- r.Clone(context.Background())
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(mustJSON(t, validPayload(t, pemText)))
	}))
	defer srv.Close()
	res, err := newManifestClient(srv.URL).ConvertManifestCode(context.Background(), "abc_DEF-123")
	if err != nil {
		t.Fatal(err)
	}
	got := <-gotCh
	if got.Method != http.MethodPost || got.URL.Path != "/app-manifests/abc_DEF-123/conversions" {
		t.Fatalf("request %s %s", got.Method, got.URL.Path)
	}
	if got.Header.Get("Accept") != "application/vnd.github+json" || got.Header.Get("X-GitHub-Api-Version") != APIVersion || got.Header.Get("User-Agent") != "autodeploy-controlplane" {
		t.Fatalf("headers %v", got.Header)
	}
	if _, ok := got.Header["Authorization"]; ok || got.ContentLength > 0 {
		t.Fatal("unexpected auth header or body")
	}
	want := ManifestConversion{AppID: 42, Slug: "autodeploy-cp", ClientID: "Iv1.abc", Name: "AutoDeploy cp", HTMLURL: "https://github.com/apps/autodeploy-cp", OwnerID: 7, OwnerLogin: "acme", OwnerType: "Organization"}
	if res.AppID != want.AppID || res.Slug != want.Slug || res.ClientID != want.ClientID || res.Name != want.Name || res.HTMLURL != want.HTMLURL || res.OwnerID != 7 || res.OwnerLogin != "acme" || res.OwnerType != "Organization" {
		t.Fatalf("result %+v", res)
	}
	if string(res.PEM) != pemText || string(res.WebhookSecret) != sentinelWebhookSecret {
		t.Fatal("secret fields not returned")
	}
}

func TestConvertManifestCodePKCS8(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	p := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	c, _ := serve(t, 201, mustJSON(t, validPayload(t, p)))
	if _, err := c.ConvertManifestCode(context.Background(), "c"); err != nil {
		t.Fatal(err)
	}
}

func TestConvertManifestCodeStatuses(t *testing.T) {
	cases := []struct {
		status int
		want   error
	}{
		{404, ErrManifestCodeRejected}, {422, ErrManifestCodeRejected},
		{403, ErrProvider}, {429, ErrRateLimited},
		{500, ErrProvider}, {502, ErrProvider}, {401, ErrProvider},
		{200, ErrInvalidResponse}, {204, ErrInvalidResponse},
	}
	for _, tc := range cases {
		c, _ := serve(t, tc.status, []byte(`{"message":"x"}`))
		_, err := c.ConvertManifestCode(context.Background(), "code")
		if !errors.Is(err, tc.want) {
			t.Errorf("%d: %v", tc.status, err)
		}
		var orphan *ManifestOrphanError
		if errors.As(err, &orphan) {
			t.Errorf("%d: unexpected orphan error", tc.status)
		}
	}
}

func TestConvertManifestCodeBadBodies(t *testing.T) {
	pemText := testKeyPEM(t)
	c, _ := serve(t, 201, []byte("<html>not json"))
	_, err := c.ConvertManifestCode(context.Background(), "code")
	var orphan *ManifestOrphanError
	if !errors.Is(err, ErrInvalidResponse) || !errors.As(err, &orphan) || orphan.AppID != 0 || orphan.HTMLURL != "" {
		t.Fatalf("non-json: %v %+v", err, orphan)
	}
	bigPayload := validPayload(t, pemText)
	bigPayload["pad"] = strings.Repeat("a", maxBodyBytes)
	c, _ = serve(t, 201, mustJSON(t, bigPayload))
	_, err = c.ConvertManifestCode(context.Background(), "code")
	if !errors.Is(err, ErrInvalidResponse) || !errors.As(err, &orphan) {
		t.Fatalf("oversize: %v", err)
	}
	for _, field := range []string{"id", "slug", "client_id", "name", "html_url", "owner", "pem", "webhook_secret"} {
		p := validPayload(t, pemText)
		delete(p, field)
		c, _ = serve(t, 201, mustJSON(t, p))
		if _, err = c.ConvertManifestCode(context.Background(), "code"); !errors.Is(err, ErrInvalidResponse) {
			t.Errorf("missing %s: %v", field, err)
		}
	}
	mutations := map[string]func(map[string]any){
		"null webhook":     func(p map[string]any) { p["webhook_secret"] = nil },
		"empty webhook":    func(p map[string]any) { p["webhook_secret"] = "" },
		"long webhook":     func(p map[string]any) { p["webhook_secret"] = strings.Repeat("a", 256) },
		"zero id":          func(p map[string]any) { p["id"] = 0 },
		"bad owner type":   func(p map[string]any) { p["owner"] = map[string]any{"id": 1, "login": "a", "type": "Bot"} },
		"owner id 0":       func(p map[string]any) { p["owner"] = map[string]any{"id": 0, "login": "a", "type": "User"} },
		"owner no login":   func(p map[string]any) { p["owner"] = map[string]any{"id": 1, "login": "", "type": "User"} },
		"http html_url":    func(p map[string]any) { p["html_url"] = "http://github.com/apps/x" },
		"foreign url":      func(p map[string]any) { p["html_url"] = "https://github.com.evil.com/apps/x" },
		"long html_url":    func(p map[string]any) { p["html_url"] = "https://github.com/" + strings.Repeat("a", 512) },
		"slug slash":       func(p map[string]any) { p["slug"] = "a/b" },
		"slug question":    func(p map[string]any) { p["slug"] = "a?b" },
		"slug hash":        func(p map[string]any) { p["slug"] = "a#b" },
		"slug leading dot": func(p map[string]any) { p["slug"] = ".a" },
		"long slug":        func(p map[string]any) { p["slug"] = strings.Repeat("a", 101) },
		"long client_id":   func(p map[string]any) { p["client_id"] = strings.Repeat("a", 256) },
		"string id":        func(p map[string]any) { p["id"] = "42" },
		"empty pem":        func(p map[string]any) { p["pem"] = "" },
		"garbage pem":      func(p map[string]any) { p["pem"] = "not a pem " + sentinelClientSecret },
	}
	for name, mutate := range mutations {
		p := validPayload(t, pemText)
		mutate(p)
		c, _ = serve(t, 201, mustJSON(t, p))
		_, err = c.ConvertManifestCode(context.Background(), "code")
		if !errors.Is(err, ErrInvalidResponse) {
			t.Errorf("%s: %v", name, err)
			continue
		}
		assertNoSecrets(t, name, err, pemText)
	}
}

func TestConvertManifestCodePEMVariants(t *testing.T) {
	good := testKeyPEM(t)
	ec, err := ecKeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	small, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	smallPEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(small)}))
	cases := map[string]string{
		"ec key":       ec,
		"two blocks":   good + good,
		"trailing":     good + "junk",
		"leading junk": "junk\n" + good,
		"1024 bits":    smallPEM,
		"wrong type":   strings.ReplaceAll(good, "RSA PRIVATE KEY", "CERTIFICATE"),
	}
	for name, p := range cases {
		c, _ := serve(t, 201, mustJSON(t, validPayload(t, p)))
		_, err := c.ConvertManifestCode(context.Background(), "code")
		var orphan *ManifestOrphanError
		if !errors.Is(err, ErrInvalidResponse) || !errors.As(err, &orphan) {
			t.Errorf("%s: %v", name, err)
			continue
		}
		assertNoSecrets(t, name, err, p)
	}
}

func TestOrphanErrorCarriesIDAndURL(t *testing.T) {
	p := validPayload(t, "garbage")
	c, _ := serve(t, 201, mustJSON(t, p))
	_, err := c.ConvertManifestCode(context.Background(), "code")
	var orphan *ManifestOrphanError
	if !errors.As(err, &orphan) || orphan.AppID != 42 || orphan.HTMLURL != "https://github.com/apps/autodeploy-cp" {
		t.Fatalf("orphan %+v err %v", orphan, err)
	}
	// An untrusted html_url is dropped but the id is kept.
	p["html_url"] = "https://evil.example/x"
	c, _ = serve(t, 201, mustJSON(t, p))
	_, err = c.ConvertManifestCode(context.Background(), "code")
	if !errors.As(err, &orphan) || orphan.AppID != 42 || orphan.HTMLURL != "" {
		t.Fatalf("orphan %+v", orphan)
	}
}

func TestConvertManifestCodeRejectsBadCodeWithoutRequest(t *testing.T) {
	c, hits := serve(t, 201, nil)
	for _, code := range []string{"", "a/b", "a b", "a?b", "a%2fb", "../x", "é", strings.Repeat("a", 129), "a\nb"} {
		if _, err := c.ConvertManifestCode(context.Background(), code); !errors.Is(err, ErrInvalidIdentity) {
			t.Errorf("code %q: %v", code, err)
		}
	}
	if _, err := c.ConvertManifestCode(context.Background(), strings.Repeat("a", 128)); errors.Is(err, ErrInvalidIdentity) {
		t.Error("128-char code should be valid")
	}
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Fatalf("requests %d, want 1 (only the 128-char code)", n)
	}
}

func TestConvertManifestCodeDoesNotFollowRedirect(t *testing.T) {
	var targetHits int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { atomic.AddInt32(&targetHits, 1) }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/x", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	_, err := newManifestClient(srv.URL).ConvertManifestCode(context.Background(), "code")
	if !errors.Is(err, ErrProvider) {
		t.Fatalf("err %v", err)
	}
	if atomic.LoadInt32(&targetHits) != 0 {
		t.Fatal("redirect target was contacted")
	}
}

func TestConvertManifestCodeContextAndTransportErrors(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := newManifestClient(srv.URL).ConvertManifestCode(ctx, "secretcode123")
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrProvider) || time.Since(start) > 5*time.Second {
		t.Fatalf("err %v", err)
	}
	if strings.Contains(err.Error(), "secretcode123") {
		t.Fatal("code leaked in error")
	}
	// Dead server: transport error must not leak the URL/code.
	dead := httptest.NewServer(http.NotFoundHandler())
	base := dead.URL
	dead.Close()
	_, err = newManifestClient(base).ConvertManifestCode(context.Background(), "secretcode123")
	if !errors.Is(err, ErrProvider) || strings.Contains(err.Error(), "secretcode123") {
		t.Fatalf("err %v", err)
	}
}

func assertNoSecrets(t *testing.T, name string, err error, pemText string) {
	t.Helper()
	for _, msg := range []string{err.Error(), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err)} {
		for _, s := range []string{sentinelClientSecret, sentinelWebhookSecret, "PRIVATE KEY", "BEGIN"} {
			if strings.Contains(msg, s) {
				t.Errorf("%s: error leaks %q", name, s)
			}
		}
		if pemText != "" && strings.Contains(msg, pemText) {
			t.Errorf("%s: error leaks pem", name)
		}
	}
}

func ecKeyPEM() (string, error) {
	key, err := ecdsaGenerate()
	if err != nil {
		return "", err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), nil
}

func ecdsaGenerate() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

func TestPEMIsCanonicalized(t *testing.T) {
	good := testKeyPEM(t)
	messy := "\n" + strings.Replace(good, "\n", "\r\n", -1) + "\n\n"
	c, _ := serve(t, 201, mustJSON(t, validPayload(t, messy)))
	res, err := c.ConvertManifestCode(context.Background(), "code")
	if err != nil {
		t.Fatal(err)
	}
	if string(res.PEM) != good {
		t.Fatal("PEM not canonical")
	}
}

func TestInjectionInNameStaysLiteral(t *testing.T) {
	for _, name := range []string{`a","public":true,"x":"`, `</script><b>&`} {
		form, err := BuildManifest(ManifestInput{Origin: "https://cp.example.com", AccountType: "personal", Name: name}, testState)
		if err != nil {
			t.Fatal(err)
		}
		var back struct {
			Name   string `json:"name"`
			Public bool   `json:"public"`
		}
		if err = json.Unmarshal(form.JSON, &back); err != nil || back.Name != name || back.Public {
			t.Fatalf("name %q -> %+v err %v", name, back, err)
		}
		if strings.Contains(string(form.JSON), "</script>") {
			t.Fatal("raw </script> in manifest JSON")
		}
	}
}

func TestShortBodyAfter201IsOrphan(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		writeTruncated(rw, conn)
	}))
	defer srv.Close()
	_, err := newManifestClient(srv.URL).ConvertManifestCode(context.Background(), "code")
	var orphan *ManifestOrphanError
	if !errors.Is(err, ErrInvalidResponse) || !errors.As(err, &orphan) {
		t.Fatalf("err %v", err)
	}
}

func writeTruncated(rw *bufio.ReadWriter, conn net.Conn) {
	_, _ = rw.WriteString("HTTP/1.1 201 Created\r\nContent-Length: 100000\r\nContent-Type: application/json\r\n\r\n{\"id\":42,")
	_ = rw.Flush()
}

func TestTimeouts(t *testing.T) {
	release := make(chan struct{})
	stall := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	stalledBody := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":42,"html_url":"https://github.com/apps/x"`))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer stall.Close()
	defer stalledBody.Close()
	defer close(release)
	_, err := newManifestClient(stall.URL, 100*time.Millisecond).ConvertManifestCode(context.Background(), "code")
	var orphan *ManifestOrphanError
	if !errors.Is(err, ErrProvider) || errors.As(err, &orphan) {
		t.Fatalf("stalled headers: %v", err)
	}
	_, err = newManifestClient(stalledBody.URL, 100*time.Millisecond).ConvertManifestCode(context.Background(), "code")
	if !errors.As(err, &orphan) || orphan.AppID != 42 || orphan.HTMLURL != "https://github.com/apps/x" {
		t.Fatalf("stalled body: %v %+v", err, orphan)
	}
}

func TestForbiddenOnlyRateLimitedWithHeaders(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) }))
	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(403)
	}))
	retry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(403)
	}))
	defer plain.Close()
	defer limited.Close()
	defer retry.Close()
	ctx := context.Background()
	if _, err := newManifestClient(plain.URL).ConvertManifestCode(ctx, "c"); !errors.Is(err, ErrProvider) || errors.Is(err, ErrRateLimited) {
		t.Fatalf("plain 403: %v", err)
	}
	for _, u := range []string{limited.URL, retry.URL} {
		if _, err := newManifestClient(u).ConvertManifestCode(ctx, "c"); !errors.Is(err, ErrRateLimited) {
			t.Fatalf("limited 403: %v", err)
		}
	}
}
