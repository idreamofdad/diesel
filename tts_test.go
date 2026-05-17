package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildWAV constructs a minimal RIFF/WAVE buffer with a fmt chunk and a
// data chunk. Tests can override the format tag / chunk order / payload
// size to exercise edge cases the parser has to tolerate.
func buildWAV(t *testing.T, formatTag uint16, channels uint16, sampleRate uint32, bitsPerSample uint16, pcm []byte, extras ...func(*bytes.Buffer)) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := func(v any) { require.NoError(t, binary.Write(&buf, binary.LittleEndian, v)) }
	buf.WriteString("RIFF")
	// File size — content length filled in after the body is assembled.
	w(uint32(0))
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	w(uint32(16))
	w(formatTag)
	w(channels)
	w(sampleRate)
	w(uint32(sampleRate) * uint32(channels) * uint32(bitsPerSample) / 8) // byteRate
	w(uint16(channels * bitsPerSample / 8))                              // blockAlign
	w(bitsPerSample)
	for _, extra := range extras {
		extra(&buf)
	}
	buf.WriteString("data")
	w(uint32(len(pcm)))
	buf.Write(pcm)
	// Backfill RIFF size = total - 8.
	data := buf.Bytes()
	binary.LittleEndian.PutUint32(data[4:8], uint32(len(data)-8))
	return data
}

func TestParseWAV_HappyPath(t *testing.T) {
	cases := []struct {
		name          string
		channels      uint16
		sampleRate    uint32
		bitsPerSample uint16
		pcm           []byte
	}{
		{"mono 16-bit 16k", 1, 16000, 16, []byte{0, 0, 1, 0, 2, 0}},
		{"stereo 16-bit 44k", 2, 44100, 16, []byte{0, 0, 0, 0}},
		{"mono 8-bit 8k", 1, 8000, 8, []byte{128, 129, 130}},
		{"mono 32-bit 48k", 1, 48000, 32, []byte{0, 0, 0, 0, 1, 0, 0, 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := buildWAV(t, 1, tc.channels, tc.sampleRate, tc.bitsPerSample, tc.pcm)
			info, err := parseWAV(raw)
			require.NoError(t, err)
			assert.Equal(t, int(tc.channels), info.channels)
			assert.Equal(t, int(tc.sampleRate), info.sampleRate)
			assert.Equal(t, int(tc.bitsPerSample), info.bitsPerSample)
			assert.Equal(t, tc.pcm, info.pcm)
		})
	}
}

func TestParseWAV_TolerantOfExtraChunks(t *testing.T) {
	// Common encoders sometimes insert LIST/INFO/JUNK chunks between fmt
	// and data; the parser should skip them.
	junkBetween := func(b *bytes.Buffer) {
		b.WriteString("LIST")
		_ = binary.Write(b, binary.LittleEndian, uint32(6))
		b.WriteString("INFO\x00\x00")
	}
	raw := buildWAV(t, 1, 1, 16000, 16, []byte{1, 0, 2, 0}, junkBetween)

	info, err := parseWAV(raw)
	require.NoError(t, err)
	assert.Equal(t, 16000, info.sampleRate)
	assert.Equal(t, []byte{1, 0, 2, 0}, info.pcm)
}

func TestParseWAV_Errors(t *testing.T) {
	mkValid := func() []byte { return buildWAV(t, 1, 1, 16000, 16, []byte{0, 0}) }

	cases := []struct {
		name       string
		mutate     func([]byte) []byte
		wantErrSub string
	}{
		{
			name:       "too short to be RIFF",
			mutate:     func([]byte) []byte { return []byte("RIF") },
			wantErrSub: "not a WAV",
		},
		{
			name:       "missing RIFF header",
			mutate:     func([]byte) []byte { return append([]byte("XXXX"), make([]byte, 100)...) },
			wantErrSub: "not a WAV",
		},
		{
			name: "wrong WAVE marker",
			mutate: func(b []byte) []byte {
				out := append([]byte{}, b...)
				copy(out[8:12], "WVEX")
				return out
			},
			wantErrSub: "not a WAV",
		},
		{
			name: "non-PCM format tag",
			mutate: func(b []byte) []byte {
				out := append([]byte{}, b...)
				// fmt chunk's formatTag is at offset 20.
				binary.LittleEndian.PutUint16(out[20:22], 3) // IEEE float
				return out
			},
			wantErrSub: "unsupported WAV format tag",
		},
		{
			name: "chunk size overruns buffer",
			mutate: func(b []byte) []byte {
				out := append([]byte{}, b...)
				// fmt chunk size is at offset 16 — bloat it past end.
				binary.LittleEndian.PutUint32(out[16:20], 1<<30)
				return out
			},
			wantErrSub: "truncated WAV chunk",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseWAV(tc.mutate(mkValid()))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErrSub)
		})
	}
}

