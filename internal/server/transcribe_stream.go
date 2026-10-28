package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"ai-service/internal/config"
)

const (
	// maxStreamBytes bounds a streamed microphone recording. 25 MB is ample; a
	// 60 s WebM clip is well under 1 MB.
	maxStreamBytes = 25 << 20

	// The clients record MediaRecorder audio; ffmpeg decodes it to the PCM the
	// live endpoints require.
	streamSampleRate = 16000
	streamChannels   = 1
)

var errStreamTooLarge = errors.New("stream too large")

// statusWriter records the status a streamed transcription answers with, so the
// request log names the provider and result without touching response bodies.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Flush keeps SSE handlers streaming when they write through this wrapper.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Status() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

// streamMetrics is filled by the provider goroutines. audioBytes is the PCM
// sent to the provider, so its duration is independent of the compressed input
// format. audioDone is when the last input audio was decoded.
type streamMetrics struct {
	audioBytes atomic.Int64
	audioDone  atomic.Int64
}

func (m *streamMetrics) addAudio(n int) {
	if n > 0 {
		m.audioBytes.Add(int64(n))
	}
}

func (m *streamMetrics) markAudioDone() {
	m.audioDone.Store(time.Now().UnixNano())
}

func (m *streamMetrics) audioDuration() time.Duration {
	n := m.audioBytes.Load()
	if n <= 0 {
		return 0
	}
	seconds := float64(n) / float64(streamSampleRate*streamChannels*2)
	return time.Duration(seconds * float64(time.Second))
}

func (m *streamMetrics) postStop() (time.Duration, bool) {
	done := m.audioDone.Load()
	if done == 0 {
		return 0, false
	}
	return time.Since(time.Unix(0, done)), true
}

// countingWriter counts the PCM bytes ffmpeg writes into the provider request.
type countingWriter struct {
	w io.Writer
	m *streamMetrics
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.m.addAudio(n)
	return n, err
}

// cappedReader records whether a streamed body exceeded its limit. The caller
// is another goroutine (ffmpeg for the AssemblyAI path), so the flag is atomic.
type cappedReader struct {
	r        io.Reader
	max      int64
	read     int64
	tooLarge atomic.Bool
}

func (c *cappedReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += int64(n)
	if c.read > c.max {
		c.tooLarge.Store(true)
		return n, errStreamTooLarge
	}
	return n, err
}

// handleTranscribeStream reads one recording as it arrives and pipes it through
// ffmpeg into the selected provider's live request. The HTTP body ending is the
// stop signal, so the client can use a plain streaming fetch with no extra
// protocol. Every transcribe provider must implement a live path; there is no
// batch fallback.
func (s *Server) handleTranscribeStream(w http.ResponseWriter, r *http.Request) {
	if !requireType(w, r, "an audio type", func(mt string) bool {
		return strings.HasPrefix(mt, "audio/") || strings.HasPrefix(mt, "video/")
	}) {
		return
	}
	provider := s.TranscribeProvider()
	if provider == "" {
		jsonError(w, "server not configured: neither ASSEMBLYAI_API_KEY nor DEEPGRAM_API_KEY is set", http.StatusInternalServerError)
		return
	}
	start := time.Now()
	contentType := r.Header.Get("Content-Type")
	sw := &statusWriter{ResponseWriter: w}
	metrics := &streamMetrics{}
	defer func() {
		postStop := "-"
		if d, ok := metrics.postStop(); ok {
			postStop = d.Round(time.Millisecond).String()
		}
		log.Printf("transcribe[%s]: type=%s status=%d audio=%s total=%s post-stop=%s",
			provider, contentType, sw.Status(),
			metrics.audioDuration().Round(10*time.Millisecond),
			time.Since(start).Round(time.Millisecond), postStop)
	}()
	body := &cappedReader{r: r.Body, max: maxStreamBytes}
	switch provider {
	case config.ProviderAssemblyAI:
		s.streamAssemblyAI(sw, r, body, metrics)
	case config.ProviderDeepgram:
		s.streamDeepgram(sw, r, body, metrics)
	default:
		jsonError(sw, "server not configured", http.StatusInternalServerError)
	}
}

// handleTranscribeWS is the browser transport for the same streamed
// transcription. Chrome refuses a streaming fetch request body to loopback
// from an extension page (normal POSTs are fine, streaming to a public host is
// fine), so the extension sends MediaRecorder chunks over a WebSocket. This
// handler forwards them to the loopback /api/transcribe/stream route, whose
// body ending is the stop signal.
func (s *Server) handleTranscribeWS(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// loopbackOnly already checked Host and Origin before routing.
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(1 << 20)

	contentType := r.URL.Query().Get("type")
	if !strings.HasPrefix(contentType, "audio/") && !strings.HasPrefix(contentType, "video/") {
		_ = c.Close(websocket.StatusPolicyViolation, "Content-Type must be an audio type")
		return
	}

	ctx := r.Context()
	pr, pw := io.Pipe()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+s.cfg.ListenAddr+"/api/transcribe/stream", pr)
	if err != nil {
		_ = c.Close(websocket.StatusInternalError, "internal")
		return
	}
	req.Header.Set("Content-Type", contentType)

	type result struct {
		body []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		res, err := s.shortClient().Do(req)
		if err != nil {
			done <- result{err: err}
			return
		}
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		done <- result{body: body, err: err}
	}()

	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			_ = pw.CloseWithError(err)
			<-done
			return
		}
		switch typ {
		case websocket.MessageBinary:
			if _, err := pw.Write(data); err != nil {
				_ = c.Close(websocket.StatusInternalError, "stream closed")
				return
			}
		case websocket.MessageText:
			switch strings.TrimSpace(string(data)) {
			case "stop":
				_ = pw.Close()
				res := <-done
				if res.err != nil {
					body, _ := json.Marshal(map[string]string{
						"error":  "could not reach the transcription service",
						"detail": res.err.Error(),
					})
					_ = c.Write(ctx, websocket.MessageText, body)
					return
				}
				_ = c.Write(ctx, websocket.MessageText, res.body)
				return
			case "cancel":
				_ = pw.CloseWithError(errors.New("cancelled"))
				<-done
				return
			}
		}
	}
}

