package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAntigravityGatewayService_ClaudeTransformOptions_MCPXML(t *testing.T) {
	groupContext := func(platform string, enabled bool) context.Context {
		ctx := context.WithValue(context.Background(), ctxkey.Group, &Group{
			ID: 1, Hydrated: true, Platform: platform, Status: StatusActive, MCPXMLInject: enabled,
		})
		return WithResolvedTargetPlatform(ctx, PlatformAntigravity)
	}
	tests := []struct {
		name        string
		ctx         context.Context
		nilSettings bool
		wantMCPXML  bool
	}{
		{name: "group disabled", ctx: groupContext(PlatformAntigravity, false)},
		{name: "group enabled", ctx: groupContext(PlatformAntigravity, true), wantMCPXML: true},
		{name: "composite disabled", ctx: groupContext(PlatformComposite, false)},
		{name: "composite enabled", ctx: groupContext(PlatformComposite, true), wantMCPXML: true},
		{name: "missing group", ctx: context.Background(), wantMCPXML: true},
		{name: "nil group", ctx: context.WithValue(context.Background(), ctxkey.Group, (*Group)(nil)), wantMCPXML: true},
		{name: "wrong group type", ctx: context.WithValue(context.Background(), ctxkey.Group, "untrusted"), wantMCPXML: true},
		{name: "unhydrated group", ctx: context.WithValue(context.Background(), ctxkey.Group, &Group{ID: 1, Platform: PlatformAntigravity, Status: StatusActive}), wantMCPXML: true},
		{name: "disabled without settings", ctx: groupContext(PlatformAntigravity, false), nilSettings: true},
		{name: "enabled without settings", ctx: groupContext(PlatformAntigravity, true), nilSettings: true, wantMCPXML: true},
		{name: "missing group without settings", ctx: context.Background(), nilSettings: true, wantMCPXML: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &AntigravityGatewayService{}
			want := antigravity.DefaultTransformOptions()
			want.EnableMCPXML = tt.wantMCPXML
			if !tt.nilSettings {
				svc.settingService = NewSettingService(&antigravitySettingRepoStub{values: map[string]string{
					SettingKeyEnableIdentityPatch: "false",
					SettingKeyIdentityPatchPrompt: "custom identity",
				}}, &config.Config{})
				want.EnableIdentityPatch = false
				want.IdentityPatch = "custom identity"
			}
			opts := svc.getClaudeTransformOptions(tt.ctx)
			require.Equal(t, want, opts)

			body, err := antigravity.TransformClaudeToGeminiWithOptions(&antigravity.ClaudeRequest{
				Model:    "claude-sonnet-4-5",
				Messages: []antigravity.ClaudeMessage{{Role: "user", Content: json.RawMessage(`"hello"`)}},
				Tools:    []antigravity.ClaudeTool{{Name: "mcp__test__lookup", InputSchema: map[string]any{"type": "object"}}},
			}, "project", "claude-sonnet-4-5", opts)
			require.NoError(t, err)
			require.Equal(t, tt.wantMCPXML, strings.Contains(string(body), "MCP XML 工具调用协议"))
		})
	}
}

