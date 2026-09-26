// Package ai is everything that calls a language model: the background jobs
// (thread digests, finding related posts, writing link notes) and Ask, the
// question-answering search (plan sections 2, 3 and 9).
//
// Two rules shape all of it:
//
//   - The AI never presents as an AI. There's no chat and no persona: every
//     call returns structured JSON, and the server renders it with its own
//     templates, so the model can't write anything that looks like a reply.
//     The shared voice rules (prompts.go) keep what it writes factual,
//     attributed, and free of advice.
//   - The model never touches the database and never sees who wrote what.
//     Threads reach it as transcripts labeled "OP" and "commenter N"
//     (store.ThreadText), and every result is checked and written by
//     ordinary code.
package ai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// LLM is one model endpoint. v1 has one implementation, Ollama on the
// Studio. Another (a bigger local model, or an API) is one more type with
// the same method; the prompts and schemas don't change.
type LLM interface {
	// Call sends one request and decodes the model's JSON answer into out.
	Call(ctx context.Context, req Request, out any) (Usage, error)
}

// Request is one model call.
type Request struct {
	// System is the fixed part: the voice rules plus one task's
	// instructions. It's identical for every call of that task, so the
	// model server keeps it cached between calls.
	System string
	// Prompt is this call's content: the threads, the question.
	Prompt string
	// Schema is the JSON schema the answer must match. Ollama constrains
	// the output to it, which is what makes a small model reliable here.
	Schema map[string]any
	// Images are small JPEG thumbnails, for models that can see (M5's
	// moderation check).
	Images [][]byte
}

// Usage is what a call cost: tokens, and time, which is the real cost on
// local hardware.
type Usage struct {
	InputTokens  int64
	OutputTokens int64
	Seconds      float64
}

// Ollama calls Ollama's native chat API.
type Ollama struct {
	URL     string // e.g. http://127.0.0.1:11434
	Model   string // e.g. a small model picked with `grus bench-llm`
	Context int    // context window in tokens; Ollama's default (2048-4096) silently truncates threads
	HTTP    *http.Client
	// Current, if set, is asked for the model and context window on every
	// call, overriding Model and Context: a server reads them from the
	// global settings, so changing the model on the admin page changes it
	// on every node without a restart.
	Current func() (model string, contextTokens int)
}

// NewOllama returns a client for model at url.
func NewOllama(url, model string, contextTokens int) *Ollama {
	if contextTokens == 0 {
		contextTokens = 16384
	}
	return &Ollama{URL: strings.TrimRight(url, "/"), Model: model, Context: contextTokens,
		// No client timeout: callers pass a context with the deadline
		// that fits the task (8 seconds for search, minutes for jobs).
		HTTP: &http.Client{}}
}

type ollamaMessage struct {
	Role    string   `json:"role"`
	Content string   `json:"content"`
	Images  []string `json:"images,omitempty"`
}

func (o *Ollama) Call(ctx context.Context, req Request, out any) (Usage, error) {
	start := time.Now()
	model, numCtx := o.Model, o.Context
	if o.Current != nil {
		model, numCtx = o.Current()
	}
	user := ollamaMessage{Role: "user", Content: req.Prompt}
	for _, im := range req.Images {
		user.Images = append(user.Images, base64.StdEncoding.EncodeToString(im))
	}
	body, err := json.Marshal(map[string]any{
		"model":    model,
		"messages": []ollamaMessage{{Role: "system", Content: req.System}, user},
		"format":   req.Schema,
		"stream":   false,
		// Temperature 0: the same thread gives the same note, and the
		// model sticks to what the text says.
		"options":    map[string]any{"temperature": 0, "num_ctx": numCtx},
		"keep_alive": "30m", // stay loaded between jobs; loading takes seconds
	})
	if err != nil {
		return Usage{}, err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, o.URL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return Usage{}, err
	}
	hr.Header.Set("Content-Type", "application/json")
	resp, err := o.HTTP.Do(hr)
	if err != nil {
		return Usage{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return Usage{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return Usage{}, fmt.Errorf("ollama: %s: %s", resp.Status, bytes.TrimSpace(data))
	}
	var r struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		PromptEvalCount int64 `json:"prompt_eval_count"`
		EvalCount       int64 `json:"eval_count"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return Usage{}, fmt.Errorf("ollama: %v", err)
	}
	u := Usage{InputTokens: r.PromptEvalCount, OutputTokens: r.EvalCount, Seconds: time.Since(start).Seconds()}
	if err := json.Unmarshal([]byte(r.Message.Content), out); err != nil {
		return u, fmt.Errorf("model answer isn't the expected JSON: %v", err)
	}
	return u, nil
}
