package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGotifyMessageEndpoint(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw  string
		want string
		ok   bool
	}{
		{"https://gotify.example.com", "https://gotify.example.com/message", true},
		{"https://gotify.example.com/", "https://gotify.example.com/message", true},
		{"https://gotify.example.com/message", "https://gotify.example.com/message", true},
		{"http://192.168.1.50:8080", "http://192.168.1.50:8080/message", true},
		{"https://host.example/gotify", "https://host.example/gotify/message", true},
		{"", "", false},
		{"gotify.example.com", "", false},
		{"ftp://gotify.example.com", "", false},
		{"https://gotify.example.com/?token=x", "", false},
	}
	for _, test := range cases {
		got, err := gotifyMessageEndpoint(test.raw)
		if test.ok {
			if err != nil || got != test.want {
				t.Fatalf("gotifyMessageEndpoint(%q) = (%q, %v), want %q", test.raw, got, err, test.want)
			}
			continue
		}
		if err == nil {
			t.Fatalf("gotifyMessageEndpoint(%q) = %q, want error", test.raw, got)
		}
	}
}

func TestGotifyPriorityDefaultsAndClamps(t *testing.T) {
	t.Parallel()
	if got := gotifyPriority(map[string]any{}); got != defaultGotifyPriority {
		t.Fatalf("empty priority = %d, want %d", got, defaultGotifyPriority)
	}
	if got := gotifyPriority(map[string]any{"priority": float64(0)}); got != defaultGotifyPriority {
		t.Fatalf("zero priority = %d, want default", got)
	}
	if got := gotifyPriority(map[string]any{"priority": float64(9)}); got != 9 {
		t.Fatalf("priority 9 = %d, want 9", got)
	}
	if got := gotifyPriority(map[string]any{"priority": float64(64)}); got != maxGotifyPriority {
		t.Fatalf("high priority = %d, want clamp to %d", got, maxGotifyPriority)
	}
	if got := gotifyPriority(map[string]any{"priority": float64(-5)}); got != minGotifyPriority {
		t.Fatalf("low priority = %d, want clamp to %d", got, minGotifyPriority)
	}
}

func TestValidateGotifyNotificationConfig(t *testing.T) {
	t.Parallel()
	valid := map[string]any{"server_url": "https://gotify.example.com", "app_token": "A123", "priority": float64(5)}
	if err := validateGotifyNotificationConfig(valid); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	for name, config := range map[string]map[string]any{
		"missing token":    {"server_url": "https://gotify.example.com"},
		"missing server":   {"app_token": "A123"},
		"bad server":       {"server_url": "not-a-url", "app_token": "A123"},
		"priority too big": {"server_url": "https://gotify.example.com", "app_token": "A123", "priority": float64(50)},
	} {
		if err := validateGotifyNotificationConfig(config); err == nil {
			t.Fatalf("%s: expected validation error", name)
		}
	}
}

func TestPostGotifyMessageSendsAppTokenAndPayload(t *testing.T) {
	var (
		gotToken     string
		gotPayload   map[string]any
		gotUserAgent string
	)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if r.URL.Path != "/message" {
			t.Errorf("path = %q, want /message", r.URL.Path)
		}
		gotToken = r.Header.Get("X-Gotify-Key")
		gotUserAgent = r.Header.Get("User-Agent")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		if err := json.Unmarshal(body, &gotPayload); err != nil {
			t.Errorf("decode body: %v", err)
		}
		_, _ = io.WriteString(w, `{"id":1,"appid":2}`)
	}))
	t.Cleanup(provider.Close)

	payload, err := json.Marshal(map[string]any{"title": "收到新短信", "message": "hello", "priority": 7})
	if err != nil {
		t.Fatal(err)
	}
	if err := postGotifyMessage(context.Background(), provider.Client(), provider.URL+"/message", "A123token", payload); err != nil {
		t.Fatalf("postGotifyMessage() = %v", err)
	}
	if gotToken != "A123token" {
		t.Fatalf("X-Gotify-Key = %q, want A123token", gotToken)
	}
	if !strings.HasPrefix(gotUserAgent, "vocat-gotify-notification/") {
		t.Fatalf("User-Agent = %q", gotUserAgent)
	}
	if gotPayload["title"] != "收到新短信" || gotPayload["message"] != "hello" || gotPayload["priority"] != float64(7) {
		t.Fatalf("payload = %#v", gotPayload)
	}
}

