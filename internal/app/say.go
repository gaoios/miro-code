package app

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// handleSay is a thin wrapper around weiran-cu's POST /audio/play endpoint.
//
// Why route through weiran-cu and not just call tts-generate.sh + afplay
// directly here:
//   - centralised cache (~/Library/Caches/weiran-tts/) shared across callers
//   - any spawn / sub-session can reach it via plain HTTP without depending
//     on the local weiran binary, the TTS script path, or PATH state
//   - the LaunchAgent runs in the user's audio session so afplay can actually
//     reach the default output device (root daemons can't)
func handleSay(args []string) {
	fs := flag.NewFlagSet("say", flag.ExitOnError)
	fs.Usage = func() {
		w := fs.Output()
		fmt.Fprintf(w, "usage: %s say [flags] <text>\n\n", appName)
		fmt.Fprintln(w, "Generate TTS via IndexTTS and play on this Mac (default audio output).")
		fmt.Fprintln(w, "Routes through weiran-cu /audio/play (cache + async + device handling).")
		fmt.Fprintln(w, "")
		fmt.Fprintln(w, "Flags:")
		fs.PrintDefaults()
	}

	var (
		voice    = fs.String("voice", "", "voice preset: ayaka|weiran (default: server default)")
		category = fs.String("category", "", "raw voice category, e.g. 原神")
		subcat   = fs.String("subcategory", "", "raw voice subcategory, e.g. 神里绫华")
		filename = fs.String("filename", "", "raw reference audio filename")
		emoText  = fs.String("emo", "", "natural-language emotion (IndexTTS2), e.g. 温柔小声")
		emoAlpha = fs.Float64("emo-alpha", 1.0, "emotion strength 0.0-2.0")
		device   = fs.String("device", "", "afplay -d device name (system default if empty)")
		volume   = fs.Float64("volume", -1, "playback volume 0.0-1.0 (negative = afplay default)")
		async    = fs.Bool("async", false, "fire and forget — do not wait for playback to finish")
		noCache  = fs.Bool("no-cache", false, "skip cache lookup and write")
		warm     = fs.Bool("warm", false, "generate + cache, do not play (warm the cache)")
		host     = fs.String("host", "127.0.0.1", "weiran-cu host")
		port     = fs.Int("port", 8105, "weiran-cu port")
		timeout  = fs.Int("timeout", 180, "client HTTP timeout seconds (TTS jobs can take a minute)")
		dryRun   = fs.Bool("dry-run", false, "print the request payload, do not send")
		listDev  = fs.Bool("list-devices", false, "list available audio output devices and exit")
		showCach = fs.Bool("show-cache", false, "show cache stats and exit")
		clearCac = fs.Bool("clear-cache", false, "clear the TTS cache and exit")
	)
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	base := fmt.Sprintf("http://%s:%d", *host, *port)
	client := &http.Client{Timeout: time.Duration(*timeout) * time.Second}

	if *listDev {
		mustGetJSON(client, base+"/audio/devices", os.Stdout)
		return
	}
	if *showCach {
		mustGetJSON(client, base+"/audio/cache", os.Stdout)
		return
	}
	if *clearCac {
		req, _ := http.NewRequest("POST", base+"/audio/cache/clear", nil)
		mustDoJSON(client, req, os.Stdout)
		return
	}

	text := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if text == "" {
		// Fallback: read from stdin if piped (e.g. `echo 'hi' | weiran say`).
		stat, _ := os.Stdin.Stat()
		if (stat.Mode() & os.ModeCharDevice) == 0 {
			b, _ := io.ReadAll(os.Stdin)
			text = strings.TrimSpace(string(b))
		}
	}
	if text == "" {
		fs.Usage()
		os.Exit(1)
	}

	payload := map[string]any{"text": text}
	if *voice != "" {
		payload["voice"] = *voice
	}
	if *category != "" {
		payload["category"] = *category
	}
	if *subcat != "" {
		payload["subcategory"] = *subcat
	}
	if *filename != "" {
		payload["filename"] = *filename
	}
	if *emoText != "" {
		payload["emo_text"] = *emoText
	}
	if *emoAlpha != 1.0 {
		payload["emo_alpha"] = *emoAlpha
	}
	if *device != "" {
		payload["device"] = *device
	}
	if *volume >= 0 {
		payload["volume"] = *volume
	}
	if *async {
		payload["async"] = true
	}
	if *noCache {
		payload["no_cache"] = true
	}
	if *warm {
		payload["cache_only"] = true
	}

	body, _ := json.Marshal(payload)
	if *dryRun {
		fmt.Println(string(body))
		return
	}

	req, err := http.NewRequest("POST", base+"/audio/play", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "build request: %v\n", err)
		os.Exit(1)
	}
	req.Header.Set("Content-Type", "application/json")
	mustDoJSON(client, req, os.Stdout)
}

func mustGetJSON(client *http.Client, url string, w io.Writer) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "GET %s: build request: %v\n", url, err)
		os.Exit(1)
	}
	mustDoJSON(client, req, w)
}

func mustDoJSON(client *http.Client, req *http.Request, w io.Writer) {
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s %s: %v\n", req.Method, req.URL, err)
		fmt.Fprintf(os.Stderr, "Hint: is weiran-cu running? `make -C %s/projects/weiran-cu status`\n", workspace)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	// Pretty-print if JSON, otherwise dump raw.
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, body, "", "  "); err == nil {
		fmt.Fprintln(w, pretty.String())
	} else {
		fmt.Fprintln(w, string(body))
	}
	if resp.StatusCode >= 400 {
		os.Exit(1)
	}
}
