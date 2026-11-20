package antigravity

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// claudeCodeTurns 复刻 Claude Code 2.1.x 实抓的 messages 形态：首轮 user 后跟 # Environment
// system 块，此后每轮 user 后追加一条 <total_tokens> system 消息，历史原样重发。
func claudeCodeTurns(turns int) []ClaudeMessage {
	msgs := []ClaudeMessage{
		{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"<system-reminder>ctx</system-reminder>"},{"type":"text","text":"turn 1"}]`)},
		{Role: "system", Content: json.RawMessage(`"# Environment\ncwd=/tmp\nDual-use security tools (C2 frameworks) require clear authorization context: pentesting engagements. tail"`)},
	}
	for i := 2; i <= turns; i++ {
		msgs = append(msgs,
			ClaudeMessage{Role: "assistant", Content: json.RawMessage(`[{"type":"text","text":"ok"}]`)},
			ClaudeMessage{Role: "user", Content: json.RawMessage(`"turn ` + string(rune('0'+i)) + `"`)},
			ClaudeMessage{Role: "system", Content: json.RawMessage(`"<total_tokens>15000000 tokens left</total_tokens>"`)},
		)
	}
	return msgs
}

func transformForPrefix(t *testing.T, msgs []ClaudeMessage) V1InternalRequest {
	t.Helper()
	body, err := TransformClaudeToGeminiWithOptions(&ClaudeRequest{
		Model:  "gemini-4-neon",
		System: json.RawMessage(`[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."},{"type":"text","text":"rules"}]`),
		Tools: []ClaudeTool{{
			Name: "SendMessage",
			InputSchema: map[string]any{
				"type": "object",
				"allOf": []any{
					map[string]any{"required": []any{"zeta", "alpha", "mid", "beta"}, "properties": map[string]any{"zeta": map[string]any{"type": "string"}, "alpha": map[string]any{"type": "string"}, "mid": map[string]any{"type": "string"}, "beta": map[string]any{"type": "string"}}},
				},
			},
		}},
		Messages: msgs,
	}, "project-1", "gemini-4-neon", DefaultTransformOptions())
	require.NoError(t, err)
	var req V1InternalRequest
	require.NoError(t, json.Unmarshal(body, &req))
	return req
}

func TestTransform_MidConversationSystemKeepsCachePrefixStable(t *testing.T) {
	prev := transformForPrefix(t, claudeCodeTurns(2))
	for turns := 3; turns <= 5; turns++ {
		cur := transformForPrefix(t, claudeCodeTurns(turns))

		prevSys, _ := json.Marshal(prev.Request.SystemInstruction)
		curSys, _ := json.Marshal(cur.Request.SystemInstruction)
		require.Equal(t, string(prevSys), string(curSys), "systemInstruction 不得随轮次变化")

		prevTools, _ := json.Marshal(prev.Request.Tools)
		curTools, _ := json.Marshal(cur.Request.Tools)
		require.Equal(t, string(prevTools), string(curTools), "tools 声明不得随请求变化")

		// 上一轮的 contents（除最后一个 user 轮外，它会被下一轮追加合并）必须是本轮的字节前缀
		require.GreaterOrEqual(t, len(cur.Request.Contents), len(prev.Request.Contents))
		for i := 0; i < len(prev.Request.Contents)-1; i++ {
			a, _ := json.Marshal(prev.Request.Contents[i])
			b, _ := json.Marshal(cur.Request.Contents[i])
			require.Equal(t, string(a), string(b), "contents[%d] 应保持不变", i)
		}
		last := len(prev.Request.Contents) - 1
		a, _ := json.Marshal(prev.Request.Contents[last].Parts)
		b, _ := json.Marshal(cur.Request.Contents[last].Parts[:len(prev.Request.Contents[last].Parts)])
		require.Equal(t, string(a), string(b))
		prev = cur
	}
}

