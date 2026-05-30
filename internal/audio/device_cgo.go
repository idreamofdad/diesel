//go:build cgo

// This file is the cgo half of package audio: native microphone capture via
// miniaudio (malgo) plus the energy-based VAD. It is excluded from
// CGO_ENABLED=0 builds (the headless daemon), which only need the pure-Go
// transcription path in audio.go. Only the desktop app and the voicecheck
// harness reference these symbols, and both are themselves //go:build cgo.

package audio

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
	"unsafe"

	"diesel/internal/settings"
	"diesel/internal/tracing"

	"github.com/gen2brain/malgo"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// VAD tuning. The thresholds are RMS amplitude over a 30 ms frame of int16
// samples — heavily mic-dependent, so these are reasonable starting values
// rather than absolutes. Quiet rooms tend to floor around 30–200; normal
// speech sits comfortably above 500. vadFrameSamples/vadFrameBytes derive
// from the STT format constants in audio.go.
const (
	vadFrameMS       = 30
	vadFrameSamples  = sttSampleRate * vadFrameMS / 1000
	vadFrameBytes    = vadFrameSamples * sttBytesPerSamp
	vadStartRMS      = 450  // sustained level required to declare "speech started"
	vadSilenceMS     = 1100 // trailing silence after speech before we stop
	vadMinSpeechMS   = 250  // ignore blips shorter than this
	vadMaxDurationMS = 30000
)

// StopReason classifies why a recording ended, so the caller can decide
// whether to send the audio onward (VAD/max-length), warn the user
// (no-speech), or quietly drop it (cancelled).
type StopReason int

const (
	StopVAD       StopReason = iota // VAD detected trailing silence
	StopMaxLength                   // hard duration ceiling hit
	StopCancelled                   // user pressed record again — discard audio
	StopCommitted                   // user pressed the commit/send button — transcribe
	StopNoSpeech                    // we never crossed the start threshold
)

// String returns a stable, human-readable label for the stop reason. Used as
// the `stt.record.reason` span attribute so trace consumers can filter by
// outcome without having to memorize the int constants.
func (r StopReason) String() string {
	switch r {
	case StopVAD:
		return "vad"
	case StopMaxLength:
		return "max_length"
	case StopCancelled:
		return "cancelled"
	case StopCommitted:
		return "committed"
	case StopNoSpeech:
		return "no_speech"
	}
	return "unknown"
}

// sharedCtx is the process-wide miniaudio context. miniaudio wants a
// single context that owns the backend connection (CoreAudio on macOS,
// WASAPI on Windows, …); capture, playback, and device enumeration all
// hang off it. Created lazily on first use and never freed — it lives for
// the lifetime of the process.
var (
	ctxOnce   sync.Once
	allocCtx  *malgo.AllocatedContext
	ctxInitEr error
)

// Context returns the shared miniaudio context, initializing it on first
// call. Exported so the tts package can open playback devices against the
// same backend connection without standing up a second context.
func Context() (malgo.Context, error) {
	ctxOnce.Do(func() {
		allocCtx, ctxInitEr = malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	})
	if ctxInitEr != nil {
		return malgo.Context{}, ctxInitEr
	}
	return allocCtx.Context, nil
}

// Recorder holds the state of an in-progress capture. It is created by
// StartRecording and lives until Stop fires its onStop callback exactly
// once.
//
// miniaudio delivers frames on its own audio thread while the user's Stop
// arrives on the GUI thread, so mu guards every mutable field against that
// two-thread access.
type Recorder struct {
	device *malgo.Device

	mu       sync.Mutex
	buf      bytes.Buffer // raw int16 LE PCM
	leftover []byte       // partial frame carried between callbacks

	startedAt       time.Time
	hasVoice        bool
	speechStartedAt time.Time
	lastVoiceAt     time.Time
	done            bool

	// span tracks the recording lifecycle for tracing — created in
	// StartRecording, ended in finish with the final reason / byte count
	// attached. Always non-nil; falls back to a no-op span when tracing
	// is disabled.
	span trace.Span

	// onStop fires once, with the captured PCM and the reason we stopped.
	// It runs on a goroutine (not the audio thread), so a GUI caller must
	// marshal any widget work onto the UI thread itself.
	onStop func(pcm []byte, reason StopReason)
}

