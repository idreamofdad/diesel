package tts

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

func TestSynthesize_ConfigErrors(t *testing.T) {
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
			_, err := Synthesize(context.Background(), tc.endpoint, "k", "m", "v", tc.text)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErrSub)
		})
	}
}

func TestSynthesize_RequestShape(t *testing.T) {
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

			audioBytes, err := Synthesize(context.Background(), srv.URL+"/", tc.apiKey, tc.model, tc.voice, "hi there")
			require.NoError(t, err)
			assert.Equal(t, []byte{1, 2, 3}, audioBytes)
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

func TestSynthesize_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte("upstream down"))
	}))
	t.Cleanup(srv.Close)

	_, err := Synthesize(context.Background(), srv.URL, "k", "m", "v", "hi")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 503")
	assert.Contains(t, err.Error(), "upstream down")
}