func TestPostGotifyMessageRejectsProviderErrors(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"unauthorized","errorCode":401,"errorDescription":"bad token"}`)
	}))
	t.Cleanup(provider.Close)

	err := postGotifyMessage(context.Background(), provider.Client(), provider.URL+"/message", "bad", []byte(`{}`))
	if !errors.Is(err, errProviderRejected) {
		t.Fatalf("error = %v, want errProviderRejected", err)
	}
}

func TestGotifyNotificationSettingsMaskAppToken(t *testing.T) {
	test := newSettingsAPITest(t)
	recorder := test.request(
		t,
		http.MethodPut,
		"/api/settings/notifications",
		`{"gotify":{"enabled":true,"server_url":"https://gotify.example.com","app_token":"Asecret123","priority":8}}`,
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body = %s", recorder.Code, recorder.Body)
	}
	if strings.Contains(recorder.Body.String(), "Asecret123") {
		t.Fatalf("PUT response leaked gotify token: %s", recorder.Body)
	}
	var notificationResponse map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &notificationResponse); err != nil {
		t.Fatal(err)
	}
	gotify := notificationResponse["data"].(map[string]any)["gotify"].(map[string]any)
	if gotify["app_token"] != "********" {
		t.Fatalf("redacted app_token = %#v", gotify["app_token"])
	}
	if gotify["server_url"] != "https://gotify.example.com" {
		t.Fatalf("server_url = %#v", gotify["server_url"])
	}

	// Re-saving the masked value must preserve the stored token.
	recorder = test.request(
		t,
		http.MethodPut,
		"/api/settings/notifications",
		`{"gotify":{"enabled":true,"server_url":"https://gotify.example.com","app_token":"********","priority":8}}`,
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("second PUT status = %d, body = %s", recorder.Code, recorder.Body)
	}
	setting, err := test.database.NotificationSetting(context.Background(), "gotify")
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	if err := json.Unmarshal(setting.Config, &stored); err != nil {
		t.Fatal(err)
	}
	if stored["app_token"] != "Asecret123" {
		t.Fatalf("stored app_token = %#v, want preserved secret", stored["app_token"])
	}
}

func TestDeviceStatusNotificationConfigRoundTrip(t *testing.T) {
	test := newSettingsAPITest(t)
	recorder := test.request(t, http.MethodGet, "/api/settings/device-notifications", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET status = %d, body = %s", recorder.Code, recorder.Body)
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	defaults := response["data"].(map[string]any)
	if defaults["enabled"] != false || defaults["device_offline"] != true || defaults["network_lost"] != true {
		t.Fatalf("default config = %#v", defaults)
	}

	recorder = test.request(
		t,
		http.MethodPut,
		"/api/settings/device-notifications",
		`{"enabled":true,"device_offline":true,"network_lost":false,"offline_grace_seconds":90,"cooldown_seconds":300}`,
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body = %s", recorder.Code, recorder.Body)
	}
	stored := test.server.loadDeviceStatusConfig(context.Background())
	if !stored.Enabled || !stored.DeviceOffline || stored.NetworkLost || stored.OfflineGraceSeconds != 90 || stored.CooldownSeconds != 300 {
		t.Fatalf("stored config = %#v", stored)
	}

	recorder = test.request(
		t,
		http.MethodPut,
		"/api/settings/device-notifications",
		`{"enabled":true,"device_offline":true,"network_lost":true,"offline_grace_seconds":10,"cooldown_seconds":1}`,
	)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid PUT status = %d, body = %s", recorder.Code, recorder.Body)
	}
}
