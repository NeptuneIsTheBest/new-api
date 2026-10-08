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
import { getRouteApi } from '@tanstack/react-router'
import type { ReactNode } from 'react'
import { useTranslation } from 'react-i18next'

import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import {
  Popover,
  PopoverContent,
  PopoverTitle,
  PopoverTrigger,
} from '@/components/ui/popover'
import { Skeleton } from '@/components/ui/skeleton'
import { toIntlLocale } from '@/i18n/languages'
import { formatLogQuota, formatNumber } from '@/lib/format'
import { requireServerSuccess } from '@/lib/server-error-message'
import { cn } from '@/lib/utils'

import { getLogStats, getUserLogStats } from '../api'
import { DEFAULT_LOG_STATS } from '../constants'
import { buildApiParams } from '../lib/utils'
import { useLogsViewScope, useUsageLogsContext } from './usage-logs-provider'

const route = getRouteApi('/_authenticated/usage-logs/$section')

function StatBadge(props: {
  label: string
  value: string | number
  accent: string
  children?: ReactNode
}) {
  const content = (
    <>
      <span
        aria-hidden='true'
        className={cn('h-3.5 w-0.5 rounded-full', props.accent)}
      />
      <span className='text-muted-foreground'>{props.label}</span>
      <span className='text-foreground/85 font-mono font-semibold tabular-nums'>
        {props.value}
      </span>
    </>
  )

  if (!props.children) {
    return (
      <Badge
        variant='outline'
        className='h-7 gap-2 rounded-md px-2.5 shadow-xs'
      >
        {content}
      </Badge>
    )
  }

  return (
    <Popover>
      <PopoverTrigger
        render={
          <Button
            variant='outline'
            size='sm'
            className='gap-2 max-sm:min-h-11'
          />
        }
      >
        {content}
      </PopoverTrigger>
      <PopoverContent align='start'>
        <PopoverTitle>{props.label}</PopoverTitle>
        {props.children}
      </PopoverContent>
    </Popover>
  )
}

export function CommonLogsStats() {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const { isAdminView: isAdmin } = useLogsViewScope()
  const searchParams = route.useSearch()
  const { sensitiveVisible } = useUsageLogsContext()

  const {
    data: stats,
    isLoading,
    isError,
  } = useQuery({
    queryKey: ['usage-logs-stats', isAdmin, searchParams],
    queryFn: async () => {
      const params = buildApiParams({
        page: 1,
        pageSize: 1,
        searchParams,
        columnFilters: [],
        isAdmin,
      })

      const result = isAdmin
        ? requireServerSuccess(await getLogStats(params))
        : requireServerSuccess(await getUserLogStats(params))

      return result.success
        ? result.data || DEFAULT_LOG_STATS
        : DEFAULT_LOG_STATS
    },
  })

  if (isLoading) {
    return (
      <div className='flex flex-wrap items-center gap-2' aria-busy='true'>
        <Skeleton className='h-7 w-[150px] rounded-md' />
        <Skeleton className='h-7 w-[100px] rounded-md' />
        <Skeleton className='h-7 w-[120px] rounded-md' />
        <Skeleton className='h-7 w-[100px] rounded-md' />
        <Skeleton className='h-7 w-[120px] rounded-md' />
      </div>
    )
  }

  const hasTokenStats = !isError && stats?.total_tokens != null
  const hasCacheStats = !isError && stats?.cache_hit_rate != null
  let quota = '••••'
  if (sensitiveVisible) {
    quota = isError ? '—' : formatLogQuota(stats?.quota || 0)
  }

  return (
    <div className='flex flex-wrap items-center gap-2'>
      <StatBadge label={t('Usage')} value={quota} accent='bg-sky-500/70' />
      <StatBadge
        label={t('Tokens')}
        value={hasTokenStats ? formatNumber(stats.total_tokens, locale) : '—'}
        accent='bg-primary/70'
      >
        {hasTokenStats && (
          <dl className='grid grid-cols-[1fr_auto] gap-x-4 gap-y-2 text-sm'>
            <dt className='text-muted-foreground'>{t('Total Tokens')}</dt>
            <dd className='font-mono tabular-nums'>
              {formatNumber(stats.total_tokens, locale)}
            </dd>
            <dt className='text-muted-foreground'>{t('Input Tokens')}</dt>
            <dd className='font-mono tabular-nums'>
              {formatNumber(stats.input_tokens, locale)}
            </dd>
            <dt className='text-muted-foreground'>{t('Output Tokens')}</dt>
            <dd className='font-mono tabular-nums'>
              {formatNumber(stats.output_tokens, locale)}
            </dd>
          </dl>
        )}
      </StatBadge>
      <StatBadge
        label={t('Cache hit rate')}
        value={
          hasCacheStats ? `${formatNumber(stats.cache_hit_rate, locale)}%` : '—'
        }
        accent='bg-chart-2'
      >
        {hasCacheStats && (
          <dl className='grid grid-cols-[1fr_auto] gap-x-4 gap-y-2 text-sm'>
            <dt className='text-muted-foreground'>{t('Cache Read Tokens')}</dt>
            <dd className='font-mono tabular-nums'>
              {formatNumber(stats.cache_read_tokens, locale)}
            </dd>
            <dt className='text-muted-foreground'>{t('Input Tokens')}</dt>
            <dd className='font-mono tabular-nums'>
              {formatNumber(stats.input_tokens, locale)}
            </dd>
          </dl>
        )}
      </StatBadge>
      <StatBadge
        label={t('RPM')}
        value={isError ? '—' : formatNumber(stats?.rpm || 0, locale)}
        accent='bg-rose-500/65'
      />
      <StatBadge
        label={t('TPM')}
        value={isError ? '—' : formatNumber(stats?.tpm || 0, locale)}
        accent='bg-slate-400/70'
      />
    </div>
  )
}
