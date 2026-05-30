//go:build cgo

package audio

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
)

// pcmFromSamples lives in audio_test.go (same package); both the pure and
// cgo test files share it.

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

func TestAudioDeviceEnumeration_DoesNotPanic(t *testing.T) {
	// Smoke test — miniaudio's device enumeration hits the host audio
	// subsystem. On CI the device list is typically empty; on a dev machine
	// it's non-empty. We can't assert content, but we can guarantee the call
	// doesn't crash and returns a slice (possibly empty) instead of nil.
	ins := InputDescriptions()
	outs := OutputDescriptions()
	assert.NotNil(t, ins)
	assert.NotNil(t, outs)
	for _, name := range ins {
		assert.NotEmpty(t, name, "empty device descriptions should have been filtered")
	}
	for _, name := range outs {
		assert.NotEmpty(t, name, "empty device descriptions should have been filtered")
	}
}