// StartRecording opens the configured audio input at 16 kHz mono Int16 and
// streams samples through an energy-based VAD. Returns the recorder so the
// caller can manually stop it; onStop fires exactly once, on a goroutine.
func StartRecording(ctx context.Context, onStop func([]byte, StopReason)) (*Recorder, error) {
	_, span := tracing.StartSpan(ctx, "stt.record",
		attribute.Int("stt.sample_rate", sttSampleRate),
		attribute.Int("stt.channels", sttChannels),
	)
	fail := func(err error) (*Recorder, error) {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.End()
		return nil, err
	}

	mctx, err := Context()
	if err != nil {
		return fail(fmt.Errorf("audio backend: %w", err))
	}

	cfg := malgo.DefaultDeviceConfig(malgo.Capture)
	cfg.Capture.Format = malgo.FormatS16
	cfg.Capture.Channels = sttChannels
	cfg.SampleRate = sttSampleRate
	cfg.Alsa.NoMMap = 1
	// Buffer comfortably so a Go-side pause can't drop captured frames (which
	// would chop the audio the STT server sees). The 50 ms period stays well
	// under the VAD's 1100 ms silence window, so detection responsiveness is
	// unaffected.
	cfg.PeriodSizeInMilliseconds = 50
	cfg.Periods = 4
	cfg.PerformanceProfile = malgo.Conservative
	// devID is read by ma_device_init synchronously below, so this local
	// stays alive for as long as the C side needs it. nil = system default.
	devID, err := pickDevice(malgo.Capture, settings.Load().InputDevice)
	if err != nil {
		return fail(fmt.Errorf("resolve input device: %w", err))
	}
	if devID != nil {
		cfg.Capture.DeviceID = unsafe.Pointer(devID)
	}

	r := &Recorder{
		onStop:    onStop,
		startedAt: time.Now(),
		span:      span,
	}
	dev, err := malgo.InitDevice(mctx, cfg, malgo.DeviceCallbacks{
		Data: func(_, in []byte, _ uint32) { r.feed(in) },
	})
	if err != nil {
		return fail(fmt.Errorf("open audio input: %w", err))
	}
	r.device = dev
	if err := dev.Start(); err != nil {
		dev.Uninit()
		return fail(fmt.Errorf("start audio input: %w", err))
	}
	return r, nil
}

