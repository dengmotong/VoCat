// Package audio bridges the modem's USB audio interface to 8 kHz PCM that the
// Web UI can play and record. Cellular voice on Quectel modules is routed over
// the module's USB Audio Class interfaces (enabled with AT+QPCMV=1,2), exactly
// like chan_quectel on FreePBX: ALSA capture carries the downlink, ALSA
// playback carries the uplink.
//
// The implementation shells out to alsa-utils (arecord/aplay) instead of using
// cgo so the server keeps building with CGO_ENABLED=0 for static images.
package audio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// SampleRate is the telephony rate exposed by the modem's USB audio codec.
	SampleRate = 8000
	// Channels is mono for both directions.
	Channels = 1
	// PeriodSamples is one ALSA period: 40 ms at 8 kHz. Keeping it small bounds
	// the mouth-to-ear latency while leaving the kernel enough work per wakeup.
	PeriodSamples = 320
	periodBytes   = PeriodSamples * 2
)

var (
	ErrClosed       = errors.New("audio: bridge is closed")
	ErrNoDevice     = errors.New("audio: no modem USB audio device found")
	ErrToolsMissing = errors.New("audio: alsa-utils (arecord/aplay) is not installed")
	cardLinePattern = regexp.MustCompile(`^\s*(\d+)\s+\[([^\]]*)\]:\s*(\S+)\s*-\s*(.*)$`)
)

// detectionSettleDelay is the gap between opening playback and opening capture.
// The EC20's USB Audio Class gadget reconfigures its alternate setting when the
// playback stream opens; starting capture in that window aborts it with
// "pcm_read: I/O error". Waiting a few hundred milliseconds makes duplex
// reliable and costs nothing on a call that lasts seconds or minutes.
const detectionSettleDelay = 300 * time.Millisecond

type syncBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (buffer *syncBuffer) Write(payload []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.Write(payload)
}

func (buffer *syncBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return strings.TrimSpace(buffer.buffer.String())
}

// Bridge owns one capture and one playback stream on the same ALSA card.
type Bridge struct {
	device       string
	capture      *exec.Cmd
	playback     *exec.Cmd
	reader       io.ReadCloser
	writer       io.WriteCloser
	writeQueue   chan []int16
	captureErr   error
	captureOut   *syncBuffer
	playbackOut  *syncBuffer
	errMu        sync.Mutex
	closeOnce    sync.Once
	closed       chan struct{}
	captureDone  chan struct{}
	playbackDone chan struct{}
	writeFailed  chan struct{}
}

// DetectDevice picks the ALSA card exported by a Quectel modem. It returns an
// empty string when no candidate exists.
func DetectDevice() string {
	raw, err := os.ReadFile("/proc/asound/cards")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		match := cardLinePattern.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		index := match[1]
		id := strings.ToUpper(match[2] + " " + match[4])
		if strings.Contains(id, "EC20") || strings.Contains(id, "EC25") ||
			strings.Contains(id, "QUECTEL") || strings.Contains(id, "EC2X") {
			return "plughw:" + index + ",0"
		}
	}
	return ""
}

func resolveDevice(configured string) (string, error) {
	configured = strings.TrimSpace(configured)
	if configured != "" {
		return configured, nil
	}
	detected := DetectDevice()
	if detected == "" {
		return "", ErrNoDevice
	}
	return detected, nil
}

func requireTools() error {
	if _, err := exec.LookPath("arecord"); err != nil {
		return ErrToolsMissing
	}
	if _, err := exec.LookPath("aplay"); err != nil {
		return ErrToolsMissing
	}
	return nil
}

func alsaArgs(device string) []string {
	return []string{
		"-q", "-D", device,
		"-f", "S16_LE",
		"-r", strconv.Itoa(SampleRate),
		"-c", strconv.Itoa(Channels),
		"-t", "raw",
		"--period-size=" + strconv.Itoa(PeriodSamples),
		"--buffer-size=" + strconv.Itoa(PeriodSamples*4),
	}
}

