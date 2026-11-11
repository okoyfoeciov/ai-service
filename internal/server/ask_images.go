package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"ai-service/internal/brave"
	"ai-service/internal/config"
	"ai-service/internal/llm"
)

// claudeImageCandidates is the owner's split: OpenRouter and Cerebras see all
// ten Brave results, Claude sees the top five with their pixels. Five 500px
// thumbnails is a sensible image-token budget; ten metadata rows is cheap.
const claudeImageCandidates = 5

// askImageTools are the two native client tools. search_images never displays
// anything; show_images is the only way an image reaches the reader's card.
func askImageTools() []llm.ToolDef {
	return []llm.ToolDef{
		{
			Name: "search_images",
			Description: "Search the web for candidate images for a visual request such as a map, photo, diagram, flag, logo, chart, or what something looks like. " +
				"Use it only when the reader explicitly asks to see an image; do not use it for ordinary questions. " +
				"It returns candidate IDs and metadata but does NOT display anything. After choosing candidates, call show_images with their IDs. " +
				"The reader will only see the images you pass to show_images.",
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "A short English image-search query, for example \"map of Uruguay\" or \"pangolin\".",
					},
				},
				"required":             []string{"query"},
				"additionalProperties": false,
			},
		},
		{
			Name: "show_images",
			Description: "Display candidate images in the reader's card. Pass one candidate ID for a single image, or several IDs for a gallery. " +
				"Only IDs returned by search_images in this conversation are valid. This is the only tool that displays images to the reader.",
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"image_ids": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"minItems":    1,
						"description": "Candidate image IDs returned by search_images.",
					},
				},
				"required":             []string{"image_ids"},
				"additionalProperties": false,
			},
		},
	}
}

type askImageCandidate struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Source  string `json:"source"`
	PageURL string `json:"page_url"`
	Width   int    `json:"width,omitempty"`
	Height  int    `json:"height,omitempty"`
}

// askImageState lives for one /api/ask request. Candidate IDs are assigned here
// so the model can refer to them in show_images without ever seeing a URL as a
// handle.
type askImageState struct {
	round      int
	send       func(string, any)
	brave      *brave.Client
	searches   int
	nextID     int
	candidates map[string]brave.Candidate
	shown      map[int]map[string]bool
}

func (s *Server) askImageExecutor(round int, send func(string, any)) llm.ToolExecutor {
	st := &askImageState{
		round:      round,
		send:       send,
		brave:      s.images,
		candidates: make(map[string]brave.Candidate),
		shown:      make(map[int]map[string]bool),
	}
	return func(ctx context.Context, name string, input json.RawMessage, includeImages bool) llm.ToolResult {
		switch name {
		case "search_images":
			return st.search(ctx, input, includeImages)
		case "show_images":
			return st.show(input)
		default:
			return llm.ToolResult{Text: "Unknown tool: " + name, IsError: true}
		}
	}
}

