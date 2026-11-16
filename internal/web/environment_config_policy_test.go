package web

import (
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"autodeploy/internal/healthgate"
	"autodeploy/internal/store/postgres"
)

func postConfigForm(t *testing.T, h *Handler, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "https://admin.example.test/environments/config", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://admin.example.test")
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "v1.AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	return rr
}

func getConfigPage(t *testing.T, h *Handler) string {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "https://admin.example.test/environments/config?environment_id=env-a", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "v1.AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("get = %d body=%s", rr.Code, rr.Body.String())
	}
	return rr.Body.String()
}

// TestEnvironmentConfigGetRendersHealthPolicy proves the page shows the five
// policy fields with their stored values and attributes derived from the
// healthgate bounds.
func TestEnvironmentConfigGetRendersHealthPolicy(t *testing.T) {
	h, admin, _ := runtimeConfigHandler(t)
	admin.getFound = true
	admin.getCfg = postgres.RuntimeConfig{
		EnvironmentID: "env-a", HealthPolicyRevision: 1, Revision: 1,
		RuntimeConfigInput: postgres.RuntimeConfigInput{
			ContainerPort: 8080, ContainerScheme: "http", ReleaseStrategy: "stop_then_start", DrainSeconds: 30, CPUMillis: 500, MemoryBytes: 268435456, RestartPolicy: "unless-stopped",
			HealthPolicy: healthgate.Policy{SuccessThreshold: 5, IntervalSeconds: 4, TimeoutSeconds: 3, DeadlineSeconds: 120, TCPPort: 8443},
		},
	}
	body := getConfigPage(t, h)
	for _, want := range []string{
		"Health successes required", "Health probe interval (s)", "Health probe timeout (s)", "Health deadline (s)", "Extra TCP check port (optional)",
		`name="health_success_threshold"`, `name="health_interval_seconds"`, `name="health_timeout_seconds"`, `name="health_deadline_seconds"`, `name="health_tcp_port"`,
		`value="5"`, `value="4"`, `value="3"`, `value="120"`, `value="8443"`,
		fmt.Sprintf(`min="%d" max="%d"`, healthgate.MinSuccessThreshold, healthgate.MaxSuccessThreshold),
		fmt.Sprintf(`min="%d" max="%d"`, healthgate.MinIntervalSeconds, healthgate.MaxIntervalSeconds),
		fmt.Sprintf(`min="%d" max="%d"`, healthgate.MinTimeoutSeconds, healthgate.MaxTimeoutSeconds),
		fmt.Sprintf(`min="%d" max="%d"`, healthgate.MinDeadlineSeconds, healthgate.MaxDeadlineSeconds),
		fmt.Sprintf(`min="%d" max="%d"`, healthgate.MinTCPPort, healthgate.MaxTCPPort),
		`inputmode="numeric"`, `maxlength="5"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("page missing %q: %s", want, body)
		}
	}
}

// TestEnvironmentConfigGetHealthPolicyDefaults proves defaults 3/2/2/60 and an
// empty TCP port render both for a stored default policy (NULL port) and for
// the create form.
func TestEnvironmentConfigGetHealthPolicyDefaults(t *testing.T) {
	for _, found := range []bool{false, true} {
		h, admin, _ := runtimeConfigHandler(t)
		admin.getFound = found
		if found {
			admin.getCfg = postgres.RuntimeConfig{
				EnvironmentID: "env-a", HealthPolicyRevision: 1, Revision: 1,
				RuntimeConfigInput: postgres.RuntimeConfigInput{ContainerPort: 8080, ContainerScheme: "http", ReleaseStrategy: "stop_then_start", DrainSeconds: 30, CPUMillis: 500, MemoryBytes: 1, RestartPolicy: "unless-stopped", HealthPolicy: healthgate.DefaultPolicy()},
			}
		}
		body := getConfigPage(t, h)
		for _, want := range []string{
			`name="health_success_threshold"`, `name="health_tcp_port"`,
		} {
			if !strings.Contains(body, want) {
				t.Fatalf("found=%v page missing %q", found, want)
			}
		}
		for _, field := range []struct{ name, value string }{
			{"health_success_threshold", "3"}, {"health_interval_seconds", "2"}, {"health_timeout_seconds", "2"}, {"health_deadline_seconds", "60"}, {"health_tcp_port", ""},
		} {
			if !strings.Contains(inputTag(t, body, field.name), fmt.Sprintf(`value="%s"`, field.value)) {
				t.Fatalf("found=%v %s default not %q: %s", found, field.name, field.value, inputTag(t, body, field.name))
			}
		}
	}
}

// inputTag returns the <input ...> tag for name.
func inputTag(t *testing.T, body, name string) string {
	t.Helper()
	i := strings.Index(body, `<input name="`+name+`"`)
	if i < 0 {
		t.Fatalf("no input %s in page", name)
	}
	j := strings.Index(body[i:], ">")
	return body[i : i+j+1]
}

func TestEnvironmentConfigPostPassesHealthPolicy(t *testing.T) {
	for _, tcp := range []struct {
		raw  string
		want int
	}{{"", 0}, {"8443", 8443}} {
		h, admin, p := runtimeConfigHandler(t)
		form := validConfigForm("env-a", serversCSRF(t, h, p), "")
		form.Set("health_success_threshold", "5")
		form.Set("health_interval_seconds", "4")
		form.Set("health_timeout_seconds", "3")
		form.Set("health_deadline_seconds", "120")
		form.Set("health_tcp_port", tcp.raw)
		rr := postConfigForm(t, h, form)
		if rr.Code != http.StatusSeeOther || admin.createCalls != 1 {
			t.Fatalf("tcp %q: code=%d createCalls=%d body=%s", tcp.raw, rr.Code, admin.createCalls, rr.Body.String())
		}
		want := healthgate.Policy{SuccessThreshold: 5, IntervalSeconds: 4, TimeoutSeconds: 3, DeadlineSeconds: 120, TCPPort: tcp.want}
		if admin.createInput.HealthPolicy != want {
			t.Fatalf("policy = %+v, want %+v", admin.createInput.HealthPolicy, want)
		}
	}
}

func TestEnvironmentConfigInvalidHealthPolicyRejectedBeforeWrite(t *testing.T) {
	cases := []struct {
		name   string
		set    map[string]string
		mirror bool // the rejected raw value must be echoed back
	}{
		{"threshold zero", map[string]string{"health_success_threshold": "0"}, true},
		{"threshold 11", map[string]string{"health_success_threshold": "11"}, true},
		{"interval zero", map[string]string{"health_interval_seconds": "0"}, true},
		{"interval 31", map[string]string{"health_interval_seconds": "31"}, true},
		{"timeout zero", map[string]string{"health_timeout_seconds": "0"}, true},
		{"timeout 31", map[string]string{"health_timeout_seconds": "31", "health_interval_seconds": "30"}, true},
		{"deadline 9", map[string]string{"health_deadline_seconds": "9"}, true},
		{"deadline 601", map[string]string{"health_deadline_seconds": "601"}, true},
		{"tcp zero", map[string]string{"health_tcp_port": "0"}, true},
		{"tcp 65536", map[string]string{"health_tcp_port": "65536"}, true},
		{"non numeric", map[string]string{"health_success_threshold": "abc"}, true},
		{"empty threshold", map[string]string{"health_success_threshold": ""}, false},
		{"plus sign", map[string]string{"health_success_threshold": "+3"}, true},
		{"minus sign", map[string]string{"health_interval_seconds": "-2"}, true},
		{"leading space", map[string]string{"health_deadline_seconds": " 60"}, true},
		{"trailing space", map[string]string{"health_tcp_port": "80 "}, true},
		{"hex", map[string]string{"health_tcp_port": "0x50"}, true},
		{"overflow", map[string]string{"health_deadline_seconds": "99999999999999999999"}, true},
		{"timeout above interval", map[string]string{"health_interval_seconds": "2", "health_timeout_seconds": "3"}, true},
		{"deadline below threshold*interval", map[string]string{"health_success_threshold": "10", "health_interval_seconds": "30", "health_timeout_seconds": "2", "health_deadline_seconds": "100"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, admin, p := runtimeConfigHandler(t)
			form := validConfigForm("env-a", serversCSRF(t, h, p), "")
			for k, v := range tc.set {
				form.Set(k, v)
			}
			rr := postConfigForm(t, h, form)
			if rr.Code != http.StatusOK {
				t.Fatalf("code = %d body=%s", rr.Code, rr.Body.String())
			}
			if admin.createCalls != 0 || admin.updateCalls != 0 {
				t.Fatal("invalid health policy reached the repository")
			}
			body := rr.Body.String()
			if !strings.Contains(body, "health policy") {
				t.Fatalf("missing health policy validation message: %s", body)
			}
			if tc.mirror {
				for _, v := range tc.set {
					if !strings.Contains(body, `value="`+strings.ReplaceAll(html.EscapeString(v), "+", "&#43;")+`"`) {
						t.Fatalf("submitted %q not re-displayed: %s", v, body)
					}
				}
			}
		})
	}
}

// TestEnvironmentConfigOldShapedFormRejected documents the missing-field
// decision: the form is strict (exact field set); the rendered form always
// sends all five policy fields, so an old-shaped 12-field post is a 400.
func TestEnvironmentConfigOldShapedFormRejected(t *testing.T) {
	for _, drop := range []string{"health_success_threshold", "health_interval_seconds", "health_timeout_seconds", "health_deadline_seconds", "health_tcp_port", ""} {
		h, admin, p := runtimeConfigHandler(t)
		form := validConfigForm("env-a", serversCSRF(t, h, p), "")
		if drop == "" {
			for _, k := range []string{"health_success_threshold", "health_interval_seconds", "health_timeout_seconds", "health_deadline_seconds", "health_tcp_port"} {
				form.Del(k)
			}
		} else {
			form.Del(drop)
		}
		rr := postConfigForm(t, h, form)
		if rr.Code != http.StatusBadRequest || admin.createCalls != 0 {
			t.Fatalf("drop %q: code=%d createCalls=%d", drop, rr.Code, admin.createCalls)
		}
	}
}
