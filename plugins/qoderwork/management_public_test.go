package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func decodeManagementResponse(t *testing.T, raw []byte) pluginapi.ManagementResponse {
	t.Helper()
	var env struct {
		OK     bool                         `json:"ok"`
		Result pluginapi.ManagementResponse `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK {
		t.Fatalf("bad envelope: %s", raw)
	}
	return env.Result
}

func runPublicPanelRequest(t *testing.T, method, body string, headers http.Header) pluginapi.ManagementResponse {
	t.Helper()
	raw, err := json.Marshal(pluginapi.ManagementRequest{
		Method:  method,
		Path:    "/v0/resource/plugins/qoderwork/panel",
		Headers: headers,
		Body:    []byte(body),
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := handleManagement(raw)
	if err != nil {
		t.Fatal(err)
	}
	return decodeManagementResponse(t, out)
}

func TestPanelUsesPublicResourceWithoutManagementKey(t *testing.T) {
	resp := runPublicPanelRequest(t, http.MethodGet, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, resp.Body)
	}
	page := string(resp.Body)
	for _, want := range []string{
		"fetch(location.pathname",
		"PANEL_CSRF",
		`api("accounts"`,
		`api("refresh"`,
		`api("checkin"`,
		`api("checkin-config"`,
		`api("credits"`,
		`api("import"`,
		`api("select"`,
		`api("claim-pro"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("panel missing %q", want)
		}
	}
	for _, forbidden := range []string{
		"/v0/management/plugins/qoderwork",
		"const API =",
		"management key",
		"managementKey",
		"qoderwork-mgmt-key",
		"Authorization",
		"authBox",
		"keyInput",
	} {
		if strings.Contains(page, forbidden) {
			t.Errorf("panel still depends on management auth %q", forbidden)
		}
	}
}

func TestPublicPanelRejectsUnsafeRequests(t *testing.T) {
	csrf := initPanelCSRF()
	goodHeaders := http.Header{
		"Content-Type":     {"application/x-www-form-urlencoded"},
		"X-Requested-With": {"qoderwork-panel"},
	}
	tests := []struct {
		name    string
		method  string
		body    string
		headers http.Header
		want    int
	}{
		{name: "missing csrf", method: http.MethodPost, body: "action=accounts", headers: goodHeaders, want: http.StatusForbidden},
		{name: "missing requested with", method: http.MethodPost, body: "action=accounts&csrf=" + csrf, headers: http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}, want: http.StatusForbidden},
		{name: "wrong content type", method: http.MethodPost, body: "action=accounts&csrf=" + csrf, headers: http.Header{"Content-Type": {"application/json"}, "X-Requested-With": {"qoderwork-panel"}}, want: http.StatusUnsupportedMediaType},
		{name: "duplicate action", method: http.MethodPost, body: "action=accounts&action=refresh&csrf=" + csrf, headers: goodHeaders, want: http.StatusBadRequest},
		{name: "unknown field", method: http.MethodPost, body: "action=accounts&csrf=" + csrf + "&extra=1", headers: goodHeaders, want: http.StatusBadRequest},
		{name: "claim without confirmation", method: http.MethodPost, body: "action=claim-pro&auth_index=1&csrf=" + csrf, headers: goodHeaders, want: http.StatusBadRequest},
		{name: "unknown action", method: http.MethodPost, body: "action=delete&csrf=" + csrf, headers: goodHeaders, want: http.StatusBadRequest},
		{name: "method", method: http.MethodPut, headers: goodHeaders, want: http.StatusMethodNotAllowed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := runPublicPanelRequest(t, tc.method, tc.body, tc.headers)
			if resp.StatusCode != tc.want {
				t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, tc.want, resp.Body)
			}
		})
	}

	large := strings.Repeat("x", (64<<10)+1)
	resp := runPublicPanelRequest(t, http.MethodPost, large, goodHeaders)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("large status=%d body=%s", resp.StatusCode, resp.Body)
	}
}

func TestPublicPanelCheckinConfigAppliesWithoutAuthorization(t *testing.T) {
	checkinAutoMu.RLock()
	old := checkinAuto
	checkinAutoMu.RUnlock()
	defer func() {
		checkinAutoMu.Lock()
		checkinAuto = old
		checkinAutoMu.Unlock()
	}()

	body := "action=checkin-config&enabled=false&csrf=" + initPanelCSRF()
	resp := runPublicPanelRequest(t, http.MethodPost, body, http.Header{
		"Content-Type":     {"application/x-www-form-urlencoded"},
		"X-Requested-With": {"qoderwork-panel"},
	})
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(resp.Body), `"checkin_auto":false`) {
		t.Fatalf("status=%d body=%s", resp.StatusCode, resp.Body)
	}
}

func TestPublicPanelAccountsNeedsNoAuthorization(t *testing.T) {
	body := "action=accounts&csrf=" + initPanelCSRF()
	resp := runPublicPanelRequest(t, http.MethodPost, body, http.Header{
		"Content-Type":     {"application/x-www-form-urlencoded"},
		"X-Requested-With": {"qoderwork-panel"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, resp.Body)
	}
	if strings.Contains(strings.ToLower(string(resp.Body)), "management key") {
		t.Fatalf("management auth leaked into response: %s", resp.Body)
	}
}
