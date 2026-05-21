package audio

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

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

// Audio capture parameters tuned for Whisper-family STT models: 16 kHz
// mono PCM is what Whisper runs at internally, so we sidestep an extra
// resample step on the server side and keep the upload tiny (~32 kB/s).
const (
	sttSampleRate    = 16000
	sttBitsPerSample = 16
	sttBytesPerSamp  = sttBitsPerSample / 8 // signed int16, little-endian
	sttChannels      = 1
	sttDefaultModel  = "whisper-1" // OpenAI-compatible default
)

// VAD tuning. The thresholds are RMS amplitude over a 30 ms frame of
// int16 samples — heavily mic-dependent, so these are reasonable starting
// values rather than absolutes. Quiet rooms tend to floor around 30–200;
// normal speech sits comfortably above 500.
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

// Recorder holds the state of an in-progress capture. It is created on
// the main Qt thread by StartRecording and lives until Stop fires its
// onStop callback exactly once.
//
// All methods must be called from the Qt main thread — the OnReadyRead
// signal fires there, and we read mutable state without a lock.
type Recorder struct {
	source   *mm.QAudioSource
	io       *qt.QIODevice
	buf      bytes.Buffer // raw int16 LE PCM
	leftover []byte       // partial frame carried between consume() calls

	startedAt       time.Time
	hasVoice        bool
	speechStartedAt time.Time
	lastVoiceAt     time.Time
	done            bool

	// span tracks the recording lifecycle for tracing — created in
	// StartRecording, ended in Stop with the final reason / byte count
	// attached. Always non-nil; falls back to a no-op span when tracing
	// is disabled.
	span trace.Span

	// onStop fires once, with the captured PCM and the reason we stopped.
	onStop func(pcm []byte, reason StopReason)
}

// StartRecording opens the default audio input at 16 kHz mono Int16 and
// streams samples through an energy-based VAD. Returns the recorder so
// the caller can manually stop it; onStop fires on the Qt main thread.
func StartRecording(ctx context.Context, onStop func([]byte, StopReason)) (*Recorder, error) {
	_, span := tracing.StartSpan(ctx, "stt.record",
		attribute.Int("stt.sample_rate", sttSampleRate),
		attribute.Int("stt.channels", sttChannels),
	)

	format := mm.NewQAudioFormat()
	format.SetSampleRate(sttSampleRate)
	format.SetChannelCount(sttChannels)
	format.SetSampleFormat(mm.QAudioFormat__Int16)

	dev := PickInputDevice(settings.Load().InputDevice)
	if dev == nil {
		err := errors.New("no audio input device available")
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.End()
		return nil, err
	}

	src := mm.NewQAudioSource5(dev, format)
	iodev := src.Start2()
	if iodev == nil {
		err := errors.New("could not open audio input")
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.End()
		return nil, err
	}

	r := &Recorder{
		source:    src,
		io:        iodev,
		onStop:    onStop,
		startedAt: time.Now(),
		span:      span,
	}
	iodev.OnReadyRead(r.consume)
	return r, nil
}

