package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ── Telegram Notify ──

// disableTelegram is set to true in tests to prevent real Telegram API calls.
var disableTelegram bool

var cachedTelegramToken string

func getTelegramToken() string {
	if cachedTelegramToken != "" {
		return cachedTelegramToken
	}
	// 1. config.json server.telegram.botToken
	if cfgData, err := os.ReadFile(filepath.Join(appDir, "config.json")); err == nil {
		var root struct {
			Server struct {
				Telegram struct {
					BotToken string `json:"botToken"`
				} `json:"telegram"`
			} `json:"server"`
		}
		if json.Unmarshal(cfgData, &root) == nil && root.Server.Telegram.BotToken != "" {
			cachedTelegramToken = root.Server.Telegram.BotToken
			return cachedTelegramToken
		}
	}
	return ""
}

func sendTelegramPhoto(photoURL, caption string) {
	if err := trySendTelegramPhoto(photoURL, caption); err != nil {
		fmt.Fprintf(os.Stderr, "["+appName+"]%v\n", err)
		os.Exit(1)
	}
	fmt.Println("photo sent")
}

func sendTelegram(text string) {
	if err := trySendTelegram(text); err != nil {
		fmt.Fprintf(os.Stderr, "["+appName+"]%v\n", err)
		os.Exit(1)
	}
	fmt.Println("sent")
}

