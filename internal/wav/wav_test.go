package wav

import (
	"bytes"
	"encoding/binary"
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

func TestParse_HappyPath(t *testing.T) {
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
			info, err := Parse(raw)
			require.NoError(t, err)
			assert.Equal(t, int(tc.channels), info.Channels)
			assert.Equal(t, int(tc.sampleRate), info.SampleRate)
			assert.Equal(t, int(tc.bitsPerSample), info.BitsPerSample)
			assert.Equal(t, tc.pcm, info.PCM)
		})
	}
}

func TestParse_TolerantOfExtraChunks(t *testing.T) {
	// Common encoders sometimes insert LIST/INFO/JUNK chunks between fmt
	// and data; the parser should skip them.
	junkBetween := func(b *bytes.Buffer) {
		b.WriteString("LIST")
		_ = binary.Write(b, binary.LittleEndian, uint32(6))
		b.WriteString("INFO\x00\x00")
	}
	raw := buildWAV(t, 1, 1, 16000, 16, []byte{1, 0, 2, 0}, junkBetween)

	info, err := Parse(raw)
	require.NoError(t, err)
	assert.Equal(t, 16000, info.SampleRate)
	assert.Equal(t, []byte{1, 0, 2, 0}, info.PCM)
}

func TestParse_Errors(t *testing.T) {
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
			_, err := Parse(tc.mutate(mkValid()))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErrSub)
		})
	}
}

func TestEncodeParse_RoundTrip(t *testing.T) {
	// Encode → Parse should yield back the same params and PCM payload.
	cases := []struct {
		name          string
		sampleRate    int
		channels      int
		bitsPerSample int
		pcm           []byte
	}{
		{"empty mono 16k 16bit", 16000, 1, 16, nil},
		{"one sample", 16000, 1, 16, []byte{0, 0}},
		{"a few samples", 16000, 1, 16, []byte{1, 0, 255, 255, 0, 128}},
		{"stereo 44.1k", 44100, 2, 16, []byte{0, 0, 0, 0, 1, 0, 1, 0}},
		{"8-bit", 8000, 1, 8, []byte{128, 129, 130}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info, err := Parse(Encode(tc.pcm, tc.sampleRate, tc.channels, tc.bitsPerSample))
			require.NoError(t, err)
			assert.Equal(t, tc.sampleRate, info.SampleRate)
			assert.Equal(t, tc.channels, info.Channels)
			assert.Equal(t, tc.bitsPerSample, info.BitsPerSample)
			if len(tc.pcm) == 0 {
				assert.Empty(t, info.PCM)
			} else {
				assert.Equal(t, tc.pcm, info.PCM)
			}
		})
	}
}
