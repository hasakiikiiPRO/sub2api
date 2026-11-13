package service

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Two parallel calls to the same tool in one Gemini turn, as sent by
// gemini-4-hydrogen for a bilingual web search.
const geminiParallelSameNameUpstream = `data: {"candidates":[{"content":{"parts":[` +
	`{"functionCall":{"name":"web_search","args":{"query":"黄仁勋 最新新闻"}}},` +
	`{"functionCall":{"name":"web_search","args":{"query":"Jensen Huang news"}}}` +
	`]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3}}` + "\n\n" +
	"data: [DONE]\n\n"

func geminiSSEResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

type streamedToolUse struct {
	name string
	args string
}

// collectAnthropicToolUses rebuilds tool_use blocks from an Anthropic SSE stream.
func collectAnthropicToolUses(t *testing.T, body string) []streamedToolUse {
	t.Helper()
	var out []streamedToolUse
	byIndex := map[int]int{}
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev struct {
			Type         string `json:"type"`
			Index        int    `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
				Name string `json:"name"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev) != nil {
			continue
		}
		switch {
		case ev.Type == "content_block_start" && ev.ContentBlock.Type == "tool_use":
			byIndex[ev.Index] = len(out)
			out = append(out, streamedToolUse{name: ev.ContentBlock.Name})
		case ev.Type == "content_block_delta" && ev.Delta.Type == "input_json_delta":
			i, ok := byIndex[ev.Index]
			require.Truef(t, ok, "input_json_delta for unknown block %d", ev.Index)
			out[i].args += ev.Delta.PartialJSON
		}
	}
	return out
}

func requireSeparateSearchCalls(t *testing.T, calls []streamedToolUse) {
	t.Helper()
	require.Len(t, calls, 2, "parallel same-name calls must not be merged: %+v", calls)
	want := []string{"黄仁勋 最新新闻", "Jensen Huang news"}
	for i, call := range calls {
		require.Equal(t, "web_search", call.name)
		var args map[string]any
		require.NoErrorf(t, json.Unmarshal([]byte(call.args), &args), "call %d args not valid JSON: %q", i, call.args)
		require.Equal(t, want[i], args["query"])
	}
}

func TestGeminiMessagesStreaming_ParallelSameNameToolCallsStaySeparate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	svc := &GeminiMessagesCompatService{}
	_, err := svc.handleStreamingResponse(c, geminiSSEResponse(geminiParallelSameNameUpstream), time.Now(), "gemini-4-hydrogen-aistudio")
	require.NoError(t, err)

	requireSeparateSearchCalls(t, collectAnthropicToolUses(t, rec.Body.String()))
}

// Args streamed for a single call across chunks (cumulative) still form one tool_use.
func TestGeminiMessagesStreaming_CumulativeArgsStayOneBlock(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := `data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"web_search","args":"{\"query\":\"Jen"}}]}}]}` + "\n\n" +
		`data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"web_search","args":"{\"query\":\"Jensen\"}"}}]},"finishReason":"STOP"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	svc := &GeminiMessagesCompatService{}
	_, err := svc.handleStreamingResponse(c, geminiSSEResponse(upstream), time.Now(), "gemini-test")
	require.NoError(t, err)

	calls := collectAnthropicToolUses(t, rec.Body.String())
	require.Len(t, calls, 1)
	require.Equal(t, `{"query":"Jensen"}`, calls[0].args)
}

func TestGeminiChatCompletionsStreaming_ParallelSameNameToolCallsStaySeparate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	svc := &GeminiMessagesCompatService{}
	_, err := svc.handleChatCompletionsStreamingResponseFromGemini(c, geminiSSEResponse(geminiParallelSameNameUpstream), time.Now(), "gemini-4-hydrogen-aistudio", false, false)
	require.NoError(t, err)

	type call struct{ name, args string }
	calls := map[int]*call{}
	sc := bufio.NewScanner(strings.NewReader(rec.Body.String()))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") || strings.TrimSpace(line) == "data: [DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Index    int `json:"index"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk))
		for _, ch := range chunk.Choices {
			for _, tc := range ch.Delta.ToolCalls {
				if calls[tc.Index] == nil {
					calls[tc.Index] = &call{}
				}
				calls[tc.Index].name += tc.Function.Name
				calls[tc.Index].args += tc.Function.Arguments
			}
		}
	}
	got := make([]streamedToolUse, 0, len(calls))
	for i := 0; i < len(calls); i++ {
		require.NotNilf(t, calls[i], "missing tool call index %d in %v", i, calls)
		got = append(got, streamedToolUse{name: calls[i].name, args: calls[i].args})
	}
	requireSeparateSearchCalls(t, got)
}

func TestGeminiFunctionCallStartsNewBlock(t *testing.T) {
	cases := []struct {
		name                           string
		openName, seen, callName, args string
		want                           bool
	}{
		{"different tool", "a", `{"x":1}`, "b", `{"x":1}`, true},
		{"same tool new complete call", "a", `{"x":1}`, "a", `{"x":2}`, true},
		{"same tool repeated chunk", "a", `{"x":1}`, "a", `{"x":1}`, false},
		{"same tool partial continuation", "a", `{"x":`, "a", `{"x":1}`, false},
		{"nothing seen yet", "a", "", "a", `{"x":1}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, geminiFunctionCallStartsNewBlock(tc.openName, tc.seen, tc.callName, tc.args))
		})
	}
}
