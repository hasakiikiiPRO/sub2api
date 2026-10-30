import { defineStore } from 'pinia'
import { ref, computed } from 'vue'
import { announcementsAPI } from '@/api'
import type { UserAnnouncement } from '@/types'

const THROTTLE_MS = 20 * 60 * 1000 // 20 minutes
const SNOOZE_KEY_PREFIX = 'sub2api:announcement-snooze:'

/** Local calendar date (YYYY-MM-DD) in the browser's timezone. */
export function localDateKey(date: Date = new Date()): string {
  const y = date.getFullYear()
  const m = String(date.getMonth() + 1).padStart(2, '0')
  const d = String(date.getDate()).padStart(2, '0')
  return `${y}-${m}-${d}`
}

function snoozeStorageKey(id: number): string {
  return `${SNOOZE_KEY_PREFIX}${id}`
}

/** True when the user chose "don't remind again today" for this announcement today. */
export function isSnoozedToday(id: number, now: Date = new Date()): boolean {
  try {
    return window.localStorage.getItem(snoozeStorageKey(id)) === localDateKey(now)
  } catch {
    return false
  }
}

function snoozeForToday(id: number, now: Date = new Date()) {
  try {
    window.localStorage.setItem(snoozeStorageKey(id), localDateKey(now))
  } catch {
    // localStorage unavailable (private mode etc.): closing still works for this page load
  }
}

export const useAnnouncementStore = defineStore('announcements', () => {
  // State
  const announcements = ref<UserAnnouncement[]>([])
  const loading = ref(false)
  const lastFetchTime = ref(0)
  const popupQueue = ref<UserAnnouncement[]>([])
  const currentPopup = ref<UserAnnouncement | null>(null)

  // Session-scoped dedup set — not reactive, used as plain lookup only
  let shownPopupIds = new Set<number>()
  let fetchGeneration = 0

  // Getters
  const unreadCount = computed(() =>
    announcements.value.filter((a) => !a.read_at).length
  )

  // Actions
  async function fetchAnnouncements(force = false) {
    const now = Date.now()
    if (!force && lastFetchTime.value > 0 && now - lastFetchTime.value < THROTTLE_MS) {
      return
    }

    // Set immediately to prevent concurrent duplicate requests
    lastFetchTime.value = now
    const generation = ++fetchGeneration

    try {
      loading.value = true
      const all = await announcementsAPI.list(false)
      if (generation !== fetchGeneration) return
      announcements.value = all.slice(0, 20)
      enqueueNewPopups()
    } catch (err: any) {
      if (generation !== fetchGeneration) return
      // Revert throttle timestamp on failure so retry is allowed
      lastFetchTime.value = 0
      console.error('Failed to fetch announcements:', err)
    } finally {
      if (generation === fetchGeneration) loading.value = false
    }
  }

  function enqueueNewPopups(source: UserAnnouncement[] = announcements.value) {
    // Popups are shown once per page load (shownPopupIds) regardless of read state;
    // only the local "don't remind again today" choice suppresses them.
    const newPopups = source.filter(
      (a) => a.notify_mode === 'popup' && !shownPopupIds.has(a.id) && !isSnoozedToday(a.id)
    )
    if (newPopups.length === 0) return

    for (const p of newPopups) {
      if (!popupQueue.value.some((q) => q.id === p.id)) {
        popupQueue.value.push(p)
      }
    }

    if (!currentPopup.value) {
      showNextPopup()
    }
  }

  // Anonymous + logged-in: popup announcements targeted at everyone, fetched on app load.
  async function fetchPublicPopups() {
    const generation = fetchGeneration
    try {
      const items = await announcementsAPI.listPublic()
      if (generation !== fetchGeneration) return
      enqueueNewPopups(items)
    } catch (err: any) {
      console.error('Failed to fetch public announcements:', err)
    }
  }

  function showNextPopup() {
    if (popupQueue.value.length === 0) {
      currentPopup.value = null
      return
    }
    currentPopup.value = popupQueue.value.shift()!
    shownPopupIds.add(currentPopup.value.id)
  }

  async function dismissPopup(options: { snoozeToday?: boolean } = {}) {
    if (!currentPopup.value) return
    const id = currentPopup.value.id
    currentPopup.value = null

    if (options.snoozeToday) {
      snoozeForToday(id)
    }

    // Keep the bell's read tracking for logged-in users (fire-and-forget).
    // Only for announcements from the user's own list, so anonymous visitors never hit the auth API.
    const own = announcements.value.find((a) => a.id === id)
    if (own && !own.read_at) {
      markAsRead(id)
    }

    // Show next popup after a short delay
    if (popupQueue.value.length > 0) {
      setTimeout(() => showNextPopup(), 300)
    }
  }

  async function markAsRead(id: number) {
    try {
      await announcementsAPI.markRead(id)
      const ann = announcements.value.find((a) => a.id === id)
      if (ann) {
        ann.read_at = new Date().toISOString()
      }
      return true
    } catch (err: any) {
      console.error('Failed to mark announcement as read:', err)
      return false
    }
  }

  async function markAllAsRead() {
    const unread = announcements.value.filter((a) => !a.read_at)
    if (unread.length === 0) return

    try {
      loading.value = true
      const results = await Promise.allSettled(unread.map(async (a) => {
        await announcementsAPI.markRead(a.id)
        a.read_at = new Date().toISOString()
      }))
      const failure = results.find((result) => result.status === 'rejected')
      if (failure) throw failure.reason
    } catch (err: any) {
      console.error('Failed to mark all as read:', err)
      throw err
    } finally {
      loading.value = false
    }
  }

  function reset() {
    fetchGeneration++
    announcements.value = []
    lastFetchTime.value = 0
    shownPopupIds = new Set()
    popupQueue.value = []
    currentPopup.value = null
    loading.value = false
  }

  return {
    // State
    announcements,
    loading,
    currentPopup,
    // Getters
    unreadCount,
    // Actions
    fetchAnnouncements,
    fetchPublicPopups,
    dismissPopup,
    markAsRead,
    markAllAsRead,
    reset,
  }
})
