package main

import (
	"context"
	"encoding/binary"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pcmFromSamples little-endian-packs an int16 slice for the RMS / WAV
// tests.
func pcmFromSamples(samples ...int16) []byte {
	buf := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(s))
	}
	return buf
}

func TestFrameRMS(t *testing.T) {
	cases := []struct {
		name    string
		samples []int16
		want    float64
		// rough comparison tolerance — RMS uses floats.
		tol float64
	}{
		{
			name:    "empty frame is zero",
			samples: nil,
			want:    0,
			tol:     0,
		},
		{
			name:    "all zeros",
			samples: []int16{0, 0, 0, 0},
			want:    0,
			tol:     0,
		},
		{
			name:    "constant 1000",
			samples: []int16{1000, 1000, 1000, 1000},
			want:    1000,
			tol:     0.001,
		},
		{
			name:    "alternating ±1000",
			samples: []int16{1000, -1000, 1000, -1000},
			want:    1000,
			tol:     0.001,
		},
		{
			name:    "single sample",
			samples: []int16{2000},
			want:    2000,
			tol:     0.001,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := frameRMS(pcmFromSamples(tc.samples...))
			assert.InDelta(t, tc.want, got, tc.tol)
			assert.False(t, math.IsNaN(got), "RMS must not be NaN")
		})
	}
}

func TestEncodeWAV_HeaderShape(t *testing.T) {
	pcm := pcmFromSamples(1, 2, 3, 4)
	wav := encodeWAV(pcm)

	require.GreaterOrEqual(t, len(wav), 44, "WAV header should be 44 bytes")
	assert.Equal(t, "RIFF", string(wav[0:4]))
	assert.Equal(t, uint32(36+len(pcm)), binary.LittleEndian.Uint32(wav[4:8]))
	assert.Equal(t, "WAVE", string(wav[8:12]))
	assert.Equal(t, "fmt ", string(wav[12:16]))
	assert.Equal(t, uint32(16), binary.LittleEndian.Uint32(wav[16:20]), "PCM fmt chunk size")
	assert.Equal(t, uint16(1), binary.LittleEndian.Uint16(wav[20:22]), "format tag = PCM")
	assert.Equal(t, uint16(sttChannels), binary.LittleEndian.Uint16(wav[22:24]))
	assert.Equal(t, uint32(sttSampleRate), binary.LittleEndian.Uint32(wav[24:28]))
	assert.Equal(t, "data", string(wav[36:40]))
	assert.Equal(t, uint32(len(pcm)), binary.LittleEndian.Uint32(wav[40:44]))
	assert.Equal(t, pcm, wav[44:])
}

func TestEncodeWAV_RoundTripThroughParseWAV(t *testing.T) {
	cases := [][]int16{
		{},
		{0},
		{1, -1, 32767, -32768},
	}
	for i, samples := range cases {
		t.Run("case "+itoaForTest(i), func(t *testing.T) {
			pcm := pcmFromSamples(samples...)
			info, err := parseWAV(encodeWAV(pcm))
			require.NoError(t, err)
			assert.Equal(t, sttSampleRate, info.sampleRate)
			assert.Equal(t, sttChannels, info.channels)
			assert.Equal(t, 16, info.bitsPerSample)
			assert.Equal(t, pcm, info.pcm)
		})
	}
}

func TestTranscribe_ConfigErrors(t *testing.T) {
	cases := []struct {
		name, endpoint, wantErrSub string
	}{
		{"empty endpoint", "", "no STT endpoint configured"},
		{"whitespace endpoint", "   ", "no STT endpoint configured"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := transcribe(context.Background(),tc.endpoint, "k", "m", []byte("wav"))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErrSub)
		})
	}
}

func TestTranscribe_RequestShape(t *testing.T) {
	cases := []struct {
		name      string
		model     string
		apiKey    string
		wantModel string
		wantAuth  string
	}{
		{"explicit model", "whisper-large-v3", "sk", "whisper-large-v3", "Bearer sk"},
		{"blank model uses default", "", "sk", sttDefaultModel, "Bearer sk"},
		{"blank key omits auth", "whisper-1", "", "whisper-1", ""},
		{"whitespace model uses default", "   ", "sk", sttDefaultModel, "Bearer sk"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotModel, gotAuth, gotPath, gotFile string
			var gotResponseFormat string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotAuth = r.Header.Get("Authorization")
				ct := r.Header.Get("Content-Type")
				assert.True(t, strings.HasPrefix(ct, "multipart/form-data"), "want multipart, got %q", ct)

				require.NoError(t, r.ParseMultipartForm(1<<20))
				gotModel = r.FormValue("model")
				gotResponseFormat = r.FormValue("response_format")
				f, _, err := r.FormFile("file")
				require.NoError(t, err)
				defer f.Close()
				raw, _ := io.ReadAll(f)
				gotFile = string(raw)

				_, _ = w.Write([]byte(`{"text":"hello world"}`))
			}))
			t.Cleanup(srv.Close)

			text, err := transcribe(context.Background(),srv.URL+"/", tc.apiKey, tc.model, []byte("FAKEWAV"))
			require.NoError(t, err)
			assert.Equal(t, "hello world", text)
			assert.Equal(t, "/audio/transcriptions", gotPath)
			assert.Equal(t, tc.wantAuth, gotAuth)
			assert.Equal(t, tc.wantModel, gotModel)
			assert.Equal(t, "json", gotResponseFormat)
			assert.Equal(t, "FAKEWAV", gotFile)
		})
	}
}

func TestTranscribe_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte("model loading"))
	}))
	t.Cleanup(srv.Close)

	_, err := transcribe(context.Background(),srv.URL, "k", "m", []byte("wav"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 503")
	assert.Contains(t, err.Error(), "model loading")
}

func TestTranscribe_TrimsResponseText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"text":"   leading and trailing   "}`))
	}))
	t.Cleanup(srv.Close)

	text, err := transcribe(context.Background(),srv.URL, "", "", []byte{})
	require.NoError(t, err)
	assert.Equal(t, "leading and trailing", text)
}

func TestAudioDeviceEnumeration_DoesNotPanic(t *testing.T) {
	// Smoke test — Qt's QMediaDevices_AudioInputs/Outputs hit the host
	// audio subsystem. On CI the device list is typically empty; on a
	// dev machine it's non-empty. We can't assert content, but we can
	// guarantee the call doesn't crash and returns a slice (possibly
	// empty) instead of nil — surfaces breakage if a future miqt update
	// changes the return convention.
	ins := audioInputDescriptions()
	outs := audioOutputDescriptions()
	assert.NotNil(t, ins)
	assert.NotNil(t, outs)
	for _, name := range ins {
		assert.NotEmpty(t, name, "empty device descriptions should have been filtered")
	}
	for _, name := range outs {
		assert.NotEmpty(t, name, "empty device descriptions should have been filtered")
	}
}

// Compile-time check that multipart is reachable in the test file —
// referenced here to keep the import live if the parsing logic above
// gets reshuffled.
var _ = multipart.NewWriter
