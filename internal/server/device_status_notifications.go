package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"vocat/internal/device"
	"vocat/internal/store"
)

const (
	deviceStatusSettingKey = "notifications.device_status"
	deviceStatusPollPeriod = 10 * time.Second
)

// deviceStatusConfig controls transition alerts for configured devices. The
// alert is deliberately transition-based: a device that is already unplugged
// when the server starts does not spam a notification on every restart, but a
// device that disappears while the server is running does.
type deviceStatusConfig struct {
	Enabled             bool `json:"enabled"`
	DeviceOffline       bool `json:"device_offline"`
	NetworkLost         bool `json:"network_lost"`
	OfflineGraceSeconds int  `json:"offline_grace_seconds"`
	CooldownSeconds     int  `json:"cooldown_seconds"`
}

func defaultDeviceStatusConfig() deviceStatusConfig {
	return deviceStatusConfig{
		Enabled:             false,
		DeviceOffline:       true,
		NetworkLost:         true,
		OfflineGraceSeconds: 60,
		CooldownSeconds:     600,
	}
}

func parseDeviceStatusConfig(config deviceStatusConfig) (deviceStatusConfig, error) {
	if config.OfflineGraceSeconds < 0 || config.OfflineGraceSeconds > 3600 {
		return deviceStatusConfig{}, errors.New("offline_grace_seconds must be between 0 and 3600")
	}
	if config.OfflineGraceSeconds == 0 {
		config.OfflineGraceSeconds = 60
	}
	if config.CooldownSeconds < 30 || config.CooldownSeconds > 86400 {
		return deviceStatusConfig{}, errors.New("cooldown_seconds must be between 30 and 86400")
	}
	return config, nil
}

func (s *Server) loadDeviceStatusConfig(ctx context.Context) deviceStatusConfig {
	config := defaultDeviceStatusConfig()
	setting, err := s.store.AppSetting(ctx, deviceStatusSettingKey)
	if err == nil {
		var stored deviceStatusConfig
		if json.Unmarshal(setting.Value, &stored) == nil {
			if parsed, parseErr := parseDeviceStatusConfig(stored); parseErr == nil {
				config = parsed
			}
		}
	} else if !errors.Is(err, store.ErrNotFound) && s.logger != nil {
		s.logger.Warn("load device status notification config", "error", err)
	}
	return config
}

// handleDeviceStatusNotifications reads and writes the device lifecycle alert
// options.
//
//	GET /api/settings/device-notifications
//	PUT /api/settings/device-notifications
func (s *Server) handleDeviceStatusNotifications(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"data": s.loadDeviceStatusConfig(r.Context())})
	case http.MethodPut:
		var request deviceStatusConfig
		if err := s.decodeJSON(w, r, &request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		config, err := parseDeviceStatusConfig(request)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_device_status_config", err.Error())
			return
		}
		payload, err := json.Marshal(config)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", "an internal error occurred")
			return
		}
		if err := s.store.UpsertAppSetting(r.Context(), store.AppSetting{
			Key:   deviceStatusSettingKey,
			Value: payload,
		}); err != nil {
			s.writeStoreError(w, err)
			return
		}
		s.audit(r, "settings.device_notifications.update", "settings", "device_notifications", "success")
		writeJSON(w, http.StatusOK, map[string]any{"data": config})
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}

type deviceStatusObservation struct {
	present       bool
	registered    bool
	dataConnected bool
	snapshot      *device.Snapshot
	model         string
	operator      string
	accessTech    string
}

type deviceStatusState struct {
	initialized      bool
	present          bool
	networkUp        bool
	presentSince     time.Time
	presentMissingAt time.Time
	networkDownAt    time.Time
	offlineNotified  bool
	networkNotified  bool
	lastNotified     map[string]time.Time
}

func (state *deviceStatusState) notifyAllowed(event string, now time.Time, cooldown time.Duration) bool {
	if state.lastNotified == nil {
		state.lastNotified = map[string]time.Time{}
	}
	last, ok := state.lastNotified[event]
	if ok && now.Sub(last) < cooldown {
		return false
	}
	state.lastNotified[event] = now
	return true
}

// StartDeviceStatusNotifications watches configured devices for hardware and
// registration transitions and pushes an alert through every enabled
// notification channel.
func (s *Server) StartDeviceStatusNotifications(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	go func() {
		states := map[string]*deviceStatusState{}
		ticker := time.NewTicker(deviceStatusPollPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.evaluateDeviceStatusNotifications(ctx, states)
			}
		}
	}()
}

