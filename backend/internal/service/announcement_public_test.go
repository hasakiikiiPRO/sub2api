package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/stretchr/testify/require"
)

type publicAnnouncementRepoStub struct {
	items []Announcement
	calls int
}

func (*publicAnnouncementRepoStub) Create(context.Context, *Announcement) error { return nil }
func (*publicAnnouncementRepoStub) GetByID(context.Context, int64) (*Announcement, error) {
	return nil, ErrAnnouncementNotFound
}
func (*publicAnnouncementRepoStub) Update(context.Context, *Announcement) error { return nil }
func (*publicAnnouncementRepoStub) Delete(context.Context, int64) error         { return nil }
func (*publicAnnouncementRepoStub) List(context.Context, pagination.PaginationParams, AnnouncementListFilters) ([]Announcement, *pagination.PaginationResult, error) {
	return nil, nil, nil
}
func (s *publicAnnouncementRepoStub) ListActive(context.Context, time.Time) ([]Announcement, error) {
	s.calls++
	return s.items, nil
}

func TestListPublicPopupsOnlyReturnsUntargetedActivePopups(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)
	repo := &publicAnnouncementRepoStub{items: []Announcement{
		{ID: 1, Title: "all-popup", Status: AnnouncementStatusActive, NotifyMode: AnnouncementNotifyModePopup, StartsAt: &past},
		{ID: 2, Title: "silent", Status: AnnouncementStatusActive, NotifyMode: AnnouncementNotifyModeSilent},
		{ID: 3, Title: "targeted", Status: AnnouncementStatusActive, NotifyMode: AnnouncementNotifyModePopup,
			Targeting: AnnouncementTargeting{AnyOf: []AnnouncementConditionGroup{{AllOf: []AnnouncementCondition{{Type: AnnouncementConditionTypeBalance, Operator: AnnouncementOperatorGT, Value: 10}}}}}},
		{ID: 4, Title: "not-started", Status: AnnouncementStatusActive, NotifyMode: AnnouncementNotifyModePopup, StartsAt: &future},
		{ID: 5, Title: "draft", Status: AnnouncementStatusDraft, NotifyMode: AnnouncementNotifyModePopup},
		{ID: 6, Title: "all-popup-2", Status: AnnouncementStatusActive, NotifyMode: AnnouncementNotifyModePopup},
	}}
	svc := NewAnnouncementService(repo, nil, nil, nil)

	got, err := svc.ListPublicPopups(context.Background())
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, int64(6), got[0].ID)
	require.Equal(t, int64(1), got[1].ID)
}

func TestListPublicPopupsCachesAndInvalidatesOnDelete(t *testing.T) {
	repo := &publicAnnouncementRepoStub{items: []Announcement{
		{ID: 1, Status: AnnouncementStatusActive, NotifyMode: AnnouncementNotifyModePopup},
	}}
	svc := NewAnnouncementService(repo, nil, nil, nil)

	_, err := svc.ListPublicPopups(context.Background())
	require.NoError(t, err)
	_, err = svc.ListPublicPopups(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, repo.calls, "second call should be served from cache")

	require.NoError(t, svc.Delete(context.Background(), 1))
	_, err = svc.ListPublicPopups(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, repo.calls, "delete must invalidate the cache")
}
