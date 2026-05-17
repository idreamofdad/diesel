package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

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

// synthesizeTTS POSTs `text` to `<endpoint>/audio/speech` and returns the
// raw audio bytes the server produced. The OpenAI Audio API contract is
// what we target — Speaches, KokoroTTS-shim, and OpenAI itself all serve
// the same shape, so we don't need a per-provider switch.
//
// We explicitly ask for WAV. QMediaPlayer's mp3 path via SetSourceDevice
// is flaky on macOS (AVFoundation wants a URL/extension to sniff format)
// and the resulting "silent playback" is the kind of bug that's hard to
// tell from a misconfiguration. With WAV we parse the header ourselves
// and feed raw PCM to QAudioSink — predictable end-to-end.
func synthesizeTTS(ctx context.Context, endpoint, apiKey, model, voice, text string) ([]byte, error) {
	if model = strings.TrimSpace(model); model == "" {
		model = ttsDefaultModel
	}
	if voice = strings.TrimSpace(voice); voice == "" {
		voice = ttsDefaultVoice
	}
	ctx, span := startSpan(ctx, "tts.synthesize",
		attribute.String("tts.model", model),
		attribute.String("tts.voice", voice),
		attribute.Int("tts.text.length", len(text)),
	)
	defer span.End()

	endpoint = normalizeEndpoint(endpoint)
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
	client := tracedHTTPClient(60 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		err := httpStatusError(resp, 512)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	audio, err := io.ReadAll(resp.Body)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetAttributes(attribute.Int("tts.audio.bytes", len(audio)))
	return audio, nil
}

// wavInfo is what we pull out of a WAV header to drive QAudioSink:
// enough to construct a QAudioFormat plus a slice pointing at the raw
// PCM payload (which we hand to QBuffer untouched).
type wavInfo struct {
	sampleRate    int
	channels      int
	bitsPerSample int
	pcm           []byte
}

// parseWAV walks the RIFF chunks until it finds fmt and data. Tolerant
// of optional chunks (LIST, INFO, JUNK, …) that some encoders insert
// between fmt and data — those are skipped over by their declared
// size. Only PCM (format tag 1) is supported, which is what Speaches
// and OpenAI's TTS both emit when response_format is "wav".
func parseWAV(b []byte) (wavInfo, error) {
	var w wavInfo
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return w, errors.New("not a WAV file")
	}
	pos := 12
	foundFmt := false
	for pos+8 <= len(b) {
		tag := string(b[pos : pos+4])
		size := int(binary.LittleEndian.Uint32(b[pos+4 : pos+8]))
		pos += 8
		if size < 0 || pos+size > len(b) {
			return w, errors.New("truncated WAV chunk")
		}
		chunk := b[pos : pos+size]
		switch tag {
		case "fmt ":
			if size < 16 {
				return w, errors.New("short fmt chunk")
			}
			if formatTag := binary.LittleEndian.Uint16(chunk[0:2]); formatTag != 1 {
				return w, fmt.Errorf("unsupported WAV format tag %d (need PCM)", formatTag)
			}
			w.channels = int(binary.LittleEndian.Uint16(chunk[2:4]))
			w.sampleRate = int(binary.LittleEndian.Uint32(chunk[4:8]))
			w.bitsPerSample = int(binary.LittleEndian.Uint16(chunk[14:16]))
			foundFmt = true
		case "data":
			if !foundFmt {
				return w, errors.New("WAV data before fmt")
			}
			w.pcm = chunk
			return w, nil
		}
		// RIFF chunks are padded to even length.
		pos += size
		if size%2 == 1 && pos < len(b) {
			pos++
		}
	}
	return w, errors.New("no data chunk in WAV")
}

// sampleFormatFor returns the QAudioFormat sample-format for a given
// WAV bit depth. WAV PCM can be 8-bit unsigned or N-bit signed for
// N ≥ 16 — Qt's sample-format enum mirrors that asymmetry.
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