func TestSampleFormatFor(t *testing.T) {
	cases := []struct {
		bits    int
		wantErr bool
	}{
		{8, false},
		{16, false},
		{32, false},
		{24, true},  // valid WAV depth, but Qt enum doesn't expose it
		{12, true},  // nonsense depth
		{0, true},
		{-1, true},
	}
	for _, tc := range cases {
		_, err := sampleFormatFor(tc.bits)
		if tc.wantErr {
			assert.Error(t, err, "bits=%d", tc.bits)
		} else {
			assert.NoError(t, err, "bits=%d", tc.bits)
		}
	}
}

func TestSynthesizeTTS_ConfigErrors(t *testing.T) {
	cases := []struct {
		name           string
		endpoint, text string
		wantErrSub     string
	}{
		{"no endpoint", "", "hello", "no TTS endpoint configured"},
		{"whitespace endpoint", "   ", "hello", "no TTS endpoint configured"},
		{"empty text", "http://x", "", "nothing to speak"},
		{"whitespace text", "http://x", "   ", "nothing to speak"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := synthesizeTTS(context.Background(),tc.endpoint, "k", "m", "v", tc.text)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErrSub)
		})
	}
}

func TestSynthesizeTTS_RequestShape(t *testing.T) {
	// Verify the wire payload, headers, and that defaults kick in when
	// model/voice are blank.
	cases := []struct {
		name      string
		model     string
		voice     string
		apiKey    string
		wantModel string
		wantVoice string
		wantAuth  string
	}{
		{name: "explicit model and voice", model: "tts-2", voice: "ember", apiKey: "sk", wantModel: "tts-2", wantVoice: "ember", wantAuth: "Bearer sk"},
		{name: "blank model uses default", model: "", voice: "ember", apiKey: "sk", wantModel: ttsDefaultModel, wantVoice: "ember", wantAuth: "Bearer sk"},
		{name: "blank voice uses default", model: "tts-1", voice: "", apiKey: "", wantModel: "tts-1", wantVoice: ttsDefaultVoice, wantAuth: ""},
		{name: "blank both", model: "", voice: "", apiKey: "", wantModel: ttsDefaultModel, wantVoice: ttsDefaultVoice, wantAuth: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotBody map[string]any
			var gotAuth, gotPath, gotCT string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotAuth = r.Header.Get("Authorization")
				gotCT = r.Header.Get("Content-Type")
				raw, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(raw, &gotBody)
				_, _ = w.Write([]byte{1, 2, 3})
			}))
			t.Cleanup(srv.Close)

			audio, err := synthesizeTTS(context.Background(),srv.URL+"/", tc.apiKey, tc.model, tc.voice, "hi there")
			require.NoError(t, err)
			assert.Equal(t, []byte{1, 2, 3}, audio)
			assert.Equal(t, "/audio/speech", gotPath)
			assert.Equal(t, "application/json", gotCT)
			assert.Equal(t, tc.wantAuth, gotAuth)
			assert.Equal(t, tc.wantModel, gotBody["model"])
			assert.Equal(t, tc.wantVoice, gotBody["voice"])
			assert.Equal(t, "hi there", gotBody["input"])
			assert.Equal(t, "wav", gotBody["response_format"])
		})
	}
}

func TestSynthesizeTTS_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte("upstream down"))
	}))
	t.Cleanup(srv.Close)

	_, err := synthesizeTTS(context.Background(),srv.URL, "k", "m", "v", "hi")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 503")
	assert.Contains(t, err.Error(), "upstream down")
}
