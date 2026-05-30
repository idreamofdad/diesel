//go:build cgo

package tts

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSampleFormatFor(t *testing.T) {
	cases := []struct {
		bits    int
		wantErr bool
	}{
		{8, false},
		{16, false},
		{32, false},
		{24, true}, // valid WAV depth, but miniaudio's enum doesn't expose it
		{12, true}, // nonsense depth
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
