package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ekasc/escape-engine/internal/provider"
	"github.com/ekasc/escape-engine/internal/session"
	"github.com/ekasc/escape-engine/internal/settings"
)

const maxCacheBenchmarkRuns = 20

func defaultCacheBenchmarkPrompt() string {
	return strings.Repeat("Escape cache benchmark stable prefix. This sentence is repeated so the provider has a stable prompt prefix to cache. ", 80)
}

func cmdCacheBenchmark(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bench-cache", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var count int
	var prompt string
	var cwd string
	var cfg providerConfig
	fs.IntVar(&count, "count", 3, "number of identical requests to send")
	fs.StringVar(&prompt, "prompt", defaultCacheBenchmarkPrompt(), "stable prompt repeated for every request")
	fs.StringVar(&cwd, "cwd", "", "working directory used to load provider settings")
	commonProviderFlags(fs, &cfg)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if count < 1 || count > maxCacheBenchmarkRuns {
		fmt.Fprintf(stderr, "escape bench-cache: count must be between 1 and %d\n", maxCacheBenchmarkRuns)
		return 2
	}
	if strings.TrimSpace(prompt) == "" {
		fmt.Fprintln(stderr, "escape bench-cache: prompt cannot be empty")
		return 2
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	set, err := settings.Load(cwd)
	if err != nil {
		fmt.Fprintln(stderr, "escape bench-cache:", err)
		return 1
	}
	if cfg.providerName == "" {
		cfg.providerName = set.DefaultProvider
	}
	if cfg.model == "" {
		cfg.model = set.DefaultModel
	}
	if cfg.thinkingLevel == "" {
		cfg.thinkingLevel = set.DefaultThinkingLevel
	}

	p, err := cfg.provider()
	if err != nil {
		fmt.Fprintln(stderr, "escape bench-cache:", err)
		return 1
	}
	model := cfg.modelOr("unknown")
	if setter, ok := p.(interface{ SetSessionID(string) }); ok {
		setter.SetSessionID(session.NewSessionID())
	}
	fmt.Fprintf(stdout, "provider=%s model=%s runs=%d prompt_chars=%d\n", providerLabel(cfg.providerName), model, count, len(prompt))

	ctx := context.Background()
	var totalInput, totalCacheRead, availableRuns int
	for run := 1; run <= count; run++ {
		usage, err := runCacheBenchmarkRequest(ctx, p, model, prompt, cfg.maxTok)
		if err != nil {
			fmt.Fprintf(stderr, "escape bench-cache: run %d failed: %v\n", run, err)
			return 1
		}
		rate, available := provider.CacheHitRate(usage)
		if available {
			availableRuns++
			totalInput += usage.Input
			totalCacheRead += usage.CacheRead
			fmt.Fprintf(stdout, "run=%d input=%d cache_read=%d cache_hit_rate=%.2f%%\n", run, usage.Input, usage.CacheRead, rate*100)
		} else {
			fmt.Fprintf(stdout, "run=%d input=%d cache_read=unavailable\n", run, usage.Input)
		}
	}

	if availableRuns == 0 {
		fmt.Fprintln(stdout, "cache_hit_rate=unavailable provider did not report cache-read usage")
		return 0
	}
	if totalInput == 0 {
		fmt.Fprintln(stdout, "cache_hit_rate=unavailable provider reported zero prompt tokens")
		return 0
	}
	fmt.Fprintf(stdout, "cache_hit_rate=%.2f%% (%d/%d prompt tokens, %d/%d runs reported cache usage)\n", float64(totalCacheRead)/float64(totalInput)*100, totalCacheRead, totalInput, availableRuns, count)
	return 0
}

func providerLabel(name string) string {
	if name == "" {
		return "auto"
	}
	return name
}

func runCacheBenchmarkRequest(ctx context.Context, p provider.Provider, model, prompt string, maxTokens int) (provider.Usage, error) {
	stream, err := p.Stream(ctx, provider.Request{
		Model:     model,
		MaxTokens: maxTokens,
		Messages: []provider.Message{
			{Role: "system", Text: "You are a coding agent. Answer the user's request."},
			{Role: "user", Text: prompt},
		},
	})
	if err != nil {
		return provider.Usage{}, err
	}
	defer stream.Close()

	var usage provider.Usage
	for {
		event, err := stream.Next()
		if err == io.EOF {
			return usage, nil
		}
		if err != nil {
			return provider.Usage{}, err
		}
		if event.Kind == provider.EventDone {
			usage = event.Usage
		}
	}
}