// deviceNames lists the non-empty names of every device of the given kind,
// in the order the backend reports them. Drives the input/output combos in
// the settings dialog. Returns nil (not an error) when the context or
// enumeration fails — a missing device list shouldn't break the dialog.
func deviceNames(kind malgo.DeviceType) []string {
	mctx, err := Context()
	if err != nil {
		return nil
	}
	devs, err := mctx.Devices(kind)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(devs))
	for i := range devs {
		if name := devs[i].Name(); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// InputDescriptions lists the human-readable names of every capture device,
// in the order the backend reports them.
func InputDescriptions() []string { return deviceNames(malgo.Capture) }

// OutputDescriptions mirrors InputDescriptions for playback devices.
func OutputDescriptions() []string { return deviceNames(malgo.Playback) }

// pickDevice maps the user's saved device name to a concrete miniaudio
// device ID, returning nil (use the platform default) for a blank/"System
// Default"/unknown selection. Matching on Name() because that's what the
// settings combo stores. The returned pointer addresses a fresh copy, so
// the caller may pass it straight to a DeviceConfig.
func pickDevice(kind malgo.DeviceType, saved string) (*malgo.DeviceID, error) {
	saved = strings.TrimSpace(saved)
	if saved == "" || saved == "System Default" {
		return nil, nil
	}
	mctx, err := Context()
	if err != nil {
		return nil, err
	}
	devs, err := mctx.Devices(kind)
	if err != nil {
		return nil, err
	}
	for i := range devs {
		if devs[i].Name() == saved {
			id := devs[i].ID
			return &id, nil
		}
	}
	return nil, nil
}

// PickOutputDevice resolves the saved output-device name to a miniaudio
// device ID for playback, returning nil for the platform default. Exported
// for the tts package, which opens its own playback devices.
func PickOutputDevice(saved string) (*malgo.DeviceID, error) {
	return pickDevice(malgo.Playback, saved)
}

// feed appends a callback's worth of capture data to the PCM buffer and
// runs the VAD state machine over it. Partial frames are carried in
// leftover so every RMS computation is over a clean 30 ms window. Runs on
// miniaudio's audio thread; it triggers Stop (off-thread teardown) once the
// VAD or the duration ceiling fires.
func (r *Recorder) feed(data []byte) {
	r.mu.Lock()
	if r.done {
		r.mu.Unlock()
		return
	}
	r.buf.Write(data)

	pool := append(r.leftover, data...)
	consumed := 0
	now := time.Now()
	for consumed+vadFrameBytes <= len(pool) {
		frame := pool[consumed : consumed+vadFrameBytes]
		consumed += vadFrameBytes
		if frameRMS(frame) >= vadStartRMS {
			if !r.hasVoice {
				r.speechStartedAt = now
			}
			r.hasVoice = true
			r.lastVoiceAt = now
		}
	}
	r.leftover = append(r.leftover[:0], pool[consumed:]...)

	var reason StopReason
	stop := false
	switch {
	case now.Sub(r.startedAt) > vadMaxDurationMS*time.Millisecond:
		reason, stop = StopMaxLength, true
	case r.hasVoice &&
		now.Sub(r.speechStartedAt) > vadMinSpeechMS*time.Millisecond &&
		now.Sub(r.lastVoiceAt) > vadSilenceMS*time.Millisecond:
		reason, stop = StopVAD, true
	}
	r.mu.Unlock()

	if stop {
		r.Stop(reason)
	}
}

// Stop ends the recording and fires onStop exactly once. Safe to call from
// the VAD path (audio thread) or a user cancel (GUI thread); the actual
// device teardown and callback run on a separate goroutine because
// ma_device_uninit must not be called from within the data callback (it
// blocks waiting for that callback to return — an instant deadlock).
func (r *Recorder) Stop(reason StopReason) {
	r.mu.Lock()
	if r.done {
		r.mu.Unlock()
		return
	}
	r.done = true
	r.mu.Unlock()
	go r.finish(reason)
}

// finish tears down the capture device and delivers the result. Runs once,
// off both the audio and (for a user-initiated stop) GUI threads.
func (r *Recorder) finish(reason StopReason) {
	if r.device != nil {
		r.device.Uninit()
	}
	r.mu.Lock()
	if !r.hasVoice && reason != StopCancelled {
		reason = StopNoSpeech
	}
	pcm := append([]byte(nil), r.buf.Bytes()...)
	hadVoice := r.hasVoice
	started := r.startedAt
	r.mu.Unlock()

	if r.span != nil {
		r.span.SetAttributes(
			attribute.String("stt.record.reason", reason.String()),
			attribute.Int("stt.record.bytes", len(pcm)),
			attribute.Bool("stt.record.had_voice", hadVoice),
			attribute.Int64("stt.record.duration_ms", time.Since(started).Milliseconds()),
		)
		r.span.End()
	}
	if r.onStop != nil {
		r.onStop(pcm, reason)
	}
}

// frameRMS returns the root-mean-square amplitude of a frame of LE int16
// samples — a reasonable proxy for loudness over the 30 ms window.
func frameRMS(frame []byte) float64 {
	n := len(frame) / 2
	if n == 0 {
		return 0
	}
	var sumSquares float64
	for i := 0; i < n; i++ {
		v := float64(int16(binary.LittleEndian.Uint16(frame[i*2:])))
		sumSquares += v * v
	}
	return math.Sqrt(sumSquares / float64(n))
}