// Open starts full-duplex PCM on the modem's USB audio card. configured may be
// an ALSA PCM name such as "plughw:0,0"; when empty the modem is auto-detected.
func Open(ctx context.Context, configured string) (*Bridge, error) {
	if err := requireTools(); err != nil {
		return nil, err
	}
	device, err := resolveDevice(configured)
	if err != nil {
		return nil, err
	}
	bridge := &Bridge{
		device:       device,
		writeQueue:   make(chan []int16, 8),
		closed:       make(chan struct{}),
		captureDone:  make(chan struct{}),
		playbackDone: make(chan struct{}),
		writeFailed:  make(chan struct{}, 1),
		captureOut:   &syncBuffer{},
		playbackOut:  &syncBuffer{},
	}

	playback := exec.CommandContext(ctx, "aplay", alsaArgs(device)...)
	playback.Stderr = bridge.playbackOut
	writer, err := playback.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("audio: playback pipe: %w", err)
	}
	if err := playback.Start(); err != nil {
		return nil, fmt.Errorf("audio: start aplay on %s: %w", device, err)
	}
	bridge.playback = playback
	bridge.writer = writer
	go func() {
		_ = playback.Wait()
		close(bridge.playbackDone)
	}()
	go bridge.writeLoop()

	// See detectionSettleDelay: capture must not race the playback alt-setting.
	time.Sleep(detectionSettleDelay)
	if !processAlive(playback) {
		bridge.killProcesses()
		return nil, fmt.Errorf("audio: aplay exited on %s: %s", device, bridge.playbackOut.String())
	}

	capture := exec.CommandContext(ctx, "arecord", alsaArgs(device)...)
	capture.Stderr = bridge.captureOut
	reader, err := capture.StdoutPipe()
	if err != nil {
		bridge.killProcesses()
		return nil, fmt.Errorf("audio: capture pipe: %w", err)
	}
	if err := capture.Start(); err != nil {
		bridge.killProcesses()
		return nil, fmt.Errorf("audio: start arecord on %s: %w", device, err)
	}
	bridge.capture = capture
	bridge.reader = reader
	go func() {
		_ = capture.Wait()
		close(bridge.captureDone)
	}()
	// Verify the capture stream survived opening; EC20 aborts immediately when
	// the gadget is misconfigured.
	select {
	case <-bridge.captureDone:
		bridge.killProcesses()
		return nil, fmt.Errorf("audio: arecord exited on %s: %s", device, bridge.captureOut.String())
	case <-time.After(150 * time.Millisecond):
	}
	return bridge, nil
}

func processAlive(command *exec.Cmd) bool {
	if command.Process == nil {
		return false
	}
	return command.Process.Signal(syscall.Signal(0)) == nil
}

// Device reports the ALSA PCM opened by the bridge.
func (bridge *Bridge) Device() string { return bridge.device }

// Codec describes the wire format for diagnostics and the settings UI.
func (bridge *Bridge) Codec() string { return "PCMU/PCMA via USB audio (S16_LE 8 kHz mono)" }

func (bridge *Bridge) writeLoop() {
	for {
		select {
		case <-bridge.closed:
			return
		case samples, ok := <-bridge.writeQueue:
			if !ok {
				return
			}
			payload := make([]byte, len(samples)*2)
			for index, sample := range samples {
				payload[index*2] = byte(sample)
				payload[index*2+1] = byte(uint16(sample) >> 8)
			}
			if _, err := bridge.writer.Write(payload); err != nil {
				select {
				case bridge.writeFailed <- struct{}{}:
				default:
				}
				return
			}
		}
	}
}

// ReadPCM blocks until one period of downlink audio is available.
func (bridge *Bridge) ReadPCM(ctx context.Context) ([]int16, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-bridge.closed:
		return nil, ErrClosed
	default:
	}
	buffer := make([]byte, periodBytes)
	if _, err := io.ReadFull(bridge.reader, buffer); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("audio: capture ended: %s", bridge.captureOut.String())
		}
		return nil, fmt.Errorf("audio: capture read: %w", err)
	}
	samples := make([]int16, len(buffer)/2)
	for index := range samples {
		samples[index] = int16(uint16(buffer[index*2]) | uint16(buffer[index*2+1])<<8)
	}
	return samples, nil
}

// WritePCM queues uplink audio. A full queue drops the oldest period so the
// call stays live instead of accumulating unbounded latency.
func (bridge *Bridge) WritePCM(samples []int16) error {
	if len(samples) == 0 {
		return nil
	}
	select {
	case <-bridge.closed:
		return ErrClosed
	case <-bridge.writeFailed:
		return ErrClosed
	default:
	}
	copied := make([]int16, len(samples))
	copy(copied, samples)
	for {
		select {
		case bridge.writeQueue <- copied:
			return nil
		case <-bridge.closed:
			return ErrClosed
		default:
			select {
			case <-bridge.writeQueue:
			default:
			}
		}
	}
}

func (bridge *Bridge) killProcesses() {
	if bridge.capture != nil && bridge.capture.Process != nil {
		_ = bridge.capture.Process.Kill()
	}
	if bridge.playback != nil && bridge.playback.Process != nil {
		_ = bridge.playback.Process.Kill()
	}
}

// Close stops both ALSA streams and releases the card.
func (bridge *Bridge) Close() error {
	bridge.closeOnce.Do(func() {
		close(bridge.closed)
		if bridge.writer != nil {
			_ = bridge.writer.Close()
		}
		bridge.killProcesses()
		// Processes are waited on by their supervisor goroutines; bound the
		// shutdown so a wedged ALSA client cannot block an HTTP handler.
		select {
		case <-bridge.captureDone:
		case <-time.After(time.Second):
		}
		select {
		case <-bridge.playbackDone:
		case <-time.After(time.Second):
		}
	})
	return nil
}
