package app

import (
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ── Link Preview (OG Tags) ──

type ogData struct {
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Image       string `json:"image,omitempty"`
	SiteName    string `json:"site_name,omitempty"`
}

var (
	ogCache   = make(map[string]*ogData)
	ogCacheMu sync.RWMutex
	ogClient  = &http.Client{
		Timeout: 8 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}

	reOGTitle = regexp.MustCompile(`(?i)<meta\s+(?:property|name)=["']og:title["']\s+content=["']([^"']+)["']`)
	reOGDesc  = regexp.MustCompile(`(?i)<meta\s+(?:property|name)=["']og:description["']\s+content=["']([^"']+)["']`)
	reOGImage = regexp.MustCompile(`(?i)<meta\s+(?:property|name)=["']og:image["']\s+content=["']([^"']+)["']`)
	reOGSite  = regexp.MustCompile(`(?i)<meta\s+(?:property|name)=["']og:site_name["']\s+content=["']([^"']+)["']`)
	reTitle   = regexp.MustCompile(`(?is)<title[^>]*>([^<]+)</title>`)

	// Also match content before property (common alternative ordering)
	reOGTitleAlt = regexp.MustCompile(`(?i)<meta\s+content=["']([^"']+)["']\s+(?:property|name)=["']og:title["']`)
	reOGDescAlt  = regexp.MustCompile(`(?i)<meta\s+content=["']([^"']+)["']\s+(?:property|name)=["']og:description["']`)
	reOGImageAlt = regexp.MustCompile(`(?i)<meta\s+content=["']([^"']+)["']\s+(?:property|name)=["']og:image["']`)
	reOGSiteAlt  = regexp.MustCompile(`(?i)<meta\s+content=["']([^"']+)["']\s+(?:property|name)=["']og:site_name["']`)

	// Fallback: meta description
	reMetaDesc    = regexp.MustCompile(`(?i)<meta\s+name=["']description["']\s+content=["']([^"']+)["']`)
	reMetaDescAlt = regexp.MustCompile(`(?i)<meta\s+content=["']([^"']+)["']\s+name=["']description["']`)
)

func fetchOGTags(url string) *ogData {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return nil
	}

	ogCacheMu.RLock()
	if cached, ok := ogCache[url]; ok {
		ogCacheMu.RUnlock()
		return cached
	}
	ogCacheMu.RUnlock()

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", fmt.Sprintf("Mozilla/5.0 (compatible; %sBot/1.0; +https://github.com/kiyor/soul-cli)", appName))
	req.Header.Set("Accept", "text/html")

	resp, err := ogClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/html") && !strings.Contains(ct, "application/xhtml") {
		return nil
	}

	// Read limited body (first 64KB should contain all meta tags)
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil
	}
	html := string(body)

	data := &ogData{}

	// Extract OG tags (try both property-first and content-first patterns)
	if m := reOGTitle.FindStringSubmatch(html); len(m) > 1 {
		data.Title = unescapeHTML(m[1])
	} else if m := reOGTitleAlt.FindStringSubmatch(html); len(m) > 1 {
		data.Title = unescapeHTML(m[1])
	}

	if m := reOGDesc.FindStringSubmatch(html); len(m) > 1 {
		data.Description = unescapeHTML(m[1])
	} else if m := reOGDescAlt.FindStringSubmatch(html); len(m) > 1 {
		data.Description = unescapeHTML(m[1])
	} else if m := reMetaDesc.FindStringSubmatch(html); len(m) > 1 {
		data.Description = unescapeHTML(m[1])
	} else if m := reMetaDescAlt.FindStringSubmatch(html); len(m) > 1 {
		data.Description = unescapeHTML(m[1])
	}

	if m := reOGImage.FindStringSubmatch(html); len(m) > 1 {
		data.Image = unescapeHTML(m[1])
	} else if m := reOGImageAlt.FindStringSubmatch(html); len(m) > 1 {
		data.Image = unescapeHTML(m[1])
	}

	if m := reOGSite.FindStringSubmatch(html); len(m) > 1 {
		data.SiteName = unescapeHTML(m[1])
	} else if m := reOGSiteAlt.FindStringSubmatch(html); len(m) > 1 {
		data.SiteName = unescapeHTML(m[1])
	}

	// Fallback: use <title> if no og:title
	if data.Title == "" {
		if m := reTitle.FindStringSubmatch(html); len(m) > 1 {
			data.Title = strings.TrimSpace(unescapeHTML(m[1]))
		}
	}

	if data.Title == "" {
		return nil // no useful data
	}

	// Truncate
	if len(data.Title) > 120 {
		data.Title = data.Title[:120] + "…"
	}
	if len(data.Description) > 200 {
		data.Description = data.Description[:200] + "…"
	}

	ogCacheMu.Lock()
	ogCache[url] = data
	// Simple eviction: cap at 500
	if len(ogCache) > 500 {
		for k := range ogCache {
			delete(ogCache, k)
			break
		}
	}
	ogCacheMu.Unlock()

	return data
}

func unescapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&quot;", "\"")
	s = strings.ReplaceAll(s, "&#39;", "'")
	s = strings.ReplaceAll(s, "&#x27;", "'")
	s = strings.ReplaceAll(s, "&apos;", "'")
	return s
}
