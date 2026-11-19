package server

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"vocat/internal/audio"
	"vocat/internal/store"
	"vocat/internal/vowifi"
)

var (
	errVoiceAudioUnavailable = errors.New("cellular voice audio is unavailable")
	errVoiceAudioBusy        = errors.New("another browser call owns the modem audio interface")
)

// cellularCallMedia adapts the ALSA/USB-audio bridge to the same interface the
// VoWiFi RTP bridge implements, so the WebSocket handler is transport-agnostic.
type cellularCallMedia struct {
	bridge *audio.Bridge
}

func (media *cellularCallMedia) Codec() string { return media.bridge.Codec() }

func (media *cellularCallMedia) ReadPCM(ctx context.Context) ([]int16, error) {
	samples, err := media.bridge.ReadPCM(ctx)
	if err != nil {
		return nil, err
	}
	return samples, nil
}

func (media *cellularCallMedia) WritePCM(samples []int16) error {
	return media.bridge.WritePCM(samples)
}

// cellularCallMediaRegistry prevents two browser tabs from opening the same
// modem audio card. ALSA allows only one capture and one playback client on
// the hardware device, so the second open would fail anyway; the registry turns
// that into a clear message and makes cleanup deterministic.
type cellularCallMediaRegistry struct {
	mu     sync.Mutex
	active map[string]context.CancelFunc
}

func (registry *cellularCallMediaRegistry) acquire(deviceID string, cancel context.CancelFunc) error {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.active == nil {
		registry.active = map[string]context.CancelFunc{}
	}
	if _, busy := registry.active[deviceID]; busy {
		return errVoiceAudioBusy
	}
	registry.active[deviceID] = cancel
	return nil
}

func (registry *cellularCallMediaRegistry) release(deviceID string) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	delete(registry.active, deviceID)
}

// prepareCellularVoice routes voice audio to the module's USB Audio Class
// interface. AT+QPCMV=1,2 is the Quectel command chan_quectel/FreePBX uses for
// the same hardware; it is idempotent and returns in milliseconds.
func (s *Server) prepareCellularVoice(ctx context.Context, physicalID string) error {
	if s.devices == nil {
		return errors.New("device manager is unavailable")
	}
	operationContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := s.devices.ExecuteAT(operationContext, physicalID, "AT+QPCMV=1,2"); err != nil {
		return fmt.Errorf("enable USB voice audio (AT+QPCMV=1,2): %w", err)
	}
	// Keep the module output at a usable level; this fails harmlessly on
	// firmware variants that do not expose the command.
	if _, err := s.devices.ExecuteAT(operationContext, physicalID, "AT+CLVL=5"); err != nil && s.logger != nil {
		s.logger.Debug("set modem call volume failed", "device_id", physicalID, "error", err)
	}
	return nil
}

// openCellularCallMedia opens the modem's USB audio card for one browser call.
func (s *Server) openCellularCallMedia(ctx context.Context, config store.Device, physicalID string) (vowifi.CallMedia, error) {
	if err := s.prepareCellularVoice(ctx, physicalID); err != nil {
		if s.logger != nil {
			s.logger.Warn("prepare cellular voice audio", "device_id", config.ID, "raw_error", err)
		}
	}
	bridge, err := audio.Open(ctx, config.AudioDevice)
	if err != nil {
		if errors.Is(err, audio.ErrToolsMissing) || errors.Is(err, audio.ErrNoDevice) {
			return nil, fmt.Errorf("%w: %v", errVoiceAudioUnavailable, err)
		}
		return nil, err
	}
	return &cellularCallMedia{bridge: bridge}, nil
}

// logCellularCallAudio periodically reports the downlink level so operators can
// tell "no audio" apart from "network silence" in the server logs.
func (s *Server) logCellularCallAudio(deviceID, callID string, samples []int16, state *cellularAudioLevel) {
	if len(samples) == 0 {
		return
	}
	var sumSquares float64
	peak := 0.0
	for _, sample := range samples {
		value := float64(sample)
		sumSquares += value * value
		if math.Abs(value) > peak {
			peak = math.Abs(value)
		}
	}
	state.squares += sumSquares
	state.peak = math.Max(state.peak, peak)
	state.total += float64(len(samples))
	if state.total < float64(audio.SampleRate) {
		return
	}
	rms := math.Sqrt(state.squares / state.total)
	if s.logger != nil {
		level := "silence"
		if rms > 50 {
			level = "audio"
		}
		s.logger.Debug("cellular call downlink level",
			"category", "call", "event", "call.media",
			"device_id", deviceID, "call_id", callID,
			"rms", int(rms), "peak", int(state.peak), "level", level,
		)
	}
	state.squares, state.peak, state.total = 0, 0, 0
}

type cellularAudioLevel struct {
	squares float64
	peak    float64
	total   float64
}
