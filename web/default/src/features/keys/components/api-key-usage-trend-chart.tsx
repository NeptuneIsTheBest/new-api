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
import { VChart } from '@visactor/react-vchart'
import { useMemo } from 'react'

import { Skeleton } from '@/components/ui/skeleton'
import { useChartTheme } from '@/lib/use-chart-theme'
import { VCHART_OPTION } from '@/lib/vchart'

import type { ApiKeyUsageTrend } from '../types'

type ApiKeyUsageTrendChartProps = {
  trend: ApiKeyUsageTrend[]
  locale?: string
  inputLabel: string
  outputLabel: string
  ariaLabel: string
}

export function ApiKeyUsageTrendChart(props: ApiKeyUsageTrendChartProps) {
  const { resolvedTheme, themeReady } = useChartTheme()
  const spec = useMemo(() => {
    const dateFormatter = new Intl.DateTimeFormat(props.locale, {
      year: 'numeric',
      month: 'short',
      day: 'numeric',
      timeZone: 'UTC',
    })
    const values = props.trend.flatMap((item) => {
      const date = dateFormatter.format(new Date(`${item.bucket}T00:00:00Z`))
      return [
        {
          date,
          category: props.inputLabel,
          tokens: item.prompt_tokens,
        },
        {
          date,
          category: props.outputLabel,
          tokens: item.completion_tokens,
        },
      ]
    })

    return {
      type: 'bar',
      data: [{ id: 'usage', values }],
      xField: 'date',
      yField: 'tokens',
      seriesField: 'category',
      stack: true,
      padding: { top: 12, right: 16, bottom: 8, left: 8 },
      color: ['#0ea5e9', '#8b5cf6'],
      bar: { style: { cornerRadius: 3 } },
      axes: [
        {
          orient: 'left',
          label: {
            formatMethod: (value: number) => value.toLocaleString(props.locale),
          },
        },
        { orient: 'bottom', label: { autoRotate: true, autoHide: true } },
      ],
      legends: { visible: true, orient: 'bottom', position: 'middle' },
      tooltip: { visible: true },
      theme: resolvedTheme === 'dark' ? 'dark' : 'light',
      background: 'transparent',
    }
  }, [
    props.inputLabel,
    props.locale,
    props.outputLabel,
    props.trend,
    resolvedTheme,
  ])

  if (!themeReady) return <Skeleton className='h-full w-full' />

  return (
    <div className='h-full w-full' role='img' aria-label={props.ariaLabel}>
      <VChart spec={spec} option={VCHART_OPTION} />
    </div>
  )
}
