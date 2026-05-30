// Package audio handles speech-to-text: encoding captured PCM as WAV and
// uploading it to an OpenAI-compatible transcription endpoint.
//
// This file is the pure-Go half — the HTTP/codec path that the server and
// bridges use to transcribe browser/remote audio. Native microphone
// capture and the energy-based VAD live in device_cgo.go behind a
// //go:build cgo tag, so a CGO_ENABLED=0 build (the headless daemon) gets
// transcription without pulling in miniaudio.
package audio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"diesel/internal/tracing"
	"diesel/internal/util"
	"diesel/internal/wav"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// Audio format parameters tuned for Whisper-family STT models: 16 kHz mono
// PCM is what Whisper runs at internally, so we sidestep an extra resample
// step on the server side and keep the upload tiny (~32 kB/s). The native
// capture path (device_cgo.go) records at these same values; EncodeWAV
// stamps them into the header.
const (
	sttSampleRate    = 16000
	sttBitsPerSample = 16
	sttBytesPerSamp  = sttBitsPerSample / 8 // signed int16, little-endian
	sttChannels      = 1
	sttDefaultModel  = "whisper-1" // OpenAI-compatible default
)

// EncodeWAV wraps a PCM buffer in a WAV header so the STT server can decode
// it without the format being declared out of band. Format is fixed to what
// the capture path produces (16 kHz mono 16-bit).
func EncodeWAV(pcm []byte) []byte {
	return wav.Encode(pcm, sttSampleRate, sttChannels, sttBitsPerSample)
}

// Transcribe POSTs `wavData` to `<endpoint>/audio/transcriptions` as
// multipart/form-data, following the OpenAI Whisper API contract.
// Speaches, faster-whisper-server, and OpenAI itself implement the same
// shape, which is why we don't need a per-provider switch here.
//
// This is a thin convenience wrapper that always declares the upload as
// WAV — what the desktop's capture→WAV pipeline produces. For audio in
// other codecs (browser MediaRecorder gives WebM/Opus or MP4/AAC) call
// TranscribeBlob and pass the original filename + content type.
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
