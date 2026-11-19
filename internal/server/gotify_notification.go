package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Gotify delivers push messages through a self-hosted Gotify server. The
// application token selects the destination app; the optional priority maps to
// Gotify's -2..10 scale and controls notification importance on the client.

const (
	defaultGotifyPriority = 5
	minGotifyPriority     = 0
	maxGotifyPriority     = 10
)

// gotifyMessageEndpoint normalizes an administrator-supplied server URL into
// the message API endpoint. Accepting a bare origin keeps the settings form
// forgiving: "https://gotify.example.com" and the fully qualified
// ".../message" form both resolve to the same endpoint.
func gotifyMessageEndpoint(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", errors.New("gotify.server_url is required")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || !parsed.IsAbs() {
		return "", errors.New("gotify.server_url must be an absolute HTTP URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("gotify.server_url must use HTTP or HTTPS")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("gotify.server_url must not contain a query or fragment")
	}
	path := strings.TrimSuffix(parsed.Path, "/")
	if !strings.HasSuffix(path, "/message") {
		path += "/message"
	}
	parsed.Path = path
	parsed.RawPath = ""
	return parsed.String(), nil
}

func gotifyPriority(config map[string]any) int {
	priority := configInt(config, "priority")
	if priority == 0 {
		// A zero priority is indistinguishable from an unset field in the
		// stored JSON. Treat it as "use the default", matching the settings UI.
		priority = defaultGotifyPriority
	}
	if priority < minGotifyPriority {
		return minGotifyPriority
	}
	if priority > maxGotifyPriority {
		return maxGotifyPriority
	}
	return priority
}

func validateGotifyNotificationConfig(config map[string]any) error {
	if _, err := gotifyMessageEndpoint(configString(config, "server_url")); err != nil {
		return err
	}
	token := strings.TrimSpace(configString(config, "app_token"))
	if token == "" {
		return errors.New("gotify.app_token is required")
	}
	priority := configInt(config, "priority")
	if priority != 0 && (priority < minGotifyPriority || priority > maxGotifyPriority) {
		return fmt.Errorf("gotify.priority must be between %d and %d", minGotifyPriority, maxGotifyPriority)
	}
	return nil
}

func sendGotifyNotification(ctx context.Context, config map[string]any, title, text string) error {
	endpoint, err := gotifyMessageEndpoint(configString(config, "server_url"))
	if err != nil {
		return err
	}
	token := strings.TrimSpace(configString(config, "app_token"))
	if token == "" {
		return errors.New("gotify.app_token is required")
	}
	target, err := validateOutboundURL(ctx, endpoint, false)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]any{
		"title":    title,
		"message":  text,
		"priority": gotifyPriority(config),
	})
	if err != nil {
		return fmt.Errorf("encode gotify notification: %w", err)
	}
	client, err := restrictedHTTPClient(ctx, 8*time.Second, "")
	if err != nil {
		return err
	}
	return postGotifyMessage(ctx, client, target.String(), token, payload)
}

// postGotifyMessage performs the actual Gotify HTTP exchange. It is split out
// so tests can drive it with an httptest client that bypasses destination
// policy, mirroring the other provider helpers.
func postGotifyMessage(ctx context.Context, client *http.Client, endpoint, token string, payload []byte) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("create gotify notification request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	request.Header.Set("X-Gotify-Key", token)
	request.Header.Set("User-Agent", "vocat-gotify-notification/1")
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("send gotify notification: %w", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		detail := strings.TrimSpace(string(body))
		if detail == "" {
			detail = http.StatusText(response.StatusCode)
		}
		if len(detail) > 200 {
			detail = detail[:200]
		}
		return fmt.Errorf("%w: Gotify HTTP %d: %s", errProviderRejected, response.StatusCode, detail)
	}
	return nil
}

func sendGotifyNotificationTest(ctx context.Context, config map[string]any) error {
	return sendGotifyNotification(ctx, config, "VoCat 测试通知", "通知通道配置正常。")
}
