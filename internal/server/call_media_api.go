package server

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/coder/websocket"

	"vocat/internal/store"
	"vocat/internal/vowifi"
)

const maxCallMediaMessage = 16 << 10

// handleCallMedia upgrades an authenticated same-origin request to a binary
// PCM bridge. Each WebSocket message contains little-endian signed 16-bit,
// 8 kHz, mono samples. VoWiFi calls bridge the IMS RTP stream; cellular calls
// bridge the modem's USB Audio Class interfaces. Codec details stay inside the
// transport-specific implementation.
func (s *Server) handleCallMedia(w http.ResponseWriter, r *http.Request, config store.Device) bool {
	if !requireMethod(w, r, http.MethodGet) {
		return true
	}
	callID := strings.TrimSpace(r.URL.Query().Get("call_id"))
	if len(callID) > 256 {
		writeError(w, http.StatusBadRequest, "invalid_call_id", "call_id is invalid")
		return true
	}
	transport := s.callTransport(config.ID)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	var (
		media     vowifi.CallMedia
		logLevels bool
	)
	if transport == "vowifi" {
		if callID == "" {
			writeError(w, http.StatusBadRequest, "invalid_call_id", "call_id is required")
			return true
		}
		controller, ok := s.vowifi.(VoWiFiCallMediaController)
		if !ok {
			writeError(w, http.StatusNotImplemented, "call_media_unavailable", "the active IMS session does not expose RTP media")
			return true
		}
		opened, err := controller.CallMedia(ctx, config.ID, callID)
		if err != nil {
			writeError(w, http.StatusConflict, "call_media_unavailable", err.Error())
			return true
		}
		media = opened
	} else {
		if s.devices == nil {
			writeError(w, http.StatusServiceUnavailable, "device_manager_unavailable", "device manager is unavailable")
			return true
		}
		_, physicalID, present := s.physicalForConfig(config)
		if !present {
			writeError(w, http.StatusConflict, "call_media_unavailable", "device is offline")
			return true
		}
		if err := s.cellularMedia.acquire(config.ID, cancel); err != nil {
			writeError(w, http.StatusConflict, "call_media_busy", err.Error())
			return true
		}
		defer s.cellularMedia.release(config.ID)
		opened, err := s.openCellularCallMedia(ctx, config, physicalID)
		if err != nil {
			writeError(w, http.StatusConflict, "call_media_unavailable", err.Error())
			return true
		}
		media = opened
		logLevels = true
		if s.logger != nil {
			s.logger.Info("cellular call audio bridge opened",
				"category", "call", "event", "call.media_open",
				"device_id", config.ID, "call_id", callID,
				"codec", opened.Codec(),
			)
		}
	}

	connection, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return true
	}
	connection.SetReadLimit(maxCallMediaMessage)
	defer connection.Close(websocket.StatusNormalClosure, "call media closed")

	downlink := make(chan error, 1)
	go func() {
		defer cancel()
		level := &cellularAudioLevel{}
		for {
			samples, readErr := media.ReadPCM(ctx)
			if readErr != nil {
				downlink <- readErr
				return
			}
			if logLevels {
				s.logCellularCallAudio(config.ID, callID, samples, level)
			}
			payload := make([]byte, len(samples)*2)
			for index, sample := range samples {
				binary.LittleEndian.PutUint16(payload[index*2:], uint16(sample))
			}
			if writeErr := connection.Write(ctx, websocket.MessageBinary, payload); writeErr != nil {
				downlink <- writeErr
				return
			}
		}
	}()

	for {
		select {
		case err := <-downlink:
			if !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) && s.logger != nil {
				s.logger.Debug("call media downlink closed", "device_id", config.ID, "call_id", callID, "error", err)
			}
			return true
		default:
		}
		messageType, payload, readErr := connection.Read(ctx)
		if readErr != nil {
			return true
		}
		if messageType != websocket.MessageBinary || len(payload) == 0 || len(payload)%2 != 0 {
			continue
		}
		samples := make([]int16, len(payload)/2)
		for index := range samples {
			samples[index] = int16(binary.LittleEndian.Uint16(payload[index*2:]))
		}
		if err := media.WritePCM(samples); err != nil {
			return true
		}
	}
}