// speaker owns a single in-flight playback. The Go GC must not collect
// any of these fields while the sink is still draining the buffer —
// QAudioSink reads from buffer.QIODevice asynchronously, so all three
// have to stay alive until cleanup() runs.
type speaker struct {
	sink   *mm.QAudioSink
	buffer *qt.QBuffer
	format *mm.QAudioFormat
	done   bool
	// onDone, if set, fires exactly once when playback ends on its own
	// (the buffer drained) — *not* when stop() cancels it early.
	// Continuous-conversation mode hangs the next recording off this so
	// the mic only reopens after Diesel has actually finished speaking.
	onDone func()
	// naturalEnd records whether we passed through IdleState (buffer
	// exhausted) before stopping. An explicit stop() jumps straight to
	// StoppedState, so this stays false and onDone is suppressed.
	naturalEnd bool
	// span tracks the playback lifetime — started in playAudio after the
	// WAV decode succeeds, ended in cleanup() with the natural_end flag.
	span      trace.Span
	startedAt time.Time
}

// playAudio decodes a WAV blob and streams its PCM through QAudioSink
// using a QBuffer as the pull-mode source device. Format selection
// comes from the WAV header rather than being hardcoded: Speaches
// emits Kokoro at 24 kHz, OpenAI's tts-1 at 24 kHz, Piper varies by
// voice — we just play what the server gave us.
//
// Returns a speaker whose stop() cancels playback early; otherwise the
// sink tears itself down when the buffer reaches IdleState (data
// exhausted).
func playAudio(ctx context.Context, audio []byte) (*speaker, error) {
	_, span := startSpan(ctx, "tts.play",
		attribute.Int("tts.audio.bytes", len(audio)),
	)
	if len(audio) == 0 {
		err := errors.New("no audio to play")
		span.SetStatus(codes.Error, err.Error())
		span.End()
		return nil, err
	}
	info, err := parseWAV(audio)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.End()
		return nil, fmt.Errorf("decode WAV: %w", err)
	}
	if info.sampleRate <= 0 || info.channels <= 0 || len(info.pcm) == 0 {
		err := errors.New("WAV header missing required fields")
		span.SetStatus(codes.Error, err.Error())
		span.End()
		return nil, err
	}
	span.SetAttributes(
		attribute.Int("tts.play.sample_rate", info.sampleRate),
		attribute.Int("tts.play.channels", info.channels),
		attribute.Int("tts.play.bits_per_sample", info.bitsPerSample),
		attribute.Int("tts.play.pcm_bytes", len(info.pcm)),
	)
	sampleFmt, err := sampleFormatFor(info.bitsPerSample)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.End()
		return nil, err
	}

	format := mm.NewQAudioFormat()
	format.SetSampleRate(info.sampleRate)
	format.SetChannelCount(info.channels)
	format.SetSampleFormat(sampleFmt)

	dev := pickOutputDevice(loadSettings().OutputDevice)
	if dev == nil {
		err := errors.New("no audio output device available")
		span.SetStatus(codes.Error, err.Error())
		span.End()
		return nil, err
	}

	buf := qt.NewQBuffer()
	buf.SetData(info.pcm)
	if !buf.Open(qt.QIODeviceBase__ReadOnly) {
		err := errors.New("could not open audio buffer")
		span.SetStatus(codes.Error, err.Error())
		span.End()
		return nil, err
	}

	sink := mm.NewQAudioSink5(dev, format)
	s := &speaker{sink: sink, buffer: buf, format: format, span: span, startedAt: time.Now()}
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

// stop halts playback immediately. Safe to call from anywhere on the Qt
// main thread, including before play has actually started or after it
// already finished — cleanup is idempotent and runs from the state
// change the Stop() call triggers.
func (s *speaker) stop() {
	if s == nil || s.done {
		return
	}
	if s.sink != nil {
		s.sink.Stop() // → StoppedState → cleanup() via the signal
	}
}

// cleanup runs once the sink reaches Idle or Stopped. Closes the buffer
// (which signals "no more data" to Qt) and marks the speaker done so a
// follow-up stop() is a no-op.
func (s *speaker) cleanup() {
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
	// stop() (user reached for the mic, or a newer reply replaced this
	// one) means the continuous loop should not fire here.
	if s.naturalEnd && s.onDone != nil {
		s.onDone()
	}
}
