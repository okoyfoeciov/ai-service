package server

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Azure Fast Transcription (MAI-Transcribe-2) accepts WAV, MP3, OGG/Opus and
// FLAC, but not the WebM that Chromium's MediaRecorder gives comart: Azure
// answers 400 invalid_audio. Its MIME cannot be trusted either — a container
// can be labelled audio/ogg while the bytes are not — and its OGG parser
// rejects some MediaRecorder conversions as "file is malformed". So every MAI
// clip is decoded and re-encoded to mono 16 kHz WAV, the simplest format Azure
// parses reliably.
//
// The conversion shells out to ffmpeg, which has to be on PATH. install.sh
// checks for it when MAI_TRANSCRIBE_API_KEY is the /api/transcribe provider.
const (
	azureConversionTimeout = 60 * time.Second
	maxConvertedBytes      = 32 << 20 // 16 kHz mono PCM: ~1.9 MB for a 60 s clip
)

// convertForAzure is a variable so tests can stand in for ffmpeg.
var convertForAzure = convertWithFFmpeg

// prepareForAzure returns the clip as mono 16 kHz WAV. It always converts:
// the input's declared type is only a hint, and Azure's parser is least
// forgiving about containers it did not produce.
func prepareForAzure(ctx context.Context, audio []byte) ([]byte, error) {
	return convertForAzure(ctx, audio)
}

// convertWithFFmpeg decodes the clip from stdin and writes WAV to stdout. The
// output is capped so a very long or hostile input cannot exhaust memory.
func convertWithFFmpeg(ctx context.Context, audio []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, azureConversionTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-i", "pipe:0",
		"-vn", "-ac", "1", "-ar", "16000",
		"-c:a", "pcm_s16le",
		"-f", "wav", "pipe:1",
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