// deviceDescriptions extracts the non-empty Description() of every
// QAudioDevice in `devs`, preserving order. Drives the input/output combos
// in the settings dialog.
func deviceDescriptions(devs []mm.QAudioDevice) []string {
	out := make([]string, 0, len(devs))
	for _, d := range devs {
		if name := d.Description(); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// InputDescriptions lists the human-readable names of every input device
// Qt sees, in the order the platform returns them.
func InputDescriptions() []string {
	return deviceDescriptions(mm.QMediaDevices_AudioInputs())
}

// OutputDescriptions mirrors InputDescriptions for outputs.
func OutputDescriptions() []string {
	return deviceDescriptions(mm.QMediaDevices_AudioOutputs())
}

// resolveAudioDevice maps the user's saved device description to a
// concrete QAudioDevice, falling back to the platform default. Matching
// on Description() rather than Id() because the user picks it from a
// combo of human-readable names.
func resolveAudioDevice(saved string, devs []mm.QAudioDevice, defaultDevice func() *mm.QAudioDevice) *mm.QAudioDevice {
	saved = strings.TrimSpace(saved)
	if saved == "" || saved == "System Default" {
		return defaultDevice()
	}
	for _, d := range devs {
		if d.Description() == saved {
			return &d
		}
	}
	return defaultDevice()
}

// PickInputDevice resolves the saved input-device description against the
// current list of inputs.
func PickInputDevice(saved string) *mm.QAudioDevice {
	return resolveAudioDevice(saved, mm.QMediaDevices_AudioInputs(), mm.QMediaDevices_DefaultAudioInput)
}

// PickOutputDevice mirrors PickInputDevice for playback devices.
func PickOutputDevice(saved string) *mm.QAudioDevice {
	return resolveAudioDevice(saved, mm.QMediaDevices_AudioOutputs(), mm.QMediaDevices_DefaultAudioOutput)
}

// consume drains whatever Qt has buffered, appends it to the PCM buffer,
// and runs the VAD state machine. Partial frames are carried over so
// every RMS computation is over a clean 30 ms window.
func (r *Recorder) consume() {
	if r.done {
		return
	}
	data := r.io.ReadAll()
	if len(data) == 0 {
		return
	}
	r.buf.Write(data)

	pool := append(r.leftover, data...)
	consumed := 0
	for consumed+vadFrameBytes <= len(pool) {
		frame := pool[consumed : consumed+vadFrameBytes]
		consumed += vadFrameBytes
		if frameRMS(frame) >= vadStartRMS {
			if !r.hasVoice {
				r.speechStartedAt = time.Now()
			}
			r.hasVoice = true
			r.lastVoiceAt = time.Now()
		}
	}
	r.leftover = append(r.leftover[:0], pool[consumed:]...)

	now := time.Now()
	switch {
	case now.Sub(r.startedAt) > vadMaxDurationMS*time.Millisecond:
		r.Stop(StopMaxLength)
	case r.hasVoice &&
		now.Sub(r.speechStartedAt) > vadMinSpeechMS*time.Millisecond &&
		now.Sub(r.lastVoiceAt) > vadSilenceMS*time.Millisecond:
		r.Stop(StopVAD)
	}
}

// Stop halts the audio source, drains any tail samples Qt buffered while
// we were processing, and fires onStop exactly once. Safe to call from
// either the VAD path (consume) or a user cancel.
func (r *Recorder) Stop(reason StopReason) {
	if r.done {
		return
	}
	r.done = true
	if r.source != nil {
		r.source.Stop()
	}
	if r.io != nil {
		if tail := r.io.ReadAll(); len(tail) > 0 {
			r.buf.Write(tail)
		}
	}
	if !r.hasVoice && reason != StopCancelled {
		reason = StopNoSpeech
	}
	if r.span != nil {
		r.span.SetAttributes(
			attribute.String("stt.record.reason", reason.String()),
			attribute.Int("stt.record.bytes", r.buf.Len()),
			attribute.Bool("stt.record.had_voice", r.hasVoice),
			attribute.Int64("stt.record.duration_ms", time.Since(r.startedAt).Milliseconds()),
		)
		r.span.End()
	}
	if r.onStop != nil {
		r.onStop(r.buf.Bytes(), reason)
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

// EncodeWAV wraps the recorder's PCM buffer in a WAV header so the STT
// server can decode it without the format being declared out of band.
// Format is fixed to whatever the recorder captured (16 kHz mono 16-bit).
func EncodeWAV(pcm []byte) []byte {
	return wav.Encode(pcm, sttSampleRate, sttChannels, sttBitsPerSample)
}

// Transcribe POSTs `wavData` to `<endpoint>/audio/transcriptions` as
// multipart/form-data, following the OpenAI Whisper API contract.
// Speaches, faster-whisper-server, and OpenAI itself implement the same
// shape, which is why we don't need a per-provider switch here.
//
// This is a thin convenience wrapper that always declares the upload as
// WAV — what the desktop's PortAudio→WAV pipeline produces. For audio
// in other codecs (browser MediaRecorder gives WebM/Opus or MP4/AAC)
// call TranscribeBlob and pass the original filename + content type.
func Transcribe(ctx context.Context, endpoint, apiKey, model string, wavData []byte) (string, error) {
	return TranscribeBlob(ctx, endpoint, apiKey, model, "audio.wav", "audio/wav", wavData)
}

// TranscribeBlob is the passthrough variant: it forwards `data` as-is
// to the STT server with whatever filename/content-type the caller
// provides. OpenAI-compatible STT servers (OpenAI, Speaches,
// faster-whisper-server, and LM Studio's whisper proxy) accept
// mp3/mp4/mpeg/mpga/m4a/wav/webm and sniff the format from the
// filename — passing the originating extension is what makes the
// browser path work without server-side transcoding.
func TranscribeBlob(ctx context.Context, endpoint, apiKey, model, filename, contentType string, data []byte) (string, error) {
	if model = strings.TrimSpace(model); model == "" {
		model = sttDefaultModel
	}
	if filename == "" {
		filename = "audio.wav"
	}
	ctx, span := tracing.StartSpan(ctx, "stt.transcribe",
		attribute.String("stt.model", model),
		attribute.Int("stt.audio.bytes", len(data)),
		attribute.String("stt.audio.filename", filename),
	)
	defer span.End()

	endpoint = util.NormalizeEndpoint(endpoint)
	if endpoint == "" {
		err := errors.New("no STT endpoint configured")
		span.SetStatus(codes.Error, err.Error())
		return "", err
	}

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	// Construct the form file part manually so we can stamp a real
	// Content-Type when the caller provided one (browser uploads do).
	// CreateFormFile would default to application/octet-stream, which
	// some stricter STT servers reject for non-WAV codecs.
	partHeader := make(map[string][]string)
	partHeader["Content-Disposition"] = []string{
		fmt.Sprintf(`form-data; name="file"; filename=%q`, filename),
	}
	if ct := strings.TrimSpace(contentType); ct != "" {
		partHeader["Content-Type"] = []string{ct}
	} else {
		partHeader["Content-Type"] = []string{"application/octet-stream"}
	}
	part, err := w.CreatePart(partHeader)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return "", err
	}
	if _, err := part.Write(data); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return "", err
	}
	_ = w.WriteField("model", model)
	_ = w.WriteField("response_format", "json")
	if err := w.Close(); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint+"/audio/transcriptions", &body)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return "", err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	if key := strings.TrimSpace(apiKey); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	// 2-minute ceiling: a 30 s recording on a slow local server (CPU-only
	// faster-whisper-large) can take well over a minute to transcribe.
	client := tracing.HTTPClient(2 * time.Minute)
	resp, err := client.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		err := util.HTTPStatusError(resp, 512)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return "", err
	}

	var payload struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return "", err
	}
	text := strings.TrimSpace(payload.Text)
	span.SetAttributes(attribute.Int("stt.text.length", len(text)))
	return text, nil
}
