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
	"time"

	"diesel/internal/audio"
	"diesel/internal/settings"
	"diesel/internal/tracing"
	"diesel/internal/util"
	"diesel/internal/wav"

	qt "github.com/mappu/miqt/qt6"
	mm "github.com/mappu/miqt/qt6/multimedia"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

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
	defer resp.Body.Close()
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

// sampleFormatFor returns the QAudioFormat sample-format for a given WAV
// bit depth. WAV PCM can be 8-bit unsigned or N-bit signed for N ≥ 16 —
// Qt's sample-format enum mirrors that asymmetry.
func sampleFormatFor(bits int) (mm.QAudioFormat__SampleFormat, error) {
	switch bits {
	case 8:
		return mm.QAudioFormat__UInt8, nil
	case 16:
		return mm.QAudioFormat__Int16, nil
	case 32:
		return mm.QAudioFormat__Int32, nil
	}
	return mm.QAudioFormat__Unknown, fmt.Errorf("unsupported bit depth: %d", bits)
}

// Speaker owns a single in-flight playback. The Go GC must not collect
// any of these fields while the sink is still draining the buffer —
// QAudioSink reads from buffer.QIODevice asynchronously, so all three
// have to stay alive until cleanup() runs.
type Speaker struct {
	sink   *mm.QAudioSink
	buffer *qt.QBuffer
	format *mm.QAudioFormat
	done   bool
	// OnDone, if set, fires exactly once when playback ends on its own
	// (the buffer drained) — *not* when Stop cancels it early.
	// Continuous-conversation mode hangs the next recording off this so
	// the mic only reopens after Diesel has actually finished speaking.
	OnDone func()
	// naturalEnd records whether we passed through IdleState (buffer
	// exhausted) before stopping. An explicit Stop jumps straight to
	// StoppedState, so this stays false and OnDone is suppressed.
	naturalEnd bool
	// span tracks the playback lifetime — started in Play after the
	// WAV decode succeeds, ended in cleanup() with the natural_end flag.
	span      trace.Span
	startedAt time.Time
}

// Play decodes a WAV blob and streams its PCM through QAudioSink using a
// QBuffer as the pull-mode source device. Format selection comes from
// the WAV header rather than being hardcoded: Speaches emits Kokoro at
// 24 kHz, OpenAI's tts-1 at 24 kHz, Piper varies by voice — we just play
// what the server gave us.
//
// Returns a Speaker whose Stop cancels playback early; otherwise the
// sink tears itself down when the buffer reaches IdleState (data
// exhausted).
func Play(ctx context.Context, audioBytes []byte) (*Speaker, error) {
	_, span := tracing.StartSpan(ctx, "tts.play",
		attribute.Int("tts.audio.bytes", len(audioBytes)),
	)
	if len(audioBytes) == 0 {
		err := errors.New("no audio to play")
		span.SetStatus(codes.Error, err.Error())
		span.End()
		return nil, err
	}
	info, err := wav.Parse(audioBytes)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.End()
		return nil, fmt.Errorf("decode WAV: %w", err)
	}
	if info.SampleRate <= 0 || info.Channels <= 0 || len(info.PCM) == 0 {
		err := errors.New("WAV header missing required fields")
		span.SetStatus(codes.Error, err.Error())
		span.End()
		return nil, err
	}
	span.SetAttributes(
		attribute.Int("tts.play.sample_rate", info.SampleRate),
		attribute.Int("tts.play.channels", info.Channels),
		attribute.Int("tts.play.bits_per_sample", info.BitsPerSample),
		attribute.Int("tts.play.pcm_bytes", len(info.PCM)),
	)
	sampleFmt, err := sampleFormatFor(info.BitsPerSample)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.End()
		return nil, err
	}

	format := mm.NewQAudioFormat()
	format.SetSampleRate(info.SampleRate)
	format.SetChannelCount(info.Channels)
	format.SetSampleFormat(sampleFmt)

	dev := audio.PickOutputDevice(settings.Load().OutputDevice)
	if dev == nil {
		err := errors.New("no audio output device available")
		span.SetStatus(codes.Error, err.Error())
		span.End()
		return nil, err
	}

	buf := qt.NewQBuffer()
	buf.SetData(info.PCM)
	if !buf.Open(qt.QIODeviceBase__ReadOnly) {
		err := errors.New("could not open audio buffer")
		span.SetStatus(codes.Error, err.Error())
		span.End()
		return nil, err
	}

	sink := mm.NewQAudioSink5(dev, format)
	s := &Speaker{sink: sink, buffer: buf, format: format, span: span, startedAt: time.Now()}
	// QAudioSink's state machine has a subtlety: when the source device
	// returns 0 bytes (buffer drained) the sink transitions to
	// IdleState but stays "active" and keeps polling for more data. If
	// we close the buffer here Qt logs "QIODevice::read (QBuffer):
	// device not open" on every pull cycle until the sink is GC'd.
	// The fix is to call Stop() on Idle so the sink transitions to
	// StoppedState — only then is it safe to close the buffer.
	sink.OnStateChanged(func(state mm.QAudio__State) {
		switch state {
		case mm.QAudio__IdleState:
			s.naturalEnd = true
			s.sink.Stop() // → StoppedState
		case mm.QAudio__StoppedState:
			s.cleanup()
		}
	})
	sink.Start(buf.QIODevice)
	return s, nil
}

// Stop halts playback immediately. Safe to call from anywhere on the Qt
// main thread, including before play has actually started or after it
// already finished — cleanup is idempotent and runs from the state
// change the Stop() call triggers.
func (s *Speaker) Stop() {
	if s == nil || s.done {
		return
	}
	if s.sink != nil {
		s.sink.Stop() // → StoppedState → cleanup() via the signal
	}
}

// cleanup runs once the sink reaches Idle or Stopped. Closes the buffer
// (which signals "no more data" to Qt) and marks the speaker done so a
// follow-up Stop is a no-op.
func (s *Speaker) cleanup() {
	if s.done {
		return
	}
	s.done = true
	if s.buffer != nil {
		s.buffer.Close()
	}
	if s.span != nil {
		s.span.SetAttributes(
			attribute.Bool("tts.play.natural_end", s.naturalEnd),
			attribute.Int64("tts.play.duration_ms", time.Since(s.startedAt).Milliseconds()),
		)
		s.span.End()
	}
	// Only chain onward when playback drained on its own — an explicit
	// Stop (user reached for the mic, or a newer reply replaced this
	// one) means the continuous loop should not fire here.
	if s.naturalEnd && s.OnDone != nil {
		s.OnDone()
	}
}
