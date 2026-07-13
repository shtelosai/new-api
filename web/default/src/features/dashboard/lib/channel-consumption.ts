/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import type { ChannelConsumptionItem } from '../types'

export interface ChannelConsumptionRow {
  channelId: number
  channelLabel: string
  rawQuota: number
}

interface ChannelConsumptionResponse {
  success: boolean
  data?: ChannelConsumptionItem[]
  message?: string
}

export function getChannelConsumptionRange(
  days: number,
  baseDate: Date = new Date()
): { start_timestamp: number; end_timestamp: number } {
  const end = new Date(baseDate)
  end.setHours(23, 59, 59, 999)

  const start = new Date(baseDate)
  start.setHours(0, 0, 0, 0)
  start.setDate(start.getDate() - (days - 1))

  return {
    start_timestamp: Math.floor(start.getTime() / 1000),
    end_timestamp: Math.floor(end.getTime() / 1000),
  }
}

export function prepareChannelConsumptionRows(
  data: ChannelConsumptionItem[]
): ChannelConsumptionRow[] {
  return data
    .map((item) => ({
      channelId: item.channel_id,
      channelLabel: `${item.channel_name || `channel-${item.channel_id}`} (#${item.channel_id})`,
      rawQuota: Number(item.quota) || 0,
    }))
    .sort((a, b) => b.rawQuota - a.rawQuota || a.channelId - b.channelId)
}

export function requireSuccessfulChannelConsumption(
  response: ChannelConsumptionResponse,
  fallbackMessage: string
): ChannelConsumptionItem[] {
  if (!response.success) {
    throw new Error(response.message || fallbackMessage)
  }
  return response.data ?? []
}
