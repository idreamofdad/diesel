//go:build cgo

// Command voicecheck is an interactive harness for the speech pipeline:
// capture → VAD → STT (Whisper) → TTS synthesis → playback. It exists to
// validate the malgo-backed audio I/O without standing up the full GUI.
//
// It reads the same SQLite-backed settings the desktop app does, so the
// STT/TTS endpoints, models, voice, and device choices configured in the
// app are reused here. Point those at a running server first, then:
//
//	CGO_CXXFLAGS="-std=c++17" go run ./cmd/voicecheck
//
// Press Enter to record a phrase; the VAD stops capture after a trailing
// pause, the audio is transcribed, and the transcript is spoken back so the
// whole round trip is exercised. Type q then Enter to quit.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"diesel/internal/audio"
	"diesel/internal/settings"
	"diesel/internal/storage"
	"diesel/internal/tts"
	"diesel/internal/util"
)

func main() {
	dataDir := flag.String("data-dir", "", "directory for Diesel's data (defaults to the OS user config dir, same as the app)")
	flag.Parse()
	if *dataDir != "" {
		util.SetConfigDir(*dataDir)
	}

	// Wire settings to the same database the desktop app uses so the STT/TTS
	// config carries over. Mirrors the backend injection in cmd/diesel.
	dbPath, err := util.ConfigFilePath("diesel.db")
	if err != nil {
		log.Fatalf("config path: %v", err)
	}
	store, err := storage.Open(dbPath)
	if err != nil {
		log.Fatalf("open storage: %v", err)
	}
	defer func() { _ = store.Close() }()
	settings.SetBackend(
		func() settings.AppSettings {
			s, err := store.LoadSettings(context.Background())
			if err != nil {
				log.Printf("load settings: %v", err)
			}
			return s
		},
		func(s settings.AppSettings) error {
			return store.SaveSettings(context.Background(), s)
		},
	)

	ctx := context.Background()
	if _, err := audio.Context(); err != nil {
		log.Fatalf("audio backend: %v", err)
	}
	printConfig()

	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("\n[Enter] record · [q] quit > ")
		line, err := reader.ReadString('\n')
		if err != nil { // EOF (Ctrl-D) ends the session cleanly.
			fmt.Println()
			return
		}
		if strings.TrimSpace(line) == "q" {
			return
		}
		runTurn(ctx)
	}
}

// printConfig echoes the resolved endpoints so a misconfiguration is obvious
// before the first recording, without leaking the API keys.
func printConfig() {
	s := settings.Load()
	sttEP := util.FirstNonEmpty(s.STTEndpoint, s.APIEndpoint)
	ttsEP := util.FirstNonEmpty(s.TTSEndpoint, s.APIEndpoint)
	fmt.Println("── voicecheck ──────────────────────────────")
	fmt.Printf("  input device : %s\n", orDefault(s.InputDevice))
	fmt.Printf("  output device: %s\n", orDefault(s.OutputDevice))
	fmt.Printf("  STT endpoint : %s  (model %s)\n", orUnset(sttEP), orUnset(s.STTModel))
	fmt.Printf("  TTS endpoint : %s  (model %s, voice %s)\n", orUnset(ttsEP), orUnset(s.TTSModel), orUnset(s.TTSVoice))
	fmt.Println("────────────────────────────────────────────")
	if sttEP == "" {
		fmt.Println("  ⚠ no STT endpoint configured — set one in the app's Settings → Speech-to-Text.")
	}
}

// recResult carries the capture outcome from the audio thread (StartRecording
// fires onStop on a goroutine) back to the turn loop.
type recResult struct {
	pcm    []byte
	reason audio.StopReason
}

func runTurn(ctx context.Context) {
	s := settings.Load()

	done := make(chan recResult, 1)
	if _, err := audio.StartRecording(ctx, func(pcm []byte, reason audio.StopReason) {
		done <- recResult{pcm, reason}
	}); err != nil {
		fmt.Println("✗ start recording:", err)
		return
	}
	fmt.Println("● recording — speak now (auto-stops after a pause)…")

	res := <-done
	switch res.reason {
	case audio.StopNoSpeech:
		fmt.Println("  (no speech detected)")
		return
	case audio.StopCancelled:
		fmt.Println("  (cancelled)")
		return
	}
	fmt.Printf("  captured %d bytes PCM (%s)\n", len(res.pcm), res.reason)

	// STT.
	sttEP := util.FirstNonEmpty(s.STTEndpoint, s.APIEndpoint)
	sttKey := util.FirstNonEmpty(s.STTAPIKey, s.APIKey)
	if sttEP == "" {
		fmt.Println("✗ no STT endpoint configured")
		return
	}
	text, err := audio.Transcribe(ctx, sttEP, sttKey, s.STTModel, audio.EncodeWAV(res.pcm))
	if err != nil {
		fmt.Println("✗ transcribe:", err)
		return
	}
	if strings.TrimSpace(text) == "" {
		fmt.Println("  (transcript empty)")
		return
	}
	fmt.Printf("  heard: %q\n", text)

	// TTS synthesis + playback — speak the transcript straight back as a
	// loopback test of the synthesis and playback paths.
	ttsEP := util.FirstNonEmpty(s.TTSEndpoint, s.APIEndpoint)
	ttsKey := util.FirstNonEmpty(s.TTSAPIKey, s.APIKey)
	if ttsEP == "" {
		fmt.Println("  (no TTS endpoint configured — skipping playback)")
		return
	}
	audioBytes, err := tts.Synthesize(ctx, ttsEP, ttsKey, s.TTSModel, s.TTSVoice, text)
	if err != nil {
		fmt.Println("✗ synthesize:", err)
		return
	}
	played := make(chan struct{})
	sp, err := tts.Play(ctx, audioBytes)
	if err != nil {
		fmt.Println("✗ play:", err)
		return
	}
	// OnDone fires on the natural-end path; the 200 ms tail delay in Play
	// means setting it here (just after Start) comfortably wins the race.
	sp.OnDone = func() { close(played) }
	fmt.Println("  ♪ speaking…")
	select {
	case <-played:
	case <-time.After(30 * time.Second):
		sp.Stop()
		fmt.Println("  (playback timed out)")
	}
}

func orDefault(v string) string {
	if strings.TrimSpace(v) == "" {
		return "System Default"
	}
	return v
}

func orUnset(v string) string {
	if strings.TrimSpace(v) == "" {
		return "(unset)"
	}
	return v
}
