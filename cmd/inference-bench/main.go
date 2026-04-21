// Command inference-bench: quick tokens/second and time-to-first-byte probe
// for any OpenAI-compatible /v1/chat/completions (llama.cpp, vllm-mlx, etc.).
//
//	Usage:
//	  go run ./cmd/inference-bench -url http://127.0.0.1:56802 -model mlx-community/Your-Repo-4bit
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/auxothq/auxot/pkg/llamacpp"
	"github.com/auxothq/auxot/pkg/openai"
)

func main() {
	baseURL := flag.String("url", "http://127.0.0.1:8080", "OpenAI base (no trailing slash), e.g. http://127.0.0.1:56802")
	model := flag.String("model", "local", `JSON "model" id (e.g. full mlx-community/Name-4bit, or "local" for llama.cpp single-model)`)
	prompt := flag.String("prompt", "In one short paragraph, name three things that are blue.", "User message")
	maxTok := flag.Int("max-tokens", 200, "max_tokens in response")
	temperature := flag.Float64("temp", 0.7, "temperature")
	flag.Parse()

	if *maxTok < 1 {
		*maxTok = 1
	}
	mtok := *maxTok

	req := &openai.ChatCompletionRequest{
		Model:          *model,
		Messages:       []openai.Message{{Role: "user", Content: openai.MessageContentString(*prompt)}},
		Stream:         true,
		ReturnProgress: true,
		MaxTokens:      &mtok,
	}
	if *temperature > 0 {
		f := *temperature
		req.Temperature = &f
	}

	ctx := context.Background()
	client := llamacpp.NewClient(*baseURL)
	t0 := time.Now()
	tFirst := t0
	sawText := false

	tokens, err := client.StreamCompletion(ctx, req)
	if err != nil {
		log.Fatalf("StreamCompletion: %v", err)
	}

	var outRunes, predTok int
	var lastTPS float64
	for t := range tokens {
		if t.FinishReason == "error" {
			log.Fatalf("stream error: %s", t.Content)
		}
		if t.Content != "" {
			if !sawText {
				tFirst = time.Now()
				sawText = true
			}
			for range t.Content {
				outRunes++
			}
		}
		if t.ReasoningContent != "" && !sawText {
			tFirst = time.Now()
			sawText = true
		}
		if t.Timings != nil {
			predTok = t.Timings.PredictedTokens
			if t.Timings.TokensPerSecond > 0 {
				lastTPS = t.Timings.TokensPerSecond
			}
		}
	}
	total := time.Since(t0)
	ttft := tFirst.Sub(t0)
	if !sawText {
		ttft = 0
	}

	roughTPS := float64(outRunes) / total.Seconds()
	if lastTPS > 0 {
		roughTPS = lastTPS
	} else if predTok > 0 && total.Seconds() > 0 {
		roughTPS = float64(predTok) / total.Seconds()
	}

	fmt.Println("── inference-bench (same /v1/chat/completions path as the worker) ──")
	fmt.Printf("  url:        %s\n", *baseURL)
	fmt.Printf("  model:      %s\n", *model)
	fmt.Printf("  wall:       %s\n", total.Truncate(time.Millisecond))
	fmt.Printf("  ttft:       %s  (first text/reasoning chunk; huge prompts = long prefill here)\n", ttft.Truncate(time.Millisecond))
	if predTok > 0 {
		fmt.Printf("  out_tok:    %d  (from server final timings)\n", predTok)
	} else {
		fmt.Printf("  out_runes:  %d  (no predicted_n from server; rough)\n", outRunes)
	}
	if lastTPS > 0 {
		fmt.Printf("  tokens/s:   %.1f  (from server last chunk, predicted_per_second)\n", lastTPS)
	} else {
		fmt.Printf("  tokens/s:   %.1f  (rough from wall and token count)\n", roughTPS)
	}
}
