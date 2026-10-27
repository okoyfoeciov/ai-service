package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// Deepgram's live speech-to-text WebSocket. Nova-3 is Deepgram's
// highest-performing general-purpose model and supports the same one-recording,
// one-final-text flow as our AssemblyAI path. Unlike AssemblyAI's single
// response, Deepgram sends Results messages at speech boundaries; this handler
// joins the final ones.
var deepgramLiveURL = "wss://api.deepgram.com/v1/listen"

const (
	deepgramModel = "nova-3"
	// Deepgram bills a specific language (not "multi") as Nova-3 Monolingual,
	// which is cheaper than Nova-3 Multilingual. This app is English-only.
	deepgramLanguage = "en"
)

type deepgramMessage struct {
	Type    string `json:"type"`
	IsFinal bool   `json:"is_final"`
	Channel struct {
		Alternatives []struct {
			Transcript string `json:"transcript"`
		} `json:"alternatives"`
	} `json:"channel"`
}

// streamDeepgram pipes the incoming recording through ffmpeg into one Deepgram
// live WebSocket. Raw 16 kHz mono PCM is sent as binary frames; after the last
// frame, CloseStream asks Deepgram to flush and close.
func (s *Server) streamDeepgram(w http.ResponseWriter, r *http.Request, body *cappedReader, metrics *streamMetrics) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	q := url.Values{}
	q.Set("model", deepgramModel)
	q.Set("language", deepgramLanguage)
	q.Set("smart_format", "true")
	q.Set("punctuate", "true")
	q.Set("encoding", "linear16")
	q.Set("sample_rate", strconv.Itoa(streamSampleRate))
	q.Set("channels", strconv.Itoa(streamChannels))
	q.Set("interim_results", "false")
	q.Set("endpointing", "300")

	conn, _, err := websocket.Dial(ctx, deepgramLiveURL+"?"+q.Encode(), &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Token " + s.cfg.DeepgramAPIKey}},
	})
	if err != nil {
		jsonErrorDetail(w, "could not reach Deepgram", http.StatusBadGateway, err.Error())
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)

	pr, pw := io.Pipe()
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-i", "pipe:0",
		"-vn", "-ac", fmt.Sprint(streamChannels), "-ar", fmt.Sprint(streamSampleRate),
		"-f", "s16le", "-c:a", "pcm_s16le", "pipe:1",
	)
	cmd.Stdin = body
	cmd.Stdout = pw
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmdDone := make(chan error, 1)
	go func() {
		err := cmd.Run()
		if err != nil {
			_ = pw.CloseWithError(err)
		} else {
			_ = pw.Close()
		}
		cmdDone <- err
	}()

	sendDone := make(chan error, 1)
	go func() {
		defer pr.Close()
		buf := make([]byte, 8192)
		for {
			n, err := pr.Read(buf)
			if n > 0 {
				if werr := conn.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
					sendDone <- werr
					cancel()
					return
				}
				metrics.addAudio(n)
			}
			switch err {
			case nil:
				continue
			case io.EOF:
				metrics.markAudioDone()
				if werr := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"CloseStream"}`)); werr != nil {
					sendDone <- werr
					cancel()
					return
				}
				sendDone <- nil
				return
			default:
				sendDone <- err
				cancel()
				return
			}
		}
	}()

	var parts []string
	var readErr error
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			readErr = err
			break
		}
		if typ != websocket.MessageText {
			continue
		}
		var msg deepgramMessage
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		if msg.Type != "Results" || !msg.IsFinal || len(msg.Channel.Alternatives) == 0 {
			continue
		}
		if t := strings.TrimSpace(msg.Channel.Alternatives[0].Transcript); t != "" {
			parts = append(parts, t)
		}
	}

	sendErr := <-sendDone
	cmdErr := <-cmdDone

	if body.tooLarge.Load() {
		jsonError(w, "recording too long", http.StatusRequestEntityTooLarge)
		return
	}
	if cmdErr != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = cmdErr.Error()
		}
		jsonErrorDetail(w, "could not stream the recording to Deepgram", http.StatusBadGateway,
			stderrTail(fmt.Errorf("ffmpeg: %v: %s", cmdErr, detail)))
		return
	}
	if sendErr != nil {
		jsonErrorDetail(w, "could not stream the recording to Deepgram", http.StatusBadGateway, sendErr.Error())
		return
	}
	text := strings.Join(parts, " ")
	if text == "" {
		detail := "no final transcript"
		if readErr != nil {
			detail = readErr.Error()
		}
		jsonErrorDetail(w, "deepgram returned no transcript", http.StatusBadGateway, detail)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"text": text})
}
