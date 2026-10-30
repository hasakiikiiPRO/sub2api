import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import { useAnnouncementStore, isSnoozedToday, localDateKey } from '../announcements'
import type { UserAnnouncement } from '@/types'

const list = vi.hoisted(() => vi.fn())
const listPublic = vi.hoisted(() => vi.fn())
const markRead = vi.hoisted(() => vi.fn())
vi.mock('@/api', () => ({ announcementsAPI: { list, listPublic, markRead } }))

const notice = (id: number, extra: Partial<UserAnnouncement> = {}): UserAnnouncement => ({
  id, title: `Notice ${id}`, content: 'Content', notify_mode: 'popup',
  created_at: '2026-10-21T00:00:00Z', updated_at: '2026-10-21T00:00:00Z', ...extra
})

beforeEach(() => {
  setActivePinia(createPinia())
  vi.resetAllMocks()
  window.localStorage.clear()
  markRead.mockResolvedValue({ message: 'ok' })
  vi.spyOn(console, 'error').mockImplementation(() => {})
})
afterEach(() => vi.restoreAllMocks())

describe('public popup announcements', () => {
  it('shows public popups to anonymous visitors without calling the auth API on close', async () => {
    const store = useAnnouncementStore()
    listPublic.mockResolvedValueOnce([notice(1)])
    await store.fetchPublicPopups()
    expect(store.currentPopup?.id).toBe(1)

    await store.dismissPopup()
    expect(store.currentPopup).toBeNull()
    expect(markRead).not.toHaveBeenCalled()
    expect(isSnoozedToday(1)).toBe(false)
  })

  it('shows popups to logged-in users even when already read', async () => {
    const store = useAnnouncementStore()
    list.mockResolvedValueOnce([notice(1, { read_at: '2026-10-22T00:00:00Z' })])
    await store.fetchAnnouncements(true)
    expect(store.currentPopup?.id).toBe(1)

    await store.dismissPopup()
    expect(markRead).not.toHaveBeenCalled() // already read, no duplicate write
  })

  it('marks unread user announcements as read on close (bell tracking preserved)', async () => {
    const store = useAnnouncementStore()
    list.mockResolvedValueOnce([notice(2)])
    await store.fetchAnnouncements(true)
    await store.dismissPopup()
    expect(markRead).toHaveBeenCalledWith(2)
  })

  it('does not show the same popup twice in one page load from both sources', async () => {
    const store = useAnnouncementStore()
    listPublic.mockResolvedValueOnce([notice(1)])
    list.mockResolvedValueOnce([notice(1)])
    await store.fetchPublicPopups()
    await store.fetchAnnouncements(true)
    await store.dismissPopup()
    expect(store.currentPopup).toBeNull()
  })

  it('"don\'t remind today" suppresses the popup for the rest of the local day only', async () => {
    const store = useAnnouncementStore()
    listPublic.mockResolvedValueOnce([notice(1)])
    await store.fetchPublicPopups()
    await store.dismissPopup({ snoozeToday: true })

    expect(window.localStorage.getItem('sub2api:announcement-snooze:1')).toBe(localDateKey())
    expect(isSnoozedToday(1)).toBe(true)

    // Simulate a new page load the same day
    setActivePinia(createPinia())
    const reloaded = useAnnouncementStore()
    listPublic.mockResolvedValueOnce([notice(1)])
    await reloaded.fetchPublicPopups()
    expect(reloaded.currentPopup).toBeNull()

    // Next day it shows again
    const tomorrow = new Date()
    tomorrow.setDate(tomorrow.getDate() + 1)
    expect(isSnoozedToday(1, tomorrow)).toBe(false)
  })

  it('swallows public fetch errors', async () => {
    const store = useAnnouncementStore()
    listPublic.mockRejectedValueOnce(new Error('boom'))
    await expect(store.fetchPublicPopups()).resolves.toBeUndefined()
    expect(store.currentPopup).toBeNull()
  })
})
