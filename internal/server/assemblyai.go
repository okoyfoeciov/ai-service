package server

// AssemblyAI's live API: one persistent request while the recording is made.
// The model is Universal-3.5 Pro; the X-AAI-Model header selects it.
// /v1/transcribe/live takes raw PCM, so transcribe_stream.go pipes the
// MediaRecorder stream through ffmpeg first.
var assemblyAILiveURL = "https://sync.assemblyai.com/v1/transcribe/live"

const assemblyAIModel = "universal-3-5-pro"

type assemblyAIResponse struct {
	Text string `json:"text"`
}
