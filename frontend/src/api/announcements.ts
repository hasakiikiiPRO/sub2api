/**
 * User Announcements API endpoints
 */

import { apiClient } from './client'
import type { UserAnnouncement } from '@/types'

export async function list(unreadOnly: boolean = false): Promise<UserAnnouncement[]> {
  const { data } = await apiClient.get<UserAnnouncement[]>('/announcements', {
    params: unreadOnly ? { unread_only: 1 } : {}
  })
  return data
}

export async function markRead(id: number): Promise<{ message: string }> {
  const { data } = await apiClient.post<{ message: string }>(`/announcements/${id}/read`)
  return data
}

/**
 * Popup announcements targeted at everyone. Anonymous (no login required).
 */
export async function listPublic(): Promise<UserAnnouncement[]> {
  const { data } = await apiClient.get<UserAnnouncement[]>('/public/announcements')
  return data
}

const announcementsAPI = {
  list,
  markRead,
  listPublic
}

export default announcementsAPI

