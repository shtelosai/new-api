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
import assert from 'node:assert/strict'
import { describe, test } from 'node:test'

import type { ChannelConsumptionItem } from '../types'
import {
  getChannelConsumptionRange,
  prepareChannelConsumptionRows,
  requireSuccessfulChannelConsumption,
} from './channel-consumption'

function assertLocalDate(
  timestamp: number,
  expected: [number, number, number, number, number, number]
) {
  const date = new Date(timestamp * 1000)
  assert.deepEqual(
    [
      date.getFullYear(),
      date.getMonth() + 1,
      date.getDate(),
      date.getHours(),
      date.getMinutes(),
      date.getSeconds(),
    ],
    expected
  )
}

describe('channel consumption helpers', () => {
  test('builds a full local-day range for a specific date', () => {
    const range = getChannelConsumptionRange(1, new Date(2026, 6, 12, 15, 30))

    assertLocalDate(range.start_timestamp, [2026, 7, 12, 0, 0, 0])
    assertLocalDate(range.end_timestamp, [2026, 7, 12, 23, 59, 59])
  })

  test('builds inclusive 7-day and 30-day natural ranges', () => {
    const baseDate = new Date(2026, 6, 12, 15, 30)
    const sevenDays = getChannelConsumptionRange(7, baseDate)
    const thirtyDays = getChannelConsumptionRange(30, baseDate)

    assertLocalDate(sevenDays.start_timestamp, [2026, 7, 6, 0, 0, 0])
    assertLocalDate(sevenDays.end_timestamp, [2026, 7, 12, 23, 59, 59])
    assertLocalDate(thirtyDays.start_timestamp, [2026, 6, 13, 0, 0, 0])
    assertLocalDate(thirtyDays.end_timestamp, [2026, 7, 12, 23, 59, 59])
  })

  test('sorts channels by quota and keeps duplicate names unique', () => {
    const data: ChannelConsumptionItem[] = [
      { channel_id: 3, channel_name: 'channel-3', quota: 25 },
      { channel_id: 2, channel_name: 'shared', quota: 150 },
      { channel_id: 1, channel_name: 'shared', quota: 150 },
    ]

    assert.deepEqual(prepareChannelConsumptionRows(data), [
      { channelId: 1, channelLabel: 'shared (#1)', rawQuota: 150 },
      { channelId: 2, channelLabel: 'shared (#2)', rawQuota: 150 },
      { channelId: 3, channelLabel: 'channel-3 (#3)', rawQuota: 25 },
    ])
  })

  test('throws unsuccessful responses instead of rendering an empty chart', () => {
    assert.throws(
      () =>
        requireSuccessfulChannelConsumption(
          { success: false, message: 'database unavailable' },
          'Failed to load channel consumption'
        ),
      /database unavailable/
    )
    assert.deepEqual(
      requireSuccessfulChannelConsumption(
        {
          success: true,
          data: [{ channel_id: 1, channel_name: 'east', quota: 100 }],
        },
        'Failed to load channel consumption'
      ),
      [{ channel_id: 1, channel_name: 'east', quota: 100 }]
    )
  })
})