// streamAssemblyAI pipes the incoming recording through ffmpeg into one
// persistent AssemblyAI /v1/transcribe/live request. The request body is
// multipart with config first and audio second, exactly as AssemblyAI requires.
func (s *Server) streamAssemblyAI(w http.ResponseWriter, r *http.Request, body *cappedReader, metrics *streamMetrics) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	done := make(chan error, 1)
	go func() {
		var err error
		defer func() {
			if err != nil {
				_ = pw.CloseWithError(err)
			} else {
				_ = pw.Close()
			}
			done <- err
		}()

		configHeader := textproto.MIMEHeader{
			"Content-Disposition": {"form-data; name=\"config\""},
			"Content-Type":        {"application/json"},
		}
		part, e := mw.CreatePart(configHeader)
		if e != nil {
			err = e
			return
		}
		// timestamps: each word carries start/end in milliseconds. comart's
		// microphone card trims the leading silence to the first word's start,
		// which is the only honest T1 in a room with a fan and a clock in it
		// (measured, 2026-10-28: energy-only onset detection anchored on the
		// noise in 2 of 8 real recordings and left 2.4s and 4.9s of silence).
		if _, e = fmt.Fprintf(part, `{"sample_rate":%d,"channels":%d,"timestamps":true}`, streamSampleRate, streamChannels); e != nil {
			err = e
			return
		}

		audioHeader := textproto.MIMEHeader{
			"Content-Disposition": {"form-data; name=\"audio\"; filename=\"audio.pcm\""},
			"Content-Type":        {"audio/pcm"},
		}
		part, e = mw.CreatePart(audioHeader)
		if e != nil {
			err = e
			return
		}

		cmd := exec.CommandContext(ctx, "ffmpeg",
			"-hide_banner", "-loglevel", "error",
			"-i", "pipe:0",
			"-vn", "-ac", fmt.Sprint(streamChannels), "-ar", fmt.Sprint(streamSampleRate),
			"-f", "s16le", "-c:a", "pcm_s16le", "pipe:1",
		)
		cmd.Stdin = body
		cmd.Stdout = &countingWriter{w: part, m: metrics}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if e := cmd.Run(); e != nil {
			metrics.markAudioDone()
			err = fmt.Errorf("ffmpeg: %v: %s", e, strings.TrimSpace(stderr.String()))
			return
		}
		metrics.markAudioDone()
		err = mw.Close()
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, assemblyAILiveURL, pr)
	if err != nil {
		_ = pr.CloseWithError(err)
		<-done
		jsonError(w, "internal", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Authorization", s.cfg.AssemblyAIKey)
	req.Header.Set("X-AAI-Model", assemblyAIModel)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	res, doErr := s.shortClient().Do(req)
	if doErr != nil {
		_ = pr.CloseWithError(doErr)
	}
	streamErr := <-done

	if body.tooLarge.Load() {
		jsonError(w, "recording too long", http.StatusRequestEntityTooLarge)
		return
	}
	if doErr != nil {
		jsonErrorDetail(w, "could not reach AssemblyAI", http.StatusBadGateway, doErr.Error())
		return
	}
	raw := drainBody(res)
	if streamErr != nil {
		jsonErrorDetail(w, "could not stream the recording to AssemblyAI", http.StatusBadGateway, stderrTail(streamErr))
		return
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":            fmt.Sprintf("assemblyai returned %d", res.StatusCode),
			"assemblyAIStatus": res.StatusCode,
			"retryAfter":       headerOrNil(res.Header, "Retry-After"),
			"detail":           string(raw),
		})
		return
	}

	var out assemblyAIResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		jsonErrorDetail(w, "assemblyai returned unparseable JSON", http.StatusBadGateway, string(raw))
		return
	}
	answer := map[string]any{"text": strings.TrimSpace(out.Text)}
	if ms, ok := out.firstWordMS(); ok {
		answer["first_word_ms"] = ms
	}
	if ms, ok := out.lastWordMS(); ok {
		answer["last_word_ms"] = ms
	}
	writeJSON(w, http.StatusOK, answer)
}

// stderrTail keeps an ffmpeg error readable; the full stderr can be long.
func stderrTail(err error) string {
	s := strings.TrimSpace(err.Error())
	const max = 600
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
