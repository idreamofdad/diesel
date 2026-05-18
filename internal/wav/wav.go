// Package wav implements the tiny slice of the RIFF/WAVE format Diesel
// needs end-to-end: encoding captured PCM for upload to STT, and parsing
// downloaded PCM from TTS for playback. Only the PCM format tag is
// supported — that's what every server we target produces.
package wav

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

// Info is what Parse extracts from a WAV header, plus a slice pointing at
// the raw PCM payload. The PCM slice aliases the input buffer; callers
// that need to retain it past the input's lifetime should copy.
type Info struct {
	SampleRate    int
	Channels      int
	BitsPerSample int
	PCM           []byte
}

// Encode wraps a PCM buffer in a 44-byte WAV/RIFF header so a server can
// decode it without us declaring the audio format out of band. Only the
// PCM format tag is emitted — callers pass already-encoded little-endian
// samples in `pcm`.
func Encode(pcm []byte, sampleRate, channels, bitsPerSample int) []byte {
	byteRate := sampleRate * channels * bitsPerSample / 8
	blockAlign := channels * bitsPerSample / 8
	var buf bytes.Buffer
	w := func(v any) { _ = binary.Write(&buf, binary.LittleEndian, v) }
	buf.WriteString("RIFF")
	w(uint32(36 + len(pcm)))
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	w(uint32(16)) // PCM fmt-chunk size
	w(uint16(1))  // PCM format tag
	w(uint16(channels))
	w(uint32(sampleRate))
	w(uint32(byteRate))
	w(uint16(blockAlign))
	w(uint16(bitsPerSample))
	buf.WriteString("data")
	w(uint32(len(pcm)))
	buf.Write(pcm)
	return buf.Bytes()
}

// Parse walks the RIFF chunks until it finds fmt and data. Tolerant of
// optional chunks (LIST, INFO, JUNK, …) that some encoders insert between
// fmt and data — those are skipped over by their declared size. Only PCM
// (format tag 1) is supported, which is what Speaches and OpenAI's TTS
// both emit when response_format is "wav".
func Parse(b []byte) (Info, error) {
	var w Info
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return w, errors.New("not a WAV file")
	}
	pos := 12
	foundFmt := false
	for pos+8 <= len(b) {
		tag := string(b[pos : pos+4])
		size := int(binary.LittleEndian.Uint32(b[pos+4 : pos+8]))
		pos += 8
		if size < 0 || pos+size > len(b) {
			return w, errors.New("truncated WAV chunk")
		}
		chunk := b[pos : pos+size]
		switch tag {
		case "fmt ":
			if size < 16 {
				return w, errors.New("short fmt chunk")
			}
			if formatTag := binary.LittleEndian.Uint16(chunk[0:2]); formatTag != 1 {
				return w, fmt.Errorf("unsupported WAV format tag %d (need PCM)", formatTag)
			}
			w.Channels = int(binary.LittleEndian.Uint16(chunk[2:4]))
			w.SampleRate = int(binary.LittleEndian.Uint32(chunk[4:8]))
			w.BitsPerSample = int(binary.LittleEndian.Uint16(chunk[14:16]))
			foundFmt = true
		case "data":
			if !foundFmt {
				return w, errors.New("WAV data before fmt")
			}
			w.PCM = chunk
			return w, nil
		}
		// RIFF chunks are padded to even length.
		pos += size
		if size%2 == 1 && pos < len(b) {
			pos++
		}
	}
	return w, errors.New("no data chunk in WAV")
}
