package llm

import "context"

// Image is one image attached to a prompt. Data is base64 without the
// "data:<media-type>;base64," prefix, which is the form both the Anthropic
// Messages API and the OpenAI-compatible image_url field want.
type Image struct {
	MediaType string
	Data      string
}

// ImageStreamer is a Streamer that can also carry images. Every client used by
// /api/ask implements it; the ask handler falls back to Stream when a request
// has no images, so a future text-only provider still serves questions.
type ImageStreamer interface {
	StreamImages(ctx context.Context, prompt string, images []Image, onText func(string)) Result
}
