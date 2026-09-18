package app

import (
	"os"
	"strings"
	"testing"
)

func TestSegmentText_Basic(t *testing.T) {
	cases := []struct {
		name  string
		input string
		check func(string) bool // return true if output is acceptable
		desc  string            // what the check verifies
	}{
		{
			name:  "empty",
			input: "",
			check: func(s string) bool { return s == "" },
			desc:  "empty string returns empty",
		},
		{
			name:  "english only",
			input: "FTS5 full text search",
			check: func(s string) bool { return s == "FTS5 full text search" },
			desc:  "english passes through unchanged",
		},
		{
			name:  "chinese segmentation",
			input: "\u5fc3\u8df3\u5de1\u68c0\u670d\u52a1\u6b63\u5e38", // 心跳巡检服务正常
			check: func(s string) bool {
				// Should be segmented into words, not single chars
				words := strings.Fields(s)
				// Must have fewer tokens than characters (7 chars -> should be ~3-4 words)
				return len(words) < 7 && len(words) >= 2
			},
			desc: "chinese text segmented into words (fewer tokens than characters)",
		},
		{
			name:  "mixed chinese english",
			input: "FTS5 \u4e2d\u6587\u5206\u8bcd", // FTS5 中文分词
			check: func(s string) bool {
				return strings.Contains(s, "FTS5") && strings.Contains(s, "\u4e2d\u6587") // 中文
			},
			desc: "mixed text preserves both english and chinese words",
		},
		{
			name:  "preserves newlines",
			input: "\u7b2c\u4e00\u884c\u5185\u5bb9\n\u7b2c\u4e8c\u884c\u5185\u5bb9", // 第一行内容\n第二行内容
			check: func(s string) bool { return strings.Contains(s, "\n") },
			desc:  "newline structure preserved",
		},
		{
			name:  "heartbeat example",
			input: "\u5fc3\u8df3\u5de1\u68c0 #300, \u670d\u52a1\u5168\u90e8\u5065\u5eb7, backlog \u6e05\u96f6",
			// 心跳巡检 #300, 服务全部健康, backlog 清零
			check: func(s string) bool {
				return strings.Contains(s, "\u5fc3\u8df3") && // 心跳
					strings.Contains(s, "#300") &&
					strings.Contains(s, "backlog")
			},
			desc: "real heartbeat text segments correctly",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := segmentText(c.input)
			if !c.check(got) {
				t.Errorf("segmentText(%q) = %q -- failed check: %s", c.input, got, c.desc)
			}
		})
	}
}

func TestSegmentText_ChineseWordBoundaries(t *testing.T) {
	// These specific words should appear as whole tokens after segmentation
	// 心跳巡检 -> should segment into 心跳 + 巡检
	input := "\u5fc3\u8df3\u5de1\u68c0"
	result := segmentText(input)
	t.Logf("input: %q, result: %q", input, result)
	words := strings.Fields(result)
	t.Logf("words: %v (count=%d)", words, len(words))

	xintiao := "\u5fc3\u8df3" // 心跳
	xunjian := "\u5de1\u68c0" // 巡检

	foundXintiao := false
	foundXunjian := false
	for _, w := range words {
		if w == xintiao {
			foundXintiao = true
		}
		if w == xunjian {
			foundXunjian = true
		}
	}
	if !foundXintiao {
		t.Errorf("expected %q as whole word in result %q (words: %v)", xintiao, result, words)
	}
	if !foundXunjian {
		t.Errorf("expected %q as whole word in result %q (words: %v)", xunjian, result, words)
	}
}

func TestSearchFTS_ChineseSegmented(t *testing.T) {
	defer setupFTSTest(t)()

	memDir := strings.TrimRight(workspace, "/") + "/memory/daily"

	// 心跳巡检 #200, 服务全部健康, backlog 清零.
	writeTestFile(t, memDir+"/2026-04-01.md",
		"\u5fc3\u8df3\u5de1\u68c0 #200, \u670d\u52a1\u5168\u90e8\u5065\u5eb7, backlog \u6e05\u96f6.")
	// ComfyUI 离线了, selfie 功能不可用.
	writeTestFile(t, memDir+"/2026-04-02.md",
		"ComfyUI \u79bb\u7ebf\u4e86, selfie \u529f\u80fd\u4e0d\u53ef\u7528.")
	// 未然的记忆系统优化, FTS5 中文分词改造完成.
	writeTestFile(t, memDir+"/2026-04-03.md",
		"\u672a\u7136\u7684\u8bb0\u5fc6\u7cfb\u7edf\u4f18\u5316, FTS5 \u4e2d\u6587\u5206\u8bcd\u6539\u9020\u5b8c\u6210.")

	if _, _, err := indexDailyNotes(); err != nil {
		t.Fatalf("index: %v", err)
	}

	// Test: "巡检" should match day 1 (whole word match)
	hits, err := searchFTS("\u5de1\u68c0", "daily", 10) // 巡检
	if err != nil {
		t.Fatalf("search xunjian: %v", err)
	}
	if len(hits) != 1 || hits[0].Date != "2026-04-01" {
		t.Errorf("xunjian search: expected 1 hit on 04-01, got %+v", hits)
	}

	// Test: "心跳巡检" should match day 1
	hits, err = searchFTS("\u5fc3\u8df3\u5de1\u68c0", "daily", 10) // 心跳巡检
	if err != nil {
		t.Fatalf("search xintiaoXunjian: %v", err)
	}
	if len(hits) != 1 || hits[0].Date != "2026-04-01" {
		t.Errorf("xintiaoXunjian search: expected 1 hit on 04-01, got %+v", hits)
	}

	// Test: "ComfyUI 离线" should match day 2
	hits, err = searchFTS("ComfyUI \u79bb\u7ebf", "daily", 10) // ComfyUI 离线
	if err != nil {
		t.Fatalf("search comfyui lixian: %v", err)
	}
	if len(hits) != 1 || hits[0].Date != "2026-04-02" {
		t.Errorf("ComfyUI lixian search: expected 1 hit on 04-02, got %+v", hits)
	}

	// Test: "分词" should match day 3
	hits, err = searchFTS("\u5206\u8bcd", "daily", 10) // 分词
	if err != nil {
		t.Fatalf("search fenci: %v", err)
	}
	if len(hits) != 1 || hits[0].Date != "2026-04-03" {
		t.Errorf("fenci search: expected 1 hit on 04-03, got %+v", hits)
	}

	// Test: English still works
	hits, err = searchFTS("FTS5", "daily", 10)
	if err != nil {
		t.Fatalf("search FTS5: %v", err)
	}
	if len(hits) != 1 || hits[0].Date != "2026-04-03" {
		t.Errorf("FTS5 search: expected 1 hit on 04-03, got %+v", hits)
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}
