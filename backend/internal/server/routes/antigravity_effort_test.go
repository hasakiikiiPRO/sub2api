package routes

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requestmodel"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAntigravityEffortMiddlewareCanonicalizesBeforeScheduling(t *testing.T) {
	for _, tc := range []struct {
		name, path, public, upstream, effort, platform string
		forced                                         bool
	}{
		{"messages", "/v1/messages", "claude-opus-5-5-antigravity", "claude-opus-5-5", `"output_config":{"effort":"medium"}`, service.PlatformComposite, false},
		{"count tokens", "/v1/messages/count_tokens", "claude-sonnet-5-5-antigravity", "claude-sonnet-5-5", `"output_config":{"effort":"medium"}`, service.PlatformComposite, false},
		{"responses", "/v1/responses", "gemini-4-argon", "", `"reasoning":{"effort":"medium"}`, service.PlatformComposite, false},
		{"responses alias", "/responses", "ag-claude-opus-5-5", "claude-opus-5-5", `"reasoning":{"effort":"medium"}`, service.PlatformComposite, false},
		{"chat", "/v1/chat/completions", "claude-opus-5-5-antigravity", "claude-opus-5-5", `"reasoning_effort":"medium"`, service.PlatformComposite, false},
		{"forced", "/antigravity/v1/messages", "claude-opus-5-5-antigravity", "claude-opus-5-5", `"output_config":{"effort":"medium"}`, service.PlatformAnthropic, true},
		{"AG group", "/v1/messages", "ag-claude-sonnet-5-5", "claude-sonnet-5-5", `"output_config":{"effort":"medium"}`, service.PlatformAntigravity, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			router := gin.New()
			router.Use(func(c *gin.Context) {
				c.Set(string(servermiddleware.ContextKeyAPIKey), &service.APIKey{Group: &service.Group{ID: 1, Platform: tc.platform}})
				c.Next()
			})
			if tc.forced {
				router.Use(servermiddleware.ForcePlatform(service.PlatformAntigravity))
			}
			resolver := service.NewCompositeRouteResolver(compositeRouteRepoStub{routes: []service.CompositeModelRoute{{
				GroupID: 1, PublicModel: tc.public, MatchType: service.CompositeRouteMatchExact,
				TargetPlatform: service.PlatformAntigravity, UpstreamModel: tc.upstream,
				Endpoint: service.CompositeRouteEndpointAny, Enabled: true,
			}}})
			router.Use(compositeTargetPlatformMiddleware(resolver))
			router.POST(tc.path, func(c *gin.Context) {
				public, ok := service.RequestedPublicModelFromContext(c.Request.Context())
				require.True(t, ok)
				require.Equal(t, tc.public, public)
				upstream, ok := service.ResolvedUpstreamModelFromContext(c.Request.Context())
				require.True(t, ok)
				family := tc.upstream
				if family == "" {
					family = tc.public
				}
				require.Equal(t, family+"-medium", upstream)
				body, err := io.ReadAll(c.Request.Body)
				require.NoError(t, err)
				// Messages scheduler takes ParseGatewayRequest.Model; compatibility
				// adapters unmarshal JSON. Neither may see a public/stale duplicate.
				parsed, err := service.ParseGatewayRequest(service.NewRequestBodyRef(body), domain.PlatformAnthropic)
				require.NoError(t, err)
				require.Equal(t, upstream, parsed.Model)
				var bound struct{ Model string }
				require.NoError(t, json.Unmarshal(body, &bound))
				require.Equal(t, upstream, bound.Model)
				require.Equal(t, []string{upstream}, requestmodel.FromBodyCandidates(c.FullPath(), "application/json", body))
				usage := (service.ChannelMappingResult{}).ToUsageFields(public, upstream)
				require.Equal(t, tc.public, usage.OriginalModel)
				c.Status(http.StatusNoContent)
			})
			body := fmt.Sprintf(`{"model":%q,"Model":"stale-duplicate","model":%q,"messages":[],%s}`, tc.public, tc.public, tc.effort)
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
		})
	}
}

