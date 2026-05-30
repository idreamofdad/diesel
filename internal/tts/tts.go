package tts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unsafe"

	"diesel/internal/audio"
	"diesel/internal/settings"
	"diesel/internal/tracing"
	"diesel/internal/util"
	"diesel/internal/wav"

	"github.com/gen2brain/malgo"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Playback device buffering. The default low-latency profile uses ~10 ms
// periods, which means ~100 cgo callbacks/sec — an occasional Go GC or
// scheduler pause then misses a period deadline and the listener hears a
// gap (stutter). Speech playback doesn't care about latency, so we trade it
// for a comfortable 100 ms deadline per callback and a 300 ms total buffer,
// far beyond any realistic Go pause.
const (
	playbackPeriodMS = 100
	playbackPeriods  = 3
)

// drainTailDelay is how long playback keeps the device open, emitting
// silence, after the last real PCM sample has been handed to miniaudio. The
// data callback runs ahead of the speaker, so the final samples are still
// buffered in the backend when our PCM runs out; closing the device
// immediately would clip the tail. It must exceed the device buffer
// (playbackPeriodMS * playbackPeriods = 300 ms), with margin to spare.
const drainTailDelay = 400 * time.Millisecond

// ttsDefaultModel + ttsDefaultVoice mirror OpenAI's TTS defaults so a
// blank-but-enabled config still produces audible output against OpenAI
// itself. Speaches users will override both via the settings dialog.
const (
	ttsDefaultModel = "tts-1"
	ttsDefaultVoice = "alloy"
)

