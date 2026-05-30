// Package tts handles text-to-speech: synthesizing speech audio from an
// OpenAI-compatible endpoint and (on the desktop) playing it back.
//
// This file is the pure-Go half — the HTTP path that the hub and server use
// to synthesize a reply's audio (which the browser then plays). Native
// playback lives in play_cgo.go behind a //go:build cgo tag, so a
// CGO_ENABLED=0 build (the headless daemon) gets synthesis without pulling
// in miniaudio.
package tts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"diesel/internal/tracing"
	"diesel/internal/util"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
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
// We explicitly ask for WAV: the desktop playback path parses the header
// itself and feeds raw PCM to the device (predictable across servers), and
// the browser plays it directly.
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