func TestAntigravityEffortMiddlewarePreservesNativeAndPinnedRoutes(t *testing.T) {
	for _, tc := range []struct{ public, upstream, platform, expected string }{
		{"claude-opus-5-5", "claude-opus-5-5", service.PlatformAnthropic, "claude-opus-5-5"},
		{"claude-sonnet-5-5", "claude-sonnet-5-5", service.PlatformAnthropic, "claude-sonnet-5-5"},
		{"claude-opus-5-5-antigravity", "claude-opus-5-5-low", service.PlatformAntigravity, "claude-opus-5-5-low"},
		{"newclaudealias", "claude-opus-5-5", service.PlatformAntigravity, "claude-opus-5-5"},
	} {
		t.Run(tc.public+"/"+tc.upstream, func(t *testing.T) {
			router := gin.New()
			router.Use(func(c *gin.Context) {
				c.Set(string(servermiddleware.ContextKeyAPIKey), &service.APIKey{Group: &service.Group{ID: 1, Platform: service.PlatformComposite}})
			})
			resolver := service.NewCompositeRouteResolver(compositeRouteRepoStub{routes: []service.CompositeModelRoute{{
				GroupID: 1, PublicModel: tc.public, MatchType: service.CompositeRouteMatchExact,
				TargetPlatform: tc.platform, UpstreamModel: tc.upstream, Endpoint: service.CompositeRouteEndpointAny, Enabled: true,
			}}})
			router.Use(compositeTargetPlatformMiddleware(resolver))
			router.POST("/v1/messages", func(c *gin.Context) {
				model, _ := service.ResolvedUpstreamModelFromContext(c.Request.Context())
				require.Equal(t, tc.expected, model)
				platform, _ := service.ResolvedTargetPlatformFromContext(c.Request.Context())
				require.Equal(t, tc.platform, platform)
				c.Status(http.StatusNoContent)
			})
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(fmt.Sprintf(`{"model":%q,"output_config":{"effort":"medium"}}`, tc.public))))
			require.Equal(t, http.StatusNoContent, w.Code)
		})
	}
}

func TestAntigravityEffortGeminiMiddlewareRestoresBodyAndPublicIdentity(t *testing.T) {
	for _, forced := range []bool{false, true} {
		t.Run(fmt.Sprint(forced), func(t *testing.T) {
			router := gin.New()
			router.Use(func(c *gin.Context) {
				c.Set(string(servermiddleware.ContextKeyAPIKey), &service.APIKey{Group: &service.Group{ID: 1, Platform: service.PlatformComposite}})
			})
			if forced {
				router.Use(servermiddleware.ForcePlatform(service.PlatformAntigravity))
			}
			// Match the live group's gemini prefix passthrough route.
			resolver := service.NewCompositeRouteResolver(compositeRouteRepoStub{routes: []service.CompositeModelRoute{{
				GroupID: 1, PublicModel: "gemini-", MatchType: service.CompositeRouteMatchPrefix,
				TargetPlatform: service.PlatformAntigravity, Endpoint: service.CompositeRouteEndpointAny, Enabled: true,
			}}})
			router.Use(compositeGeminiTargetPlatformMiddleware(resolver))
			body := `{"contents":[],"generationConfig":{"thinkingConfig":{"thinkingLevel":"low"}}}`
			router.POST("/v1beta/models/*modelAction", func(c *gin.Context) {
				upstream, _ := service.ResolvedUpstreamModelFromContext(c.Request.Context())
				require.Equal(t, "gemini-4-argon-low", upstream)
				public, _ := service.RequestedPublicModelFromContext(c.Request.Context())
				require.Equal(t, "gemini-4-argon", public)
				got, err := io.ReadAll(c.Request.Body)
				require.NoError(t, err)
				require.Equal(t, body, string(got))
				require.Equal(t, int64(len(got)), c.Request.ContentLength)
				c.Status(http.StatusNoContent)
			})
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-4-argon:generateContent", strings.NewReader(body)))
			require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
		})
	}
}

func TestAntigravityEffortMiddlewareRejectsInvalidEffort(t *testing.T) {
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(servermiddleware.ContextKeyAPIKey), &service.APIKey{Group: &service.Group{ID: 1, Platform: service.PlatformAntigravity}})
	})
	router.Use(compositeTargetPlatformMiddleware(nil))
	router.POST("/v1/messages", func(c *gin.Context) { t.Fatal("invalid effort reached scheduling") })
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-opus-5-5-antigravity","output_config":{"effort":"urgent"}}`)))
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), "unsupported Antigravity effort")
}
