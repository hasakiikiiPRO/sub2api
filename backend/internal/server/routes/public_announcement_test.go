package routes

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type publicAnnRepoStub struct{ items []service.Announcement }

func (*publicAnnRepoStub) Create(context.Context, *service.Announcement) error { return nil }
func (*publicAnnRepoStub) GetByID(context.Context, int64) (*service.Announcement, error) {
	return nil, service.ErrAnnouncementNotFound
}
func (*publicAnnRepoStub) Update(context.Context, *service.Announcement) error { return nil }
func (*publicAnnRepoStub) Delete(context.Context, int64) error                 { return nil }
func (*publicAnnRepoStub) List(context.Context, pagination.PaginationParams, service.AnnouncementListFilters) ([]service.Announcement, *pagination.PaginationResult, error) {
	return nil, nil, nil
}
func (s *publicAnnRepoStub) ListActive(context.Context, time.Time) ([]service.Announcement, error) {
	return s.items, nil
}

func TestPublicAnnouncementsRouteIsAnonymousAndMinimal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &publicAnnRepoStub{items: []service.Announcement{
		{ID: 1, Title: "指南", Content: "hello", Status: service.AnnouncementStatusActive, NotifyMode: service.AnnouncementNotifyModePopup},
		{ID: 2, Title: "定向", Content: "secret", Status: service.AnnouncementStatusActive, NotifyMode: service.AnnouncementNotifyModePopup,
			Targeting: service.AnnouncementTargeting{AnyOf: []service.AnnouncementConditionGroup{{AllOf: []service.AnnouncementCondition{{Type: service.AnnouncementConditionTypeSubscription, Operator: service.AnnouncementOperatorIn, GroupIDs: []int64{7}}}}}}},
	}}
	svc := service.NewAnnouncementService(repo, nil, nil, nil)

	router := gin.New()
	v1 := router.Group("/api/v1")
	jwtCalled := false
	RegisterAuthRoutes(
		v1,
		&handler.Handlers{
			Auth:         &handler.AuthHandler{},
			Setting:      &handler.SettingHandler{},
			Announcement: handler.NewAnnouncementHandler(svc),
		},
		servermiddleware.JWTAuthMiddleware(func(c *gin.Context) {
			jwtCalled = true
			c.AbortWithStatus(http.StatusUnauthorized)
		}),
		servermiddleware.AuditLogMiddleware(func(c *gin.Context) { c.Next() }),
		redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"}),
		nil,
		nil,
	)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/public/announcements", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.False(t, jwtCalled, "public announcements must not require auth")
	require.Equal(t, "no-store", w.Header().Get("Cache-Control"))

	var body struct {
		Data []map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Data, 1)
	require.EqualValues(t, 1, body.Data[0]["id"])
	require.NotContains(t, body.Data[0], "targeting")
	require.NotContains(t, body.Data[0], "read_at")
	require.NotContains(t, body.Data[0], "created_by")
	require.NotContains(t, w.Body.String(), "secret")
}
