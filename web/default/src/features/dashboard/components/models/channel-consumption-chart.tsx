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
import { useQuery } from '@tanstack/react-query'
import { VChart } from '@visactor/react-vchart'
import { BarChart3, WalletCards } from 'lucide-react'
import { type ReactNode, useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { DatePicker } from '@/components/date-picker'
import { EmptyState } from '@/components/empty-state'
import { ErrorState } from '@/components/error-state'
import { Skeleton } from '@/components/ui/skeleton'
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { getChannelConsumption } from '@/features/dashboard/api'
import {
  getChannelConsumptionRange,
  prepareChannelConsumptionRows,
  requireSuccessfulChannelConsumption,
} from '@/features/dashboard/lib/channel-consumption'
import { formatQuota } from '@/lib/format'
import { useChartTheme } from '@/lib/use-chart-theme'
import { VCHART_OPTION } from '@/lib/vchart'

type RangeDays = 1 | 7 | 30

const RANGE_OPTIONS: Array<{ days: RangeDays; labelKey: string }> = [
  { days: 1, labelKey: 'Today' },
  { days: 7, labelKey: '7 Days' },
  { days: 30, labelKey: '30 Days' },
]

export function ChannelConsumptionChart() {
  const { t } = useTranslation()
  const { resolvedTheme, themeReady } = useChartTheme()
  const [rangeDays, setRangeDays] = useState<RangeDays | undefined>(1)
  const [selectedDate, setSelectedDate] = useState<Date>()

  const timeRange = useMemo(() => {
    const baseDate = rangeDays ? new Date() : selectedDate || new Date()
    return getChannelConsumptionRange(rangeDays || 1, baseDate)
  }, [rangeDays, selectedDate])

  const query = useQuery({
    queryKey: [
      'dashboard',
      'channel-consumption',
      timeRange.start_timestamp,
      timeRange.end_timestamp,
    ],
    queryFn: () => getChannelConsumption(timeRange),
    select: (response) =>
      requireSuccessfulChannelConsumption(
        response,
        t('Please try again later.')
      ),
    staleTime: 60_000,
  })

  const rows = useMemo(
    () => prepareChannelConsumptionRows(query.data ?? []),
    [query.data]
  )
  const totalQuota = useMemo(
    () => rows.reduce((sum, row) => sum + row.rawQuota, 0),
    [rows]
  )
  const chartHeight = Math.max(300, rows.length * 34 + 48)
  const chartSpec = useMemo(
    () => ({
      type: 'bar' as const,
      data: [{ id: 'channelConsumptionData', values: rows }],
      xField: 'rawQuota',
      yField: 'channelLabel',
      direction: 'horizontal' as const,
      legends: { visible: false },
      bar: {
        state: {
          hover: { stroke: '#000', lineWidth: 1 },
        },
      },
      label: {
        visible: true,
        position: 'outside' as const,
        formatMethod: (value: number) => formatQuota(value),
        style: { fontSize: 11 },
      },
      axes: [
        {
          orient: 'left' as const,
          type: 'band' as const,
          label: { autoLimit: false },
        },
        { orient: 'bottom' as const, type: 'linear' as const, visible: false },
      ],
      tooltip: {
        mark: {
          content: [
            {
              key: (datum: Record<string, unknown>) => datum?.channelLabel,
              value: (datum: Record<string, unknown>) =>
                formatQuota(Number(datum?.rawQuota) || 0),
            },
          ],
        },
      },
      background: { fill: 'transparent' },
      animation: true,
    }),
    [rows]
  )

  const handleRangeChange = (value: string) => {
    setRangeDays(Number(value) as RangeDays)
    setSelectedDate(undefined)
  }

  const handleDateSelect = (date: Date | undefined) => {
    setSelectedDate(date)
    if (date) {
      setRangeDays(undefined)
      return
    }
    setRangeDays(1)
  }

  let chartContent: ReactNode
  if (query.isPending || !themeReady) {
    chartContent = <Skeleton className='h-[300px] w-full' />
  } else if (query.isError) {
    chartContent = (
      <ErrorState
        className='min-h-[300px]'
        title={t('Failed to load')}
        description={
          query.error instanceof Error
            ? query.error.message
            : t('Please try again later.')
        }
        onRetry={() => void query.refetch()}
      />
    )
  } else if (rows.length === 0) {
    chartContent = (
      <EmptyState
        icon={BarChart3}
        className='min-h-[300px]'
        title={t('No data available')}
      />
    )
  } else {
    chartContent = (
      <div className='max-h-[520px] overflow-auto'>
        <div className='min-w-[560px]' style={{ height: chartHeight }}>
          <VChart
            key={`${timeRange.start_timestamp}-${timeRange.end_timestamp}-${rows.length}-${resolvedTheme}`}
            spec={{
              ...chartSpec,
              theme: resolvedTheme === 'dark' ? 'dark' : 'light',
              background: 'transparent',
            }}
            option={VCHART_OPTION}
          />
        </div>
      </div>
    )
  }

  let totalQuotaDisplay = formatQuota(totalQuota)
  if (query.isPending) {
    totalQuotaDisplay = '—'
  } else if (query.isError) {
    totalQuotaDisplay = '--'
  }

  return (
    <div className='overflow-hidden rounded-lg border'>
      <div className='flex w-full flex-col gap-2 border-b px-3 py-2 sm:px-5 sm:py-3 lg:flex-row lg:items-center lg:justify-between'>
        <div className='flex min-w-0 items-center gap-2'>
          <WalletCards className='text-muted-foreground/60 size-4 shrink-0' />
          <div className='truncate text-sm font-semibold'>
            {t('Channel Consumption Ranking')}
          </div>
          <span className='text-muted-foreground shrink-0 text-xs'>
            {t('Total:')} {totalQuotaDisplay}
          </span>
        </div>

        <div className='flex max-w-full items-center gap-1.5 overflow-x-auto pb-0.5'>
          <Tabs
            value={rangeDays ? String(rangeDays) : ''}
            onValueChange={handleRangeChange}
          >
            <TabsList className='shrink-0'>
              {RANGE_OPTIONS.map((option) => (
                <TabsTrigger
                  key={option.days}
                  value={String(option.days)}
                  className='px-2.5 text-xs'
                >
                  {t(option.labelKey)}
                </TabsTrigger>
              ))}
            </TabsList>
          </Tabs>
          <div className='shrink-0'>
            <DatePicker
              selected={selectedDate}
              onSelect={handleDateSelect}
              placeholder={t('Pick a date')}
            />
          </div>
        </div>
      </div>

      <div className='p-1.5 sm:p-2'>{chartContent}</div>
    </div>
  )
}
