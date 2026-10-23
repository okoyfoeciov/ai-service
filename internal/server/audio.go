package server

import (
	"bytes"
	"context"
	"fmt"
	"mime"
	"os/exec"
	"strings"
	"time"
)

// Azure Fast Transcription (MAI-Transcribe-2) decodes WAV, MP3, OGG/Opus and
// FLAC, but not the WebM/Opus that Chromium's MediaRecorder gives comart, nor
// the MP4/AAC that is Safari's fallback. Those clips are re-encoded before they
// are uploaded; everything Azure already reads is passed through untouched.
//
// The conversion shells out to ffmpeg, which has to be on PATH. install.sh
// checks for it when MAI_TRANSCRIBE_API_KEY is the /api/transcribe provider.
const (
	azureConversionTimeout = 60 * time.Second
	maxConvertedBytes      = 32 << 20 // an OGG/Opus cap; a 60 s clip is ~240 KB
)

// azureNativeTypes are the media types Azure decodes as they are. Parameters
// such as ";codecs=opus" are ignored when matching.
var azureNativeTypes = map[string]bool{
	"audio/flac":   true,
	"audio/mp3":    true,
	"audio/mpeg":   true,
	"audio/ogg":    true,
	"audio/wav":    true,
	"audio/wave":   true,
	"audio/x-flac": true,
	"audio/x-wav":  true,
}

// convertForAzure is a variable so tests can stand in for ffmpeg.
var convertForAzure = convertWithFFmpeg

// prepareForAzure returns audio in a form Azure Fast Transcription accepts,
// plus the matching media type. Unsupported containers are converted to mono
// 16 kHz OGG/Opus, a format the MAI endpoint decodes.
func prepareForAzure(ctx context.Context, audio []byte, contentType string) ([]byte, string, error) {
	if azureNativeType(contentType) {
		return audio, contentType, nil
	}
	out, err := convertForAzure(ctx, audio)
	if err != nil {
		return nil, "", err
	}
	return out, "audio/ogg", nil
}

func azureNativeType(contentType string) bool {
	if mt, _, err := mime.ParseMediaType(contentType); err == nil {
		return azureNativeTypes[mt]
	}
	// A malformed header should not force a transcode on a type we know.
	mt := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	return azureNativeTypes[mt]
}

// convertWithFFmpeg decodes the clip from stdin and writes mono 16 kHz
// OGG/Opus to stdout. The output is capped so a very long or hostile input
// cannot exhaust memory.
func convertWithFFmpeg(ctx context.Context, audio []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, azureConversionTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-i", "pipe:0",
		"-vn", "-ac", "1", "-ar", "16000",
		"-c:a", "libopus", "-b:a", "32k",
		"-f", "ogg", "pipe:1",
	)
	cmd.Stdin = bytes.NewReader(audio)
	out := &limitedBuffer{max: maxConvertedBytes}
	var stderr bytes.Buffer
	cmd.Stdout = out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return nil, fmt.Errorf("ffmpeg could not convert the recording: %s", detail)
	}
	if out.Len() == 0 {
		return nil, fmt.Errorf("ffmpeg produced no audio")
	}
	return out.Bytes(), nil
}

type limitedBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > b.max {
		return 0, fmt.Errorf("converted recording exceeds %d bytes", b.max)
	}
	return b.buf.Write(p)
}

func (b *limitedBuffer) Len() int      { return b.buf.Len() }
func (b *limitedBuffer) Bytes() []byte { return b.buf.Bytes() }