func (s *Server) evaluateDeviceStatusNotifications(ctx context.Context, states map[string]*deviceStatusState) {
	config := s.loadDeviceStatusConfig(ctx)
	if !config.Enabled {
		// Drop history so re-enabling does not replay transitions that happened
		// while the feature was off.
		for id := range states {
			delete(states, id)
		}
		return
	}
	configured, err := s.store.ListDevices(ctx)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("list devices for status notification", "error", err)
		}
		return
	}
	now := time.Now()
	grace := time.Duration(config.OfflineGraceSeconds) * time.Second
	cooldown := time.Duration(config.CooldownSeconds) * time.Second
	for _, entry := range configured {
		observation := s.observeDeviceStatus(entry)
		state, ok := states[entry.ID]
		if !ok {
			state = &deviceStatusState{present: observation.present, networkUp: observation.registered}
			state.initialized = true
			states[entry.ID] = state
			continue
		}
		if !state.initialized {
			state.present = observation.present
			state.networkUp = observation.registered
			state.initialized = true
			continue
		}
		s.evaluateDevicePresence(ctx, entry, observation, state, now, grace, cooldown, config)
		s.evaluateDeviceNetwork(ctx, entry, observation, state, now, grace, cooldown, config)
	}
}

func (s *Server) evaluateDevicePresence(
	ctx context.Context,
	entry store.Device,
	observation deviceStatusObservation,
	state *deviceStatusState,
	now time.Time,
	grace time.Duration,
	cooldown time.Duration,
	config deviceStatusConfig,
) {
	if observation.present {
		if !state.present {
			state.present = true
			state.presentMissingAt = time.Time{}
			if state.offlineNotified {
				state.offlineNotified = false
				if state.notifyAllowed("device_online", now, cooldown) {
					s.sendDeviceStatusNotification(ctx, entry, observation,
						"设备已恢复在线",
						"硬件连接已恢复，设备重新可用。",
					)
				}
			}
		}
		return
	}
	if state.present {
		state.present = false
		state.presentMissingAt = now
	}
	if !config.DeviceOffline || state.offlineNotified {
		return
	}
	if state.presentMissingAt.IsZero() || now.Sub(state.presentMissingAt) < grace {
		return
	}
	if !state.notifyAllowed("device_offline", now, cooldown) {
		return
	}
	state.offlineNotified = true
	s.sendDeviceStatusNotification(ctx, entry, observation,
		"设备掉线",
		fmt.Sprintf("设备已与主机断开连接超过 %d 秒，请检查 USB 线缆、供电或模组状态。", config.OfflineGraceSeconds),
	)
}

func (s *Server) evaluateDeviceNetwork(
	ctx context.Context,
	entry store.Device,
	observation deviceStatusObservation,
	state *deviceStatusState,
	now time.Time,
	grace time.Duration,
	cooldown time.Duration,
	config deviceStatusConfig,
) {
	if observation.registered {
		if !state.networkUp {
			state.networkUp = true
			state.networkDownAt = time.Time{}
			if state.networkNotified {
				state.networkNotified = false
				if state.notifyAllowed("network_up", now, cooldown) {
					s.sendDeviceStatusNotification(ctx, entry, observation,
						"网络已恢复",
						"设备已重新注册到蜂窝网络。",
					)
				}
			}
		}
		return
	}
	if state.networkUp {
		state.networkUp = false
		state.networkDownAt = now
	}
	if !config.NetworkLost || state.networkNotified {
		return
	}
	if state.networkDownAt.IsZero() || now.Sub(state.networkDownAt) < grace {
		return
	}
	if !state.notifyAllowed("network_lost", now, cooldown) {
		return
	}
	state.networkNotified = true
	s.sendDeviceStatusNotification(ctx, entry, observation,
		"设备掉网",
		fmt.Sprintf("设备已持续 %d 秒未注册到蜂窝网络，请检查信号、SIM 卡与运营商状态。", config.OfflineGraceSeconds),
	)
}

func (s *Server) observeDeviceStatus(config store.Device) deviceStatusObservation {
	observation := deviceStatusObservation{}
	physical, _, present := s.physicalForConfig(config)
	if !present {
		return observation
	}
	observation.present = true
	observation.snapshot = physical.Snapshot
	if physical.Snapshot != nil {
		observation.model = strings.TrimSpace(physical.Snapshot.Model)
		observation.operator = strings.TrimSpace(physical.Snapshot.OperatorName)
		observation.accessTech = strings.TrimSpace(physical.Snapshot.AccessTech)
		observation.registered = snapshotRegistered(physical.Snapshot)
	}
	if runtime := s.cellularDataRuntime(); runtime != nil {
		observation.dataConnected = runtime.status(config.ID, config.NetworkEnabled).Connected
	}
	return observation
}

// snapshotRegistered reports whether the modem is attached to a cellular
// network. 3GPP registration status 1 (home) and 5 (roaming) both count.
func snapshotRegistered(snapshot *device.Snapshot) bool {
	if snapshot == nil || !snapshot.SIMReady {
		return false
	}
	return snapshot.RegistrationStatus == 1 || snapshot.RegistrationStatus == 5
}

