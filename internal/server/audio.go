package server

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Azure Fast Transcription (MAI-Transcribe-2) does not accept the WebM that
// comart's MediaRecorder produces, and its MIME type cannot be trusted. So
// every MAI clip is decoded and re-encoded to mono 16 kHz WAV, the format Azure
// parses most reliably.
//
// The conversion shells out to ffmpeg, which must be on PATH. install.sh checks
// for it when MAI_TRANSCRIBE_API_KEY is the /api/transcribe provider.
const (
	azureConversionTimeout = 60 * time.Second
	maxConvertedBytes      = 32 << 20 // 16 kHz mono PCM: ~1.9 MB for a 60 s clip
)

// convertForAzure is a variable so tests can stand in for ffmpeg.
var convertForAzure = convertWithFFmpeg

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
