package llm

import (
	"context"
	"encoding/json"
)

// ToolDef is one client tool offered to the model. Schema is the JSON Schema
// object (type, properties, required, additionalProperties). Each provider
// translates the same definition into its own wire shape.
type ToolDef struct {
	Name        string
	Description string
	Schema      map[string]any
}

// ToolImage is one image attached to a tool result, with the candidate ID it
// belongs to. The label matters when some thumbnails fail to download: Claude
// must still know which image is img_1 and which is img_2.
type ToolImage struct {
	Image
	Label string
}

// ToolResult is what the backend returns to the model after it executes a tool.
// Images is used only by providers whose native tool result accepts image
// blocks (Claude); the OpenAI-compatible clients send Text only.
type ToolResult struct {
	Text    string
	Images  []ToolImage
	IsError bool
}

// ToolExecutor runs one native tool call. includeImages is true only for
// providers that can put images in a tool result; the executor uses it to avoid
// downloading thumbnails that the other providers would discard.
type ToolExecutor func(ctx context.Context, name string, input json.RawMessage, includeImages bool) ToolResult

// ToolStreamer is a Streamer that supports a native client-tool loop. The
// client defines the tools, executes them through exec, and sends the results
// back to the model until it answers or the loop budget runs out.
type ToolStreamer interface {
	StreamWithTools(ctx context.Context, prompt string, images []Image, tools []ToolDef, exec ToolExecutor, onText func(string), onSearch func(bool)) Result
}
