package tools

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

const webSearchResults = 8

var resultPattern = regexp.MustCompile(`<a rel="nofollow" class="result__a" href="([^"]+)"[^>]*>(.*?)</a>`)

func WebSearch() Tool {
	return Tool{
		Name:         "web_search",
		ParallelSafe: true,
		Description:  "Search the public web and return ranked result titles and URLs.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "Search query"},
			},
			"required": []string{"query"},
		},
		Run: func(ctx context.Context, args map[string]any) Result {
			var p struct {
				Query string `json:"query"`
			}
			if err := decodeArgs(args, &p); err != nil {
				return errResult(err)
			}
			p.Query = strings.TrimSpace(p.Query)
			if p.Query == "" {
				return errResult(fmt.Errorf("search query is required"))
			}
			return webSearch(ctx, http.DefaultClient, p.Query)
		},
	}
}

func webSearch(ctx context.Context, client *http.Client, query string) Result {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://html.duckduckgo.com/html/?q="+url.QueryEscape(query), nil)
	if err != nil {
		return errResult(err)
	}
	req.Header.Set("User-Agent", "escape/0.1")
	resp, err := client.Do(req)
	if err != nil {
		return errResult(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Result{Output: fmt.Sprintf("web search failed: HTTP %s", resp.Status), IsError: true}
	}
	body, err := readAllLimited(resp.Body, 2<<20)
	if err != nil {
		return errResult(err)
	}
	matches := resultPattern.FindAllStringSubmatch(string(body), webSearchResults)
	if len(matches) == 0 {
		return Result{Output: "No web results found."}
	}
	var out strings.Builder
	for i, m := range matches {
		link, err := url.Parse(html.UnescapeString(m[1]))
		if err != nil {
			continue
		}
		if link.Host == "duckduckgo.com" {
			link = resolveDuckDuckGoLink(link)
		}
		title := strings.Join(regexp.MustCompile(`<[^>]+>`).FindAllString(html.UnescapeString(m[2]), -1), "")
		title = strings.Join(strings.Fields(title), " ")
		if title != "" {
			fmt.Fprintf(&out, "%d. %s\n%s\n", i+1, title, link.String())
		}
	}
	if out.Len() == 0 {
		return Result{Output: "No web results found."}
	}
	return Result{Output: strings.TrimSpace(out.String())}
}

func resolveDuckDuckGoLink(link *url.URL) *url.URL {
	if raw := link.Query().Get("uddg"); raw != "" {
		if target, err := url.Parse(raw); err == nil {
			return target
		}
	}
	return link
}

func readAllLimited(r interface{ Read([]byte) (int, error) }, limit int) ([]byte, error) {
	data := make([]byte, 0, 4096)
	buf := make([]byte, 4096)
	for len(data) < limit {
		n, err := r.Read(buf)
		data = append(data, buf[:n]...)
		if err != nil {
			if err == io.EOF {
				return data, nil
			}
			return nil, err
		}
	}
	return data[:limit], nil
}
