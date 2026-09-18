package app

import (
	"strings"
	"sync"
	"unicode"

	"github.com/go-ego/gse"
)

// Chinese text segmentation for FTS5 indexing.
// Uses gse (pure Go, no CGO) to segment Chinese text into words,
// replacing the default unicode61 single-character tokenization.
//
// Strategy: pre-segment text before inserting into FTS5 base tables.
// Each Chinese word becomes a space-separated token that FTS5's default
// tokenizer treats as a whole unit. English/numbers pass through unchanged.

var (
	seg     gse.Segmenter
	segOnce sync.Once
)

// initSegmenter loads the gse dictionary (embedded, ~12MB). Called once lazily.
func initSegmenter() {
	segOnce.Do(func() {
		// Load embedded dictionary (zh). This takes ~200ms on first call.
		seg.LoadDict()
	})
}

// segmentText performs Chinese word segmentation on the input text.
// Chinese characters are segmented into words separated by spaces.
// Non-Chinese text (English, numbers, punctuation) passes through unchanged.
// Newlines and paragraph structure are preserved.
//
// Example:
//
//	"心跳巡检服务正常" → "心跳 巡检 服务 正常"
//	"FTS5 中文分词" → "FTS5 中文 分词"
func segmentText(text string) string {
	if text == "" {
		return ""
	}
	initSegmenter()

	// Process line by line to preserve structure
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if line == "" {
			continue
		}
		// Only segment if line contains CJK characters
		if !containsCJK(line) {
			continue
		}
		lines[i] = segmentLine(line)
	}
	return strings.Join(lines, "\n")
}

// segmentLine segments a single line of text.
// Splits into CJK and non-CJK runs, segments CJK runs with gse,
// and preserves non-CJK runs as-is.
func segmentLine(line string) string {
	runes := []rune(line)
	var result strings.Builder
	result.Grow(len(line) * 2) // rough estimate

	i := 0
	for i < len(runes) {
		if isCJK(runes[i]) {
			// Collect contiguous CJK run (including CJK punctuation mixed in)
			j := i
			for j < len(runes) && (isCJK(runes[j]) || isCJKPunct(runes[j])) {
				j++
			}
			chunk := string(runes[i:j])
			// Segment with gse in search mode — produces both compound words
			// and their sub-words for better recall. E.g. "心跳巡检" →
			// ["心跳", "巡检", "心跳巡检"] so both partial and full matches work.
			words := seg.CutSearch(chunk, true)
			for k, w := range words {
				w = strings.TrimSpace(w)
				if w == "" {
					continue
				}
				if k > 0 {
					result.WriteByte(' ')
				} else if result.Len() > 0 {
					// Ensure space between preceding non-CJK text and first CJK word
					s := result.String()
					if s[len(s)-1] != ' ' {
						result.WriteByte(' ')
					}
				}
				result.WriteString(w)
			}
			i = j
		} else {
			// Non-CJK: pass through as-is
			result.WriteRune(runes[i])
			i++
		}
	}
	return result.String()
}

// containsCJK returns true if the string contains any CJK character.
func containsCJK(s string) bool {
	for _, r := range s {
		if isCJK(r) {
			return true
		}
	}
	return false
}

// isCJK returns true for CJK Unified Ideographs and common extensions.
func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r)
}

// isCJKPunct returns true for common CJK punctuation that should stay
// attached to surrounding CJK text during segmentation.
func isCJKPunct(r rune) bool {
	switch r {
	case '\u3002', '\uff0c', '\u3001', '\uff01', '\uff1f', '\uff1a', '\uff1b', // 。，、！？：；
		'\uff08', '\uff09', '\u3010', '\u3011', '\u300a', '\u300b', // （）【】《》
		'\u201c', '\u201d', '\u2018', '\u2019', '\u2014', '\u2026': // ""''—…
		return true
	}
	return false
}