func (st *askImageState) search(ctx context.Context, input json.RawMessage, includeImages bool) llm.ToolResult {
	var args struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return llm.ToolResult{Text: "search_images needs a query string.", IsError: true}
	}
	query := strings.TrimSpace(args.Query)
	if query == "" {
		return llm.ToolResult{Text: "search_images needs a non-empty query.", IsError: true}
	}
	if st.searches >= config.BraveAskImageSearches {
		st.send("images", map[string]any{
			"round": st.round, "status": "error",
			"message": "Image search limit reached.",
		})
		return llm.ToolResult{Text: "Image search limit reached. Answer with the images you already have.", IsError: true}
	}
	st.searches++
	st.send("images", map[string]any{"round": st.round, "status": "searching"})

	start := time.Now()
	cands, err := st.brave.Search(ctx, query)
	if err != nil {
		log.Printf("ask[images]: search query=%q failed: %v", query, err)
		st.send("images", map[string]any{
			"round": st.round, "status": "error",
			"message": "Image search failed.",
		})
		return llm.ToolResult{Text: "Image search failed. You can answer without an image, or try one more search.", IsError: true}
	}
	if len(cands) == 0 {
		st.send("images", map[string]any{
			"round": st.round, "status": "empty",
			"message": "No images found.",
		})
		return llm.ToolResult{Text: fmt.Sprintf("No images found for %q. Answer without an image.", query)}
	}
	if includeImages && len(cands) > claudeImageCandidates {
		cands = cands[:claudeImageCandidates]
	}

	out := make([]askImageCandidate, 0, len(cands))
	chosen := make([]brave.Candidate, 0, len(cands))
	for _, c := range cands {
		st.nextID++
		id := fmt.Sprintf("img_%d", st.nextID)
		c.ID = id
		st.candidates[id] = c
		chosen = append(chosen, c)
		out = append(out, askImageCandidate{
			ID: id, Title: c.Title, Source: c.Source, PageURL: c.PageURL,
			Width: c.Width, Height: c.Height,
		})
	}

	var images []llm.ToolImage
	if includeImages {
		images = downloadBraveImages(ctx, st.brave, chosen)
	}

	payload, _ := json.Marshal(struct {
		Query      string              `json:"query"`
		Candidates []askImageCandidate `json:"candidates"`
	}{Query: query, Candidates: out})
	prefix := fmt.Sprintf("Found %d candidate image(s). This tool does not display them. Call show_images with the IDs you choose.", len(out))
	if includeImages {
		prefix += " The candidate images are attached for you to inspect."
	}
	log.Printf("ask[images]: search query=%q results=%d attached=%d duration=%s",
		query, len(out), len(images), time.Since(start).Round(time.Millisecond))
	return llm.ToolResult{Text: prefix + "\n\n" + string(payload), Images: images}
}

func (st *askImageState) show(input json.RawMessage) llm.ToolResult {
	var args struct {
		ImageIDs []string `json:"image_ids"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return llm.ToolResult{Text: "show_images needs an image_ids array.", IsError: true}
	}
	if st.shown[st.round] == nil {
		st.shown[st.round] = make(map[string]bool)
	}
	seen := make(map[string]bool, len(args.ImageIDs))
	images := make([]map[string]any, 0, len(args.ImageIDs))
	var invalid []string
	for _, rawID := range args.ImageIDs {
		id := strings.TrimSpace(rawID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		c, ok := st.candidates[id]
		if !ok {
			invalid = append(invalid, id)
			continue
		}
		if st.shown[st.round][id] {
			continue
		}
		st.shown[st.round][id] = true
		images = append(images, map[string]any{
			"id":       c.ID,
			"title":    c.Title,
			"source":   c.Source,
			"page":     c.PageURL,
			"thumb":    c.ThumbURL,
			"original": c.OriginalURL,
			"width":    c.Width,
			"height":   c.Height,
		})
	}
	if len(images) == 0 {
		return llm.ToolResult{
			Text:    "No new valid candidate IDs. Use IDs returned by search_images.",
			IsError: true,
		}
	}
	st.send("images", map[string]any{
		"round":  st.round,
		"status": "ready",
		"images": images,
	})
	text := fmt.Sprintf("Displayed %d image(s) in the reader's card.", len(images))
	if len(invalid) > 0 {
		text += " Ignored unknown IDs: " + strings.Join(invalid, ", ") + "."
	}
	return llm.ToolResult{Text: text}
}

// downloadBraveImages fetches the top candidates' Brave-proxied thumbnails for
// Claude's tool result. Failures are skipped; the metadata still reaches the
// model. OpenRouter and Cerebras never call this.
func downloadBraveImages(ctx context.Context, c *brave.Client, cands []brave.Candidate) []llm.ToolImage {
	out := make([]llm.ToolImage, len(cands))
	var wg sync.WaitGroup
	for i := range cands {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			mediaType, data, err := c.Download(ctx, cands[i].ThumbURL)
			if err != nil {
				log.Printf("ask[images]: thumbnail %s failed: %v", cands[i].ID, err)
				return
			}
			out[i] = llm.ToolImage{
				Image: llm.Image{MediaType: mediaType, Data: data},
				Label: cands[i].ID + ": " + cands[i].Title,
			}
		}(i)
	}
	wg.Wait()
	filtered := out[:0]
	for _, img := range out {
		if img.Data != "" {
			filtered = append(filtered, img)
		}
	}
	return filtered
}
