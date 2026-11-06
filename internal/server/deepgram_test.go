package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"ai-service/internal/config"
)

func TestTranscribeStreamDeepgram(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	oldURL := deepgramLiveURL
	defer func() { deepgramLiveURL = oldURL }()

	var gotAuth, gotModel, gotLanguage, gotEncoding, gotSampleRate, gotSmartFormat, gotInterim, gotRawQuery string
	var gotAudio int
	var firstFinalSent bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotRawQuery = r.URL.RawQuery
		q := r.URL.Query()
		gotModel = q.Get("model")
		gotLanguage = q.Get("language")
		gotEncoding = q.Get("encoding")
		gotSampleRate = q.Get("sample_rate")
		gotSmartFormat = q.Get("smart_format")
		gotInterim = q.Get("interim_results")

		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		for {
			typ, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			if typ == websocket.MessageBinary {
				gotAudio += len(data)
				if !firstFinalSent && gotAudio > 0 {
					firstFinalSent = true
					_ = c.Write(ctx, websocket.MessageText,
						[]byte(`{"type":"Results","is_final":true,"channel":{"alternatives":[{"transcript":"first part"}]}}`))
				}
				continue
			}
			var msg struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(data, &msg) != nil || msg.Type != "CloseStream" {
				continue
			}
			_ = c.Write(ctx, websocket.MessageText,
				[]byte(`{"type":"Results","is_final":true,"channel":{"alternatives":[{"transcript":"second part."}]}}`))
			_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"Metadata"}`))
			_ = c.Close(websocket.StatusNormalClosure, "")
			return
		}
	}))
	defer upstream.Close()
	deepgramLiveURL = "ws" + strings.TrimPrefix(upstream.URL, "http")

	s := &Server{
		cfg: config.Config{
			DeepgramAPIKey:     "dg-key",
			TranscribeProvider: config.ProviderDeepgram,
		},
		httpShort: upstream.Client(),
	}
	// Deepgram ignores ?timestamps=0: the upstream request and the answer are
	// the same as without it.
	var firstRawQuery string
	for i, path := range []string{"/api/transcribe/stream", "/api/transcribe/stream?timestamps=0"} {
		gotAudio, firstFinalSent = 0, false
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(testWAV()))
		req.Host = "127.0.0.1:8770"
		req.Header.Set("Content-Type", "audio/wav")
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", path, rec.Code, rec.Body.String())
		}
		var out map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s: answer %q is not JSON: %v", path, rec.Body.String(), err)
		}
		if out["text"] != "first part second part." || len(out) != 1 {
			t.Errorf("%s: answer %v, want only both final segments joined", path, out)
		}
		if gotAuth != "Token dg-key" || gotModel != "nova-3" || gotLanguage != "en" || gotEncoding != "linear16" ||
			gotSampleRate != "16000" || gotSmartFormat != "true" || gotInterim != "false" {
			t.Errorf("%s: query/auth: auth=%q model=%q language=%q encoding=%q sample_rate=%q smart_format=%q interim_results=%q",
				path, gotAuth, gotModel, gotLanguage, gotEncoding, gotSampleRate, gotSmartFormat, gotInterim)
		}
		if i == 0 {
			firstRawQuery = gotRawQuery
		} else if gotRawQuery != firstRawQuery {
			t.Errorf("%s: Deepgram query %q, want %q", path, gotRawQuery, firstRawQuery)
		}
		if gotAudio == 0 {
			t.Errorf("%s: Deepgram received no PCM audio", path)
		}
	}
}
