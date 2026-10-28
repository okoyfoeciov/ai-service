package server

// AssemblyAI's live API: one persistent request while the recording is made.
// The model is Universal-3.5 Pro; the X-AAI-Model header selects it.
// /v1/transcribe/live takes raw PCM, so transcribe_stream.go pipes the
// MediaRecorder stream through ffmpeg first.
var assemblyAILiveURL = "https://sync.assemblyai.com/v1/transcribe/live"

const assemblyAIModel = "universal-3-5-pro"

type assemblyAIResponse struct {
	Text  string `json:"text"`
	Words []struct {
		Text  string `json:"text"`
		Start *int   `json:"start"` // milliseconds from the first sample; absent when unaligned
		End   *int   `json:"end"`
	} `json:"words"`
}

// firstWordMS is the start of the first word the service could align, and
// whether there was one. comart trims the recording's lead-in here; a word
// without a start is skipped, and a response with no timings at all says
// nothing rather than guessing.
func (r assemblyAIResponse) firstWordMS() (int, bool) {
	for _, w := range r.Words {
		if w.Start != nil && *w.Start >= 0 {
			return *w.Start, true
		}
	}
	return 0, false
}

// lastWordMS is the END of the last word the service could align. comart ends
// the microphone clip here: the recognizer knows which sound was the last word,
// including a soft final "s" or "th" that an energy threshold would miss. A word
// without an end is skipped; no timings at all says nothing rather than guessing.
func (r assemblyAIResponse) lastWordMS() (int, bool) {
	for i := len(r.Words) - 1; i >= 0; i-- {
		if r.Words[i].End != nil && *r.Words[i].End >= 0 {
			return *r.Words[i].End, true
		}
	}
	return 0, false
}