// trySendTelegram sends a text message, returns error instead of os.Exit (hook-safe)
// tries Markdown first, falls back to plain text on failure
func trySendTelegram(text string) error {
	if disableTelegram {
		return nil
	}
	token := getTelegramToken()
	if token == "" {
		return fmt.Errorf("Telegram bot token not found")
	}

	preheatChatAction("text")

	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", token)

	// try Markdown first
	resp, err := http.PostForm(apiURL, url.Values{
		"chat_id":    {tgChatID},
		"text":       {text},
		"parse_mode": {"Markdown"},
	})
	if err != nil {
		return fmt.Errorf("send failed: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	json.Unmarshal(body, &result)

	if result.OK {
		return nil
	}

	// Markdown failed, fallback to plain text (drop parse_mode)
	resp, err = http.PostForm(apiURL, url.Values{
		"chat_id": {tgChatID},
		"text":    {text},
	})
	if err != nil {
		return fmt.Errorf("send failed (fallback): %w", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	json.Unmarshal(body, &result)
	if !result.OK {
		return fmt.Errorf("Telegram API error: %s", result.Description)
	}
	return nil
}

// sendTelegramVoice posts an OGG/Opus file as a voice message and exits on
// failure (CLI-style). Use trySendTelegramVoice from non-CLI callers.
func sendTelegramVoice(oggPath, caption string) {
	if err := trySendTelegramVoice(oggPath, caption); err != nil {
		fmt.Fprintf(os.Stderr, "["+appName+"]%v\n", err)
		os.Exit(1)
	}
	fmt.Println("voice sent")
}

// trySendTelegramVoice posts an audio file as a Telegram voice message via
// sendVoice multipart upload. The file should already be OGG/Opus mono — TG
// will reject other containers as "voice".
func trySendTelegramVoice(oggPath, caption string) error {
	if disableTelegram {
		return nil
	}
	token := getTelegramToken()
	if token == "" {
		return fmt.Errorf("Telegram bot token not found")
	}

	preheatChatAction("voice")

	f, err := os.Open(oggPath)
	if err != nil {
		return fmt.Errorf("open voice file: %w", err)
	}
	defer f.Close()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("chat_id", tgChatID); err != nil {
		return fmt.Errorf("write chat_id: %w", err)
	}
	if caption != "" {
		if err := mw.WriteField("caption", caption); err != nil {
			return fmt.Errorf("write caption: %w", err)
		}
	}
	fw, err := mw.CreateFormFile("voice", filepath.Base(oggPath))
	if err != nil {
		return fmt.Errorf("create form file: %w", err)
	}
	if _, err := io.Copy(fw, f); err != nil {
		return fmt.Errorf("copy voice bytes: %w", err)
	}
	if err := mw.Close(); err != nil {
		return fmt.Errorf("close multipart: %w", err)
	}

	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendVoice", token)
	req, err := http.NewRequest("POST", apiURL, &buf)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("voice send failed: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	json.Unmarshal(body, &result)
	if !result.OK {
		return fmt.Errorf("Telegram API error: %s", result.Description)
	}
	return nil
}

// trySendTelegramPhoto sends a photo, returns error instead of os.Exit
// tries Markdown first, falls back to plain text on failure
func trySendTelegramPhoto(photoURL, caption string) error {
	if disableTelegram {
		return nil
	}
	token := getTelegramToken()
	if token == "" {
		return fmt.Errorf("Telegram bot token not found")
	}

	preheatChatAction("photo")

	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendPhoto", token)
	resp, err := http.PostForm(apiURL, url.Values{
		"chat_id":    {tgChatID},
		"photo":      {photoURL},
		"caption":    {caption},
		"parse_mode": {"Markdown"},
	})
	if err != nil {
		return fmt.Errorf("photo send failed: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	json.Unmarshal(body, &result)

	if result.OK {
		return nil
	}

	// Markdown failed, fallback to plain text
	resp, err = http.PostForm(apiURL, url.Values{
		"chat_id": {tgChatID},
		"photo":   {photoURL},
		"caption": {caption},
	})
	if err != nil {
		return fmt.Errorf("photo send failed (fallback): %w", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	json.Unmarshal(body, &result)
	if !result.OK {
		return fmt.Errorf("Telegram API error: %s", result.Description)
	}
	return nil
}

// ── Extended Telegram Methods (2026-04-28) ──
// 添加了：ChatAction(随机可爱版), MediaGroup(图册),
// editMessageText(动态进度条), Inline Keyboard.

// disableChatAction 测试或静音预热时设为 true。
var disableChatAction bool

// chatActionForKind 把消息类型映射到 TG ChatAction，文本类偶尔切 choose_sticker。
func chatActionForKind(kind string) string {
	if kind == "text" && rand.Intn(20) == 0 {
		return "choose_sticker"
	}
	switch kind {
	case "text":
		return "typing"
	case "photo", "album":
		return "upload_photo"
	case "voice":
		return "record_voice"
	case "document":
		return "upload_document"
	case "video", "gif":
		return "upload_video"
	default:
		return "typing"
	}
}

func trySendTelegramChatAction(action string) error {
	if disableTelegram {
		return nil
	}
	token := getTelegramToken()
	if token == "" {
		return fmt.Errorf("Telegram bot token not found")
	}
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendChatAction", token)
	resp, err := http.PostForm(apiURL, url.Values{
		"chat_id": {tgChatID},
		"action":  {action},
	})
	if err != nil {
		return fmt.Errorf("chatAction failed: %w", err)
	}
	resp.Body.Close()
	return nil
}

// preheatChatAction 发 action + 随机 sleep。WEIRAN_NO_CHAT_ACTION=1 关闭。
func preheatChatAction(kind string) {
	if disableChatAction || os.Getenv("WEIRAN_NO_CHAT_ACTION") == "1" {
		return
	}
	action := chatActionForKind(kind)
	_ = trySendTelegramChatAction(action)
	var lo, hi int
	switch kind {
	case "text":
		lo, hi = 400, 1100
	case "photo", "voice", "document", "gif", "video":
		lo, hi = 700, 1500
	case "album":
		lo, hi = 900, 1700
	default:
		lo, hi = 500, 1200
	}
	time.Sleep(time.Duration(lo+rand.Intn(hi-lo)) * time.Millisecond)
}

// ── sendMediaGroup（图册，2-10 张一组）──

type mediaGroupItem struct {
	Type    string `json:"type"`
	Media   string `json:"media"`
	Caption string `json:"caption,omitempty"`
}

func sendTelegramAlbum(urls []string, caption string) {
	if err := trySendTelegramAlbum(urls, caption); err != nil {
		fmt.Fprintf(os.Stderr, "["+appName+"]%v\n", err)
		os.Exit(1)
	}
	fmt.Println("album sent")
}

func trySendTelegramAlbum(urls []string, caption string) error {
	if disableTelegram {
		return nil
	}
	if len(urls) < 2 || len(urls) > 10 {
		return fmt.Errorf("album needs 2-10 photos, got %d", len(urls))
	}
	token := getTelegramToken()
	if token == "" {
		return fmt.Errorf("Telegram bot token not found")
	}
	preheatChatAction("album")

	items := make([]mediaGroupItem, 0, len(urls))
	for i, u := range urls {
		it := mediaGroupItem{Type: "photo", Media: u}
		if i == 0 && caption != "" {
			it.Caption = caption
		}
		items = append(items, it)
	}
	mediaJSON, err := json.Marshal(items)
	if err != nil {
		return fmt.Errorf("marshal media: %w", err)
	}
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMediaGroup", token)
	resp, err := http.PostForm(apiURL, url.Values{
		"chat_id": {tgChatID},
		"media":   {string(mediaJSON)},
	})
	if err != nil {
		return fmt.Errorf("album send failed: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	json.Unmarshal(body, &result)
	if !result.OK {
		return fmt.Errorf("Telegram API error: %s", result.Description)
	}
	return nil
}

// ── editMessageText（动态进度条）──

func editTelegramMessage(messageID int64, text string) {
	if err := tryEditTelegramMessage(messageID, text); err != nil {
		fmt.Fprintf(os.Stderr, "["+appName+"]%v\n", err)
		os.Exit(1)
	}
	fmt.Println("edited")
}

func tryEditTelegramMessage(messageID int64, text string) error {
	if disableTelegram {
		return nil
	}
	token := getTelegramToken()
	if token == "" {
		return fmt.Errorf("Telegram bot token not found")
	}
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/editMessageText", token)
	resp, err := http.PostForm(apiURL, url.Values{
		"chat_id":    {tgChatID},
		"message_id": {fmt.Sprintf("%d", messageID)},
		"text":       {text},
		"parse_mode": {"Markdown"},
	})
	if err != nil {
		return fmt.Errorf("edit failed: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	json.Unmarshal(body, &result)
	if result.OK || strings.Contains(result.Description, "not modified") {
		return nil
	}
	resp, err = http.PostForm(apiURL, url.Values{
		"chat_id":    {tgChatID},
		"message_id": {fmt.Sprintf("%d", messageID)},
		"text":       {text},
	})
	if err != nil {
		return fmt.Errorf("edit failed (fallback): %w", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	json.Unmarshal(body, &result)
	if !result.OK && !strings.Contains(result.Description, "not modified") {
		return fmt.Errorf("Telegram API error: %s", result.Description)
	}
	return nil
}

// ── Inline Keyboard ──

type inlineButton struct {
	Text         string `json:"text"`
	URL          string `json:"url,omitempty"`
	CallbackData string `json:"callback_data,omitempty"`
}

func sendTelegramKeyboard(text string, rows [][]inlineButton) {
	id, err := trySendTelegramKeyboard(text, rows)
	if err != nil {
		fmt.Fprintf(os.Stderr, "["+appName+"]%v\n", err)
		os.Exit(1)
	}
	fmt.Printf("keyboard sent (message_id=%d)\n", id)
}

func trySendTelegramKeyboard(text string, rows [][]inlineButton) (int64, error) {
	if disableTelegram {
		return 0, nil
	}
	token := getTelegramToken()
	if token == "" {
		return 0, fmt.Errorf("Telegram bot token not found")
	}
	preheatChatAction("text")

	rm := map[string]interface{}{"inline_keyboard": rows}
	rmJSON, _ := json.Marshal(rm)
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", token)
	resp, err := http.PostForm(apiURL, url.Values{
		"chat_id":      {tgChatID},
		"text":         {text},
		"parse_mode":   {"Markdown"},
		"reply_markup": {string(rmJSON)},
	})
	if err != nil {
		return 0, fmt.Errorf("keyboard send failed: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		Result      struct {
			MessageID int64 `json:"message_id"`
		} `json:"result"`
	}
	json.Unmarshal(body, &result)
	if result.OK {
		return result.Result.MessageID, nil
	}
	resp, err = http.PostForm(apiURL, url.Values{
		"chat_id":      {tgChatID},
		"text":         {text},
		"reply_markup": {string(rmJSON)},
	})
	if err != nil {
		return 0, fmt.Errorf("keyboard send failed (fallback): %w", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	json.Unmarshal(body, &result)
	if !result.OK {
		return 0, fmt.Errorf("Telegram API error: %s", result.Description)
	}
	return result.Result.MessageID, nil
}

// parseButtonsArg 解析 "label1=https://...,label2=cb:foo,/n,label3=cb:bar"
// 用 "," 分按钮；"/n" 表示换行。label=value：value 以 http(s):// 开头当 URL，否则 callback_data。
//
// LIMITATION: split 用裸 "," 因为 inline keyboard 在 weiran 内部场景里 URL/callback
// 很少含逗号；如果你需要传 URL 含逗号（OAuth callback、含 ?q=a,b 的查询串等），
// 这里会把 URL 切掉。短期对策：URL-encode 成 %2C；长期要换成支持转义的 parser。
func parseButtonsArg(spec string) [][]inlineButton {
	if spec == "" {
		return nil
	}
	var rows [][]inlineButton
	var cur []inlineButton
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if part == "/n" || part == "\\n" {
			if len(cur) > 0 {
				rows = append(rows, cur)
				cur = nil
			}
			continue
		}
		eq := strings.Index(part, "=")
		if eq < 0 {
			cur = append(cur, inlineButton{Text: part, CallbackData: part})
			continue
		}
		label := strings.TrimSpace(part[:eq])
		value := strings.TrimSpace(part[eq+1:])
		btn := inlineButton{Text: label}
		if strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://") {
			btn.URL = value
		} else {
			btn.CallbackData = value
		}
		cur = append(cur, btn)
	}
	if len(cur) > 0 {
		rows = append(rows, cur)
	}
	return rows
}