func (s *Server) sendDeviceStatusNotification(
	ctx context.Context,
	config store.Device,
	observation deviceStatusObservation,
	title string,
	detail string,
) {
	label := firstNonEmpty(config.Name, config.ID)
	if observation.model != "" {
		label += "（" + observation.model + "）"
	}
	lines := []string{
		title,
		"设备  " + label,
		"时间  " + time.Now().Local().Format("2006-01-02 15:04:05"),
	}
	if observation.operator != "" {
		lines = append(lines, "运营商  "+observation.operator)
	}
	if observation.accessTech != "" {
		lines = append(lines, "网络  "+observation.accessTech)
	}
	lines = append(lines, "详情  "+detail)
	text := strings.Join(lines, "\n")
	destCtx := s.notificationDestinationContext(ctx)
	for _, channel := range []string{"telegram", "bark", "email", "pushplus", "gotify", "webhook", "wecom", "lark"} {
		setting, err := s.store.NotificationSetting(destCtx, channel)
		if errors.Is(err, store.ErrNotFound) || (err == nil && !setting.Enabled) {
			continue
		}
		if err != nil {
			if s.logger != nil {
				s.logger.Warn("read device status notification setting", "channel", channel, "error", err)
			}
			continue
		}
		var notificationConfig map[string]any
		if err := json.Unmarshal(setting.Config, &notificationConfig); err != nil {
			if s.logger != nil {
				s.logger.Warn("decode device status notification setting", "channel", channel, "error", err)
			}
			continue
		}
		if err := sendDeviceStatusNotificationText(destCtx, channel, notificationConfig, title, text); err != nil {
			if s.logger != nil {
				s.logger.Warn("send device status notification", "channel", channel, "device_id", config.ID, "raw_error", err)
			}
		}
	}
	if s.logger != nil {
		s.logger.Info("device status notification sent",
			"category", "notification", "event", "device.status",
			"device_id", config.ID, "title", title,
		)
	}
}

func sendDeviceStatusNotificationText(ctx context.Context, channel string, config map[string]any, title, text string) error {
	switch channel {
	case "telegram":
		return sendTelegramTextNotification(ctx, config, text)
	case "bark":
		return sendBarkTextNotification(ctx, config, title, text)
	case "email":
		return sendEmailTextNotification(ctx, config, title, text)
	case "pushplus":
		return sendPushplusTextNotification(ctx, config, title, text)
	case "gotify":
		return sendGotifyNotification(ctx, config, title, text)
	case "webhook":
		return sendDeviceStatusWebhook(ctx, config, title, text)
	case "wecom":
		return sendWecomNotification(ctx, config, wecomTextValues(title, text))
	case "lark":
		return sendLarkNotification(ctx, config, larkTextValues(title, text))
	default:
		return fmt.Errorf("unsupported notification channel %q", channel)
	}
}

func wecomTextValues(title, text string) wecomTemplateValues {
	return wecomTemplateValues{
		"event":        "device.status",
		"title":        title,
		"message":      text,
		"timestamp":    time.Now().UTC().Format(time.RFC3339),
		"content":      "",
		"number":       "",
		"device_id":    "",
		"device_name":  "",
		"device_label": "",
		"time":         time.Now().Local().Format("2006-01-02 15:04:05"),
	}
}

func larkTextValues(title, text string) larkTemplateValues {
	return larkTemplateValues{
		"event":        "device.status",
		"title":        title,
		"message":      text,
		"timestamp":    time.Now().UTC().Format(time.RFC3339),
		"content":      "",
		"number":       "",
		"device_id":    "",
		"device_name":  "",
		"device_label": "",
		"time":         time.Now().Local().Format("2006-01-02 15:04:05"),
	}
}

// sendDeviceStatusWebhook emits the same envelope style as the other event
// webhooks so existing receivers can distinguish the event from SMS pushes.
func sendDeviceStatusWebhook(ctx context.Context, config map[string]any, title, text string) error {
	payload, _ := json.Marshal(map[string]any{
		"event":     "device.status",
		"title":     title,
		"message":   text,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
	client, err := restrictedHTTPClient(ctx, durationMilliseconds(configInt(config, "timeout_ms"), 5*time.Second), "")
	if err != nil {
		return err
	}
	for _, destination := range configStrings(config, "urls") {
		parsed, err := validateOutboundURL(ctx, destination, false)
		if err != nil {
			return err
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, parsed.String(), strings.NewReader(string(payload)))
		if err != nil {
			return err
		}
		for name, value := range configStringMap(config, "headers") {
			request.Header.Set(name, value)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("User-Agent", "vocat-device-status/1")
		if err := performNotificationRequest(client, request, false); err != nil {
			return err
		}
	}
	return nil
}