func TestAntigravityGatewayService_Forward_MCPXML(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name       string
		group      *Group
		toolName   string
		retry      bool
		wantMCPXML bool
	}{
		{name: "disabled including signature retries", group: &Group{MCPXMLInject: false}, toolName: "mcp__test__lookup", retry: true},
		{name: "enabled including signature retries", group: &Group{MCPXMLInject: true}, toolName: "mcp__test__lookup", retry: true, wantMCPXML: true},
		{name: "composite disabled", group: &Group{Platform: PlatformComposite}, toolName: "mcp__test__lookup"},
		{name: "composite enabled", group: &Group{Platform: PlatformComposite, MCPXMLInject: true}, toolName: "mcp__test__lookup", wantMCPXML: true},
		{name: "missing auth group uses default", toolName: "mcp__test__lookup", wantMCPXML: true},
		{name: "non MCP tools do not inject", group: &Group{MCPXMLInject: true}, toolName: "lookup"},
		{name: "no tools do not inject", group: &Group{MCPXMLInject: true}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := antigravity.ClaudeRequest{
				Model:     "claude-sonnet-4-5",
				MaxTokens: 16,
				System:    json.RawMessage(`"Keep the user instructions."`),
				Messages: []antigravity.ClaudeMessage{
					{Role: "user", Content: json.RawMessage(`"hello"`)},
				},
			}
			if tt.toolName != "" {
				req.Tools = []antigravity.ClaudeTool{{Name: tt.toolName, InputSchema: map[string]any{"type": "object"}}}
			}
			upstream := &queuedHTTPUpstreamStub{}
			if tt.retry {
				req.Thinking = &antigravity.ThinkingConfig{Type: "enabled", BudgetTokens: 1024}
				req.Messages = append(req.Messages,
					antigravity.ClaudeMessage{Role: "assistant", Content: json.RawMessage(`[{"type":"thinking","thinking":"plan","signature":"bad-signature"},{"type":"tool_use","id":"tool-1","name":"mcp__test__lookup","input":{}}]`)},
					antigravity.ClaudeMessage{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"tool-1","content":"ok"}]`)},
				)
				for i := 0; i < 2; i++ {
					upstream.responses = append(upstream.responses, &http.Response{
						StatusCode: http.StatusBadRequest,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"Corrupted thought signature."}}`)),
					})
				}
			}
			response := antigravityCompatSuccessResponse()
			if tt.toolName != "" {
				responseBody, err := json.Marshal(antigravity.V1InternalResponse{Response: antigravity.GeminiResponse{
					Candidates: []antigravity.GeminiCandidate{{
						Content: &antigravity.GeminiContent{Role: "model", Parts: []antigravity.GeminiPart{{
							FunctionCall: &antigravity.GeminiFunctionCall{Name: tt.toolName, Args: map[string]any{"query": "result"}},
						}}},
						FinishReason: "STOP",
					}},
				}})
				require.NoError(t, err)
				response.Body = io.NopCloser(strings.NewReader("data: " + string(responseBody) + "\n\n"))
			}
			upstream.responses = append(upstream.responses, response)
			svc := newAntigravityCompatService(config.GatewayConfig{MaxLineSize: defaultMaxLineSize}, upstream)
			body, err := json.Marshal(req)
			require.NoError(t, err)
			c, recorder := newAntigravityCompatContext(http.MethodPost, "/v1/messages", body)
			if tt.group != nil {
				group := *tt.group
				group.ID, group.Hydrated, group.Status = 1, true, StatusActive
				if group.Platform == "" {
					group.Platform = PlatformAntigravity
				}
				ctx := context.WithValue(c.Request.Context(), ctxkey.Group, &group)
				if group.Platform == PlatformComposite {
					ctx = WithCompositeRouteDecision(ctx, CompositeRouteDecision{Matched: true, TargetPlatform: PlatformAntigravity})
				}
				c.Request = c.Request.WithContext(ctx)
			}

			result, err := svc.Forward(c.Request.Context(), c, newAntigravityCompatAccount(AccountTypeOAuth), body, false)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, http.StatusOK, recorder.Code)
			if tt.toolName != "" {
				var claudeResponse antigravity.ClaudeResponse
				require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &claudeResponse))
				require.Equal(t, "tool_use", claudeResponse.StopReason)
				require.Len(t, claudeResponse.Content, 1)
				require.Equal(t, "tool_use", claudeResponse.Content[0].Type)
				require.Equal(t, tt.toolName, claudeResponse.Content[0].Name)
				require.Equal(t, map[string]any{"query": "result"}, claudeResponse.Content[0].Input)
			}
			require.Len(t, upstream.requestBodies, len(upstream.responses))
			for i, requestBody := range upstream.requestBodies {
				var transformed antigravity.V1InternalRequest
				require.NoError(t, json.Unmarshal(requestBody, &transformed))
				require.NotNil(t, transformed.Request.SystemInstruction)
				var system strings.Builder
				for _, part := range transformed.Request.SystemInstruction.Parts {
					system.WriteString(part.Text)
				}
				require.Equal(t, tt.wantMCPXML, strings.Contains(system.String(), "MCP XML 工具调用协议"), "attempt %d", i)
				require.Contains(t, system.String(), "You are Antigravity", "identity must remain unchanged")
				require.Contains(t, system.String(), "Keep the user instructions.")
				if tt.toolName == "" {
					require.Empty(t, transformed.Request.Tools)
				} else {
					require.Len(t, transformed.Request.Tools, 1)
					require.Len(t, transformed.Request.Tools[0].FunctionDeclarations, 1)
					require.Equal(t, tt.toolName, transformed.Request.Tools[0].FunctionDeclarations[0].Name, "native declarations must remain unchanged")
				}
			}
		})
	}
}