func TestTransform_MidConversationSystemStaysInPlaceAndSanitized(t *testing.T) {
	req := transformForPrefix(t, claudeCodeTurns(3))

	sysJSON, _ := json.Marshal(req.Request.SystemInstruction)
	require.NotContains(t, string(sysJSON), "total_tokens")
	require.NotContains(t, string(sysJSON), "# Environment")

	// 角色交替：user/model/user/model/user，无 system 角色、无连续同角色
	roles := make([]string, 0, len(req.Request.Contents))
	for i, c := range req.Request.Contents {
		require.NotEqual(t, "system", c.Role)
		if i > 0 {
			require.NotEqual(t, req.Request.Contents[i-1].Role, c.Role)
		}
		roles = append(roles, c.Role)
	}
	require.Equal(t, []string{"user", "model", "user", "model", "user"}, roles)

	all, _ := json.Marshal(req.Request.Contents)
	require.Contains(t, string(all), "# Environment")
	require.Contains(t, string(all), "total_tokens")
	// 中途 system 消息同样经过指纹清洗
	require.NotContains(t, string(all), "Dual-use security tools")
	require.Contains(t, string(all), "tail")
	require.True(t, strings.HasSuffix(req.Request.Contents[len(req.Request.Contents)-1].Parts[len(req.Request.Contents[len(req.Request.Contents)-1].Parts)-1].Text, "</total_tokens>"))
}

func TestTransform_LeadingMessageSystemStillHoistedAndSanitized(t *testing.T) {
	req := transformForPrefix(t, []ClaudeMessage{
		{Role: "system", Content: json.RawMessage(`"You are Claude Code, Anthropic's official CLI for Claude. leading ctx"`)},
		{Role: "user", Content: json.RawMessage(`"hi"`)},
	})
	sysJSON, _ := json.Marshal(req.Request.SystemInstruction)
	require.Contains(t, string(sysJSON), "leading ctx")
	require.NotContains(t, string(sysJSON), "You are Claude Code")
	require.Len(t, req.Request.Contents, 1)
}

func TestMergeAllOf_RequiredOrderDeterministic(t *testing.T) {
	var first []byte
	for i := 0; i < 50; i++ {
		req := transformForPrefix(t, claudeCodeTurns(1))
		b, _ := json.Marshal(req.Request.Tools)
		if first == nil {
			first = b
			continue
		}
		require.True(t, bytes.Equal(first, b), "工具声明序列化必须稳定")
	}
}

// 跟在 model 轮之后的 system 消息（tool_use 与 tool_result 之间、prefill 之后）沿用抽取行为，
// 不插入新的 user 轮，避免把文本放到 functionResponse 之前或打断 prefill。
func TestTransform_SystemAfterModelTurnStillHoisted(t *testing.T) {
	t.Run("between tool_use and tool_result", func(t *testing.T) {
		req := transformForPrefix(t, []ClaudeMessage{
			{Role: "user", Content: json.RawMessage(`"run it"`)},
			{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls"}}]`)},
			{Role: "system", Content: json.RawMessage(`"between marker"`)},
			{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"toolu_1","content":"out"}]`)},
		})
		sysJSON, _ := json.Marshal(req.Request.SystemInstruction)
		require.Contains(t, string(sysJSON), "between marker")
		require.Len(t, req.Request.Contents, 3)
		last := req.Request.Contents[2]
		require.Equal(t, "user", last.Role)
		require.NotNil(t, last.Parts[0].FunctionResponse, "functionResponse 必须是该 user 轮第一个 part")
	})
	t.Run("after assistant prefill", func(t *testing.T) {
		req := transformForPrefix(t, []ClaudeMessage{
			{Role: "user", Content: json.RawMessage(`"q"`)},
			{Role: "assistant", Content: json.RawMessage(`"prefill"`)},
			{Role: "system", Content: json.RawMessage(`"tail marker"`)},
		})
		sysJSON, _ := json.Marshal(req.Request.SystemInstruction)
		require.Contains(t, string(sysJSON), "tail marker")
		require.Equal(t, "model", req.Request.Contents[len(req.Request.Contents)-1].Role)
	})
}