// Synthesize POSTs `text` to `<endpoint>/audio/speech` and returns the
// raw audio bytes the server produced. The OpenAI Audio API contract is
// what we target — Speaches, KokoroTTS-shim, and OpenAI itself all serve
// the same shape, so we don't need a per-provider switch.
//
// We explicitly ask for WAV. QMediaPlayer's mp3 path via SetSourceDevice
// is flaky on macOS (AVFoundation wants a URL/extension to sniff format)
// and the resulting "silent playback" is the kind of bug that's hard to
// tell from a misconfiguration. With WAV we parse the header ourselves
// and feed raw PCM to QAudioSink — predictable end-to-end.
func Synthesize(ctx context.Context, endpoint, apiKey, model, voice, text string) ([]byte, error) {
	if model = strings.TrimSpace(model); model == "" {
		model = ttsDefaultModel
	}
	if voice = strings.TrimSpace(voice); voice == "" {
		voice = ttsDefaultVoice
	}
	ctx, span := tracing.StartSpan(ctx, "tts.synthesize",
		attribute.String("tts.model", model),
		attribute.String("tts.voice", voice),
		attribute.Int("tts.text.length", len(text)),
	)
	defer span.End()

	endpoint = util.NormalizeEndpoint(endpoint)
	if endpoint == "" {
		err := errors.New("no TTS endpoint configured")
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	if strings.TrimSpace(text) == "" {
		err := errors.New("nothing to speak")
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	body, err := json.Marshal(map[string]any{
		"model":           model,
		"input":           text,
		"voice":           voice,
		"response_format": "wav",
	})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint+"/audio/speech", bytes.NewReader(body))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key := strings.TrimSpace(apiKey); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	// 60 s is plenty for a 1–2 sentence reply on a local server; longer
	// completions are rare in this UI because the system prompt caps
	// Diesel at 3 sentences.
	client := tracing.HTTPClient(60 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		err := util.HTTPStatusError(resp, 512)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	audioBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetAttributes(attribute.Int("tts.audio.bytes", len(audioBytes)))
	return audioBytes, nil
}

// sampleFormatFor returns the miniaudio sample-format for a given WAV bit
// depth. WAV PCM is 8-bit unsigned or N-bit signed for N ≥ 16 —
// miniaudio's format enum mirrors that asymmetry.
func sampleFormatFor(bits int) (malgo.FormatType, error) {
	switch bits {
	case 8:
		return malgo.FormatU8, nil
	case 16:
		return malgo.FormatS16, nil
	case 32:
		return malgo.FormatS32, nil
	}
	return malgo.FormatUnknown, fmt.Errorf("unsupported bit depth: %d", bits)
}

// Speaker owns a single in-flight playback. The data callback streams pcm
// out to the device on miniaudio's audio thread; mu guards the cursor and
// teardown flags against the user's Stop arriving on another thread.
type Speaker struct {
	device *malgo.Device

	mu  sync.Mutex
	pcm []byte // raw PCM, format described by the device config
	pos int    // bytes already handed to the device

	// OnDone, if set, fires exactly once when playback ends on its own
	// (the buffer drained) — *not* when Stop cancels it early.
	// Continuous-conversation mode hangs the next recording off this so
	// the mic only reopens after Diesel has actually finished speaking.
	OnDone func()
	// naturalEnd records whether playback drained on its own. An explicit
	// Stop leaves it false, suppressing OnDone.
	naturalEnd bool
	// draining marks that the PCM has been fully emitted and the tail
	// timer is running, so the callback only schedules teardown once.
	draining bool
	// finished guards finish() so the device is uninitialized and OnDone
	// fires exactly once across the Stop and drain paths.
	finished bool

	// span tracks the playback lifetime — started in Play after the WAV
	// decode succeeds, ended in finish() with the natural_end flag.
	span      trace.Span
	startedAt time.Time
}

// Play decodes a WAV blob and streams its PCM to a miniaudio playback
// device, pulling samples from an in-memory cursor in the data callback.
// Format selection comes from the WAV header rather than being hardcoded:
// Speaches emits Kokoro at 24 kHz, OpenAI's tts-1 at 24 kHz, Piper varies
// by voice — we just play what the server gave us.
//
// Returns a Speaker whose Stop cancels playback early; otherwise the
// device tears itself down a short tail after the PCM is exhausted.
func Play(ctx context.Context, audioBytes []byte) (*Speaker, error) {
	_, span := tracing.StartSpan(ctx, "tts.play",
		attribute.Int("tts.audio.bytes", len(audioBytes)),
	)
	fail := func(err error) (*Speaker, error) {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.End()
		return nil, err
	}
	if len(audioBytes) == 0 {
		return fail(errors.New("no audio to play"))
	}
	info, err := wav.Parse(audioBytes)
	if err != nil {
		return fail(fmt.Errorf("decode WAV: %w", err))
	}
	if info.SampleRate <= 0 || info.Channels <= 0 || len(info.PCM) == 0 {
		return fail(errors.New("WAV header missing required fields"))
	}
	span.SetAttributes(
		attribute.Int("tts.play.sample_rate", info.SampleRate),
		attribute.Int("tts.play.channels", info.Channels),
		attribute.Int("tts.play.bits_per_sample", info.BitsPerSample),
		attribute.Int("tts.play.pcm_bytes", len(info.PCM)),
	)
	sampleFmt, err := sampleFormatFor(info.BitsPerSample)
	if err != nil {
		return fail(err)
	}

	mctx, err := audio.Context()
	if err != nil {
		return fail(fmt.Errorf("audio backend: %w", err))
	}

	cfg := malgo.DefaultDeviceConfig(malgo.Playback)
	cfg.Playback.Format = sampleFmt
	cfg.Playback.Channels = uint32(info.Channels)
	cfg.SampleRate = uint32(info.SampleRate)
	// Buffer generously so a Go-side pause can't starve the device mid-
	// utterance — see playbackPeriodMS. Latency is irrelevant here.
	cfg.PeriodSizeInMilliseconds = playbackPeriodMS
	cfg.Periods = playbackPeriods
	cfg.PerformanceProfile = malgo.Conservative
	// devID is read synchronously by ma_device_init below; nil = default.
	devID, err := audio.PickOutputDevice(settings.Load().OutputDevice)
	if err != nil {
		return fail(fmt.Errorf("resolve output device: %w", err))
	}
	if devID != nil {
		cfg.Playback.DeviceID = unsafe.Pointer(devID)
	}

	// info.PCM aliases the input buffer (see wav.Info); copy it so the
	// Speaker can outlive audioBytes while the callback streams from it.
	s := &Speaker{
		pcm:       append([]byte(nil), info.PCM...),
		span:      span,
		startedAt: time.Now(),
	}
	dev, err := malgo.InitDevice(mctx, cfg, malgo.DeviceCallbacks{Data: s.read})
	if err != nil {
		return fail(fmt.Errorf("open audio output: %w", err))
	}
	s.device = dev
	if err := dev.Start(); err != nil {
		dev.Uninit()
		return fail(fmt.Errorf("start audio output: %w", err))
	}
	return s, nil
}

// read fills the device's output buffer from the PCM cursor, zero-filling
// any remainder once the data runs out and arming the tail timer the first
// time that happens. Runs on miniaudio's audio thread.
func (s *Speaker) read(out, _ []byte, _ uint32) {
	s.mu.Lock()
	n := copy(out, s.pcm[s.pos:])
	s.pos += n
	if n < len(out) {
		for i := n; i < len(out); i++ {
			out[i] = 0
		}
		if s.pos >= len(s.pcm) && !s.draining && !s.finished {
			s.draining = true
			go s.drainThenFinish()
		}
	}
	s.mu.Unlock()
}

// drainThenFinish waits out the backend's tail buffer after the PCM is
// exhausted, then finishes as a natural end (firing OnDone).
func (s *Speaker) drainThenFinish() {
	time.Sleep(drainTailDelay)
	s.finish(true)
}

// Stop halts playback immediately. Safe to call from any goroutine,
// including before play has started or after it already finished — finish
// is idempotent and suppresses OnDone on this (non-natural) path.
func (s *Speaker) Stop() {
	if s == nil {
		return
	}
	s.finish(false)
}

// finish uninitializes the device and, on a natural end, fires OnDone —
// exactly once. ma_device_uninit must not run on the audio thread (it
// blocks on the callback returning), so finish is only ever called from
// the drain goroutine or an external Stop, never from read.
func (s *Speaker) finish(natural bool) {
	s.mu.Lock()
	if s.finished {
		s.mu.Unlock()
		return
	}
	s.finished = true
	if natural {
		s.naturalEnd = true
	}
	fireOnDone := s.naturalEnd && s.OnDone != nil
	onDone := s.OnDone
	s.mu.Unlock()

	if s.device != nil {
		s.device.Uninit()
	}
	if s.span != nil {
		s.span.SetAttributes(
			attribute.Bool("tts.play.natural_end", s.naturalEnd),
			attribute.Int64("tts.play.duration_ms", time.Since(s.startedAt).Milliseconds()),
		)
		s.span.End()
	}
	if fireOnDone {
		onDone()
	}
}
