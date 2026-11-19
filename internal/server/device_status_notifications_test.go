package server

import (
	"testing"

	"vocat/internal/device"
)

func TestSnapshotRegistered(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		snapshot *device.Snapshot
		want     bool
	}{
		{"nil snapshot", nil, false},
		{"sim not ready", &device.Snapshot{RegistrationStatus: 1}, false},
		{"home registered", &device.Snapshot{SIMReady: true, RegistrationStatus: 1}, true},
		{"roaming registered", &device.Snapshot{SIMReady: true, RegistrationStatus: 5}, true},
		{"searching", &device.Snapshot{SIMReady: true, RegistrationStatus: 2}, false},
		{"denied", &device.Snapshot{SIMReady: true, RegistrationStatus: 3}, false},
		{"unknown", &device.Snapshot{SIMReady: true, RegistrationStatus: 4}, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := snapshotRegistered(testCase.snapshot); got != testCase.want {
				t.Fatalf("snapshotRegistered() = %v, want %v", got, testCase.want)
			}
		})
	}
}

func TestParseDeviceStatusConfig(t *testing.T) {
	t.Parallel()
	if _, err := parseDeviceStatusConfig(deviceStatusConfig{
		OfflineGraceSeconds: -1,
		CooldownSeconds:     600,
	}); err == nil {
		t.Fatal("negative grace must be rejected")
	}
	if _, err := parseDeviceStatusConfig(deviceStatusConfig{
		OfflineGraceSeconds: 60,
		CooldownSeconds:     5,
	}); err == nil {
		t.Fatal("cooldown below the floor must be rejected")
	}
	parsed, err := parseDeviceStatusConfig(deviceStatusConfig{
		Enabled:             true,
		DeviceOffline:       true,
		NetworkLost:         true,
		OfflineGraceSeconds: 0,
		CooldownSeconds:     600,
	})
	if err != nil {
		t.Fatal(err)
	}
	if parsed.OfflineGraceSeconds != 60 {
		t.Fatalf("zero grace should default to 60, got %d", parsed.OfflineGraceSeconds)
	}
}
