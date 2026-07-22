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
import {
  Alert02Icon,
  ArrowRight01Icon,
  ChartBarStackedIcon,
} from '@hugeicons/core-free-icons'
import { HugeiconsIcon } from '@hugeicons/react'
import { useQuery } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { lazy, Suspense, useMemo } from 'react'
import { useTranslation } from 'react-i18next'

import {
  sideDrawerContentClassName,
  sideDrawerHeaderClassName,
} from '@/components/drawer-layout'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from '@/components/ui/card'
import {
  Empty,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from '@/components/ui/empty'
import { Progress } from '@/components/ui/progress'
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from '@/components/ui/sheet'
import { Skeleton } from '@/components/ui/skeleton'
import { toIntlLocale } from '@/i18n/languages'
import { formatBillingQuota, formatNumber } from '@/lib/format'
import { cn } from '@/lib/utils'

import { getApiKeyUsageDetails } from '../api'
import type { ApiKey, ApiKeyUsageDetails } from '../types'

const LazyApiKeyUsageTrendChart = lazy(() =>
  import('./api-key-usage-trend-chart').then((module) => ({
    default: module.ApiKeyUsageTrendChart,
  }))
)

type ApiKeyUsageDetailsDrawerProps = {
  open: boolean
  onOpenChange: (open: boolean) => void
  apiKey: ApiKey | null
}

function UsageMetric(props: {
  label: string
  value: string
  description: string
  tone?: 'default' | 'danger'
}) {
  return (
    <Card size='sm' className='gap-2'>
      <CardHeader>
        <CardDescription>{props.label}</CardDescription>
        <CardTitle
          className={cn(
            'text-xl font-semibold tabular-nums',
            props.tone === 'danger' && 'text-destructive'
          )}
        >
          {props.value}
        </CardTitle>
      </CardHeader>
      <CardContent className='text-muted-foreground text-xs'>
        {props.description}
      </CardContent>
    </Card>
  )
}

function UsageDetailsSkeleton() {
  return (
    <div className='space-y-6'>
      <Skeleton className='h-9 w-44' />
      <div className='grid grid-cols-2 gap-3 lg:grid-cols-4'>
        {['cost', 'settled', 'failed', 'tokens'].map((key) => (
          <Skeleton key={key} className='h-28 rounded-xl' />
        ))}
      </div>
      <Skeleton className='h-72 rounded-xl' />
      <Skeleton className='h-64 rounded-xl' />
    </div>
  )
}

function UsageDetailsContent(props: {
  details: ApiKeyUsageDetails
  locale?: string
  onViewLogs: () => void
}) {
  const { t } = useTranslation()
  const dateTimeFormatter = useMemo(
    () =>
      new Intl.DateTimeFormat(props.locale, {
        dateStyle: 'medium',
        timeStyle: 'short',
        timeZone: props.details.timezone,
      }),
    [props.details.timezone, props.locale]
  )
  const rangeLabel = `${dateTimeFormatter.format(
    new Date(props.details.range_start * 1000)
  )} – ${dateTimeFormatter.format(new Date(props.details.range_end * 1000))}`
  const cacheHitRate = Intl.NumberFormat(props.locale, {
    style: 'percent',
    maximumFractionDigits: 2,
  }).format(props.details.summary.cache_hit_rate)
  const maxModelQuota = Math.max(
    0,
    ...props.details.models.map((model) => model.total_quota)
  )

  if (!props.details.available) {
    return (
      <Alert>
        <HugeiconsIcon icon={Alert02Icon} strokeWidth={2} />
        <AlertTitle>{t('Usage logs unavailable')}</AlertTitle>
        <AlertDescription>
          {t(
            'Detailed usage is unavailable because consumption logging is disabled.'
          )}
        </AlertDescription>
      </Alert>
    )
  }

  return (
    <div className='space-y-6'>
      <div className='flex flex-wrap items-center justify-between gap-3'>
        <div className='min-w-0'>
          <div className='text-sm font-medium'>
            {props.details.reset_at > 0
              ? t('Since last reset')
              : t('Since API key creation')}
          </div>
          <div className='text-muted-foreground mt-0.5 text-xs tabular-nums'>
            {rangeLabel}
          </div>
        </div>
        <Button variant='outline' size='sm' onClick={props.onViewLogs}>
          {t('View Logs')}
          <HugeiconsIcon
            icon={ArrowRight01Icon}
            strokeWidth={2}
            aria-hidden='true'
          />
        </Button>
      </div>

      <section aria-labelledby='api-key-usage-summary'>
        <h3 id='api-key-usage-summary' className='mb-3 text-sm font-semibold'>
          {t('Summary')}
        </h3>
        <div className='grid grid-cols-2 gap-3 lg:grid-cols-4'>
          <UsageMetric
            label={t('Net Cost')}
            value={formatBillingQuota(props.details.summary.total_quota)}
            description={`${t('Charged')}: ${formatBillingQuota(
              props.details.summary.charged_quota
            )} · ${t('Refunded')}: ${formatBillingQuota(
              props.details.summary.refunded_quota
            )}`}
          />
          <UsageMetric
            label={t('Settled Requests')}
            value={formatNumber(
              props.details.summary.settled_requests,
              props.locale
            )}
            description={t('Requests with settled usage')}
          />
          <UsageMetric
            label={t('Failed Requests')}
            value={formatNumber(
              props.details.summary.failed_requests,
              props.locale
            )}
            description={t('Failed requests are not included in token totals')}
            tone={
              props.details.summary.failed_requests > 0 ? 'danger' : 'default'
            }
          />
          <UsageMetric
            label={t('Total Tokens')}
            value={formatNumber(
              props.details.summary.total_tokens,
              props.locale
            )}
            description={`${t('Input Tokens')}: ${formatNumber(
              props.details.summary.prompt_tokens,
              props.locale
            )} · ${t('Output Tokens')}: ${formatNumber(
              props.details.summary.completion_tokens,
              props.locale
            )}`}
          />
        </div>
        <div className='text-muted-foreground mt-3 flex flex-wrap gap-x-5 gap-y-1 rounded-lg border px-3 py-2 text-xs'>
          <span>
            {t('Cache Hit Rate')}: {cacheHitRate}
          </span>
          <span>
            {t('Cached Tokens')}:{' '}
            {formatNumber(props.details.summary.cache_tokens, props.locale)}
          </span>
          <span>
            {t('Cache Input Tokens')}:{' '}
            {formatNumber(
              props.details.summary.cache_input_tokens,
              props.locale
            )}
          </span>
        </div>
      </section>

      <section aria-labelledby='api-key-usage-trend'>
        <div className='mb-3'>
          <h3 id='api-key-usage-trend' className='text-sm font-semibold'>
            {t('Daily Token Trend')}
          </h3>
          <p className='text-muted-foreground mt-1 text-xs'>
            {t(
              'Input and output tokens are grouped by your local calendar day.'
            )}
          </p>
        </div>
        <div className='h-72 overflow-hidden rounded-xl border p-2'>
          {props.details.trend.length > 0 ? (
            <Suspense fallback={<Skeleton className='h-full w-full' />}>
              <LazyApiKeyUsageTrendChart
                trend={props.details.trend}
                locale={props.locale}
                inputLabel={t('Input Tokens')}
                outputLabel={t('Output Tokens')}
                ariaLabel={t('Daily input and output token usage')}
              />
            </Suspense>
          ) : (
            <Empty className='h-full border-0'>
              <EmptyHeader>
                <EmptyMedia variant='icon'>
                  <HugeiconsIcon icon={ChartBarStackedIcon} strokeWidth={2} />
                </EmptyMedia>
                <EmptyTitle>{t('No daily usage')}</EmptyTitle>
                <EmptyDescription>
                  {t(
                    'Daily usage will appear after this API key makes settled requests.'
                  )}
                </EmptyDescription>
              </EmptyHeader>
            </Empty>
          )}
        </div>
      </section>

      <section aria-labelledby='api-key-model-ranking'>
        <div className='mb-3'>
          <h3 id='api-key-model-ranking' className='text-sm font-semibold'>
            {t('Model Ranking')}
          </h3>
          <p className='text-muted-foreground mt-1 text-xs'>
            {t('Top models ordered by net cost.')}
          </p>
        </div>
        {props.details.models.length > 0 ? (
          <div className='divide-border overflow-hidden rounded-xl border'>
            {props.details.models.map((model, index) => {
              const quotaPercent =
                maxModelQuota > 0
                  ? (model.total_quota / maxModelQuota) * 100
                  : 0
              return (
                <div
                  key={model.model_name || '__unknown_model__'}
                  className='px-3 py-3'
                >
                  <div className='flex items-start justify-between gap-4'>
                    <div className='flex min-w-0 items-start gap-2.5'>
                      <span className='text-muted-foreground mt-0.5 w-5 shrink-0 text-right font-mono text-xs'>
                        {index + 1}
                      </span>
                      <div className='min-w-0'>
                        <div className='truncate text-sm font-medium'>
                          {model.model_name || t('Unknown model')}
                        </div>
                        <div className='text-muted-foreground mt-0.5 flex flex-wrap gap-x-3 gap-y-0.5 text-xs'>
                          <span>
                            {t('Settled Requests')}:{' '}
                            {formatNumber(model.settled_requests, props.locale)}
                          </span>
                          <span>
                            {t('Failed Requests')}:{' '}
                            {formatNumber(model.failed_requests, props.locale)}
                          </span>
                          <span>
                            {t('Tokens')}:{' '}
                            {formatNumber(model.total_tokens, props.locale)}
                          </span>
                        </div>
                      </div>
                    </div>
                    <span className='shrink-0 font-medium tabular-nums'>
                      {formatBillingQuota(model.total_quota)}
                    </span>
                  </div>
                  <Progress
                    value={quotaPercent}
                    aria-label={`${model.model_name || t('Unknown model')} ${t('Cost')}`}
                    className='mt-2 h-1'
                  />
                </div>
              )
            })}
          </div>
        ) : (
          <Empty className='min-h-44 border'>
            <EmptyHeader>
              <EmptyMedia variant='icon'>
                <HugeiconsIcon icon={ChartBarStackedIcon} strokeWidth={2} />
              </EmptyMedia>
              <EmptyTitle>{t('No model usage')}</EmptyTitle>
              <EmptyDescription>
                {t('Model usage will appear after this API key is used.')}
              </EmptyDescription>
            </EmptyHeader>
          </Empty>
        )}
      </section>
    </div>
  )
}

export function ApiKeyUsageDetailsDrawer(props: ApiKeyUsageDetailsDrawerProps) {
  const { t, i18n } = useTranslation()
  const navigate = useNavigate()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const timezone = Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC'
  const usageQuery = useQuery({
    queryKey: ['api-key-usage-details', props.apiKey?.id, timezone],
    enabled: props.open && Boolean(props.apiKey),
    queryFn: async () => {
      if (!props.apiKey) throw new Error(t('Failed to load usage details'))
      const result = await getApiKeyUsageDetails(props.apiKey.id, timezone)
      if (!result.success || !result.data) {
        throw new Error(result.message || t('Failed to load usage details'))
      }
      return result.data
    },
    retry: false,
  })

  const handleViewLogs = () => {
    if (!props.apiKey || !usageQuery.data) return
    void navigate({
      to: '/usage-logs/$section',
      params: { section: 'common' },
      search: {
        page: 1,
        token: props.apiKey.name,
        tokenId: props.apiKey.id,
        startTime: usageQuery.data.range_start * 1000,
        endTime: usageQuery.data.range_end * 1000,
        startWrittenAtNano: usageQuery.data.range_start_nano,
        endWrittenAtNano: usageQuery.data.range_end_nano,
        startTimeExclusive: usageQuery.data.range_start_exclusive || undefined,
      },
    })
  }

  return (
    <Sheet open={props.open} onOpenChange={props.onOpenChange}>
      <SheetContent
        className={sideDrawerContentClassName('sm:max-w-3xl')}
        aria-busy={usageQuery.isFetching}
      >
        <SheetHeader className={sideDrawerHeaderClassName('pr-12')}>
          <SheetTitle>{t('Usage Details')}</SheetTitle>
          <SheetDescription>
            {props.apiKey
              ? t(
                  'Detailed usage for {{name}} in the current tracking window.',
                  {
                    name: props.apiKey.name,
                  }
                )
              : t('API Key')}
          </SheetDescription>
        </SheetHeader>
        <div className='min-h-0 flex-1 overflow-y-auto overscroll-contain px-4 py-4 sm:px-6 sm:py-5'>
          {usageQuery.isLoading ? <UsageDetailsSkeleton /> : null}
          {usageQuery.isError ? (
            <Alert variant='destructive'>
              <HugeiconsIcon icon={Alert02Icon} strokeWidth={2} />
              <AlertTitle>{t('Failed to load usage details')}</AlertTitle>
              <AlertDescription>
                {usageQuery.error instanceof Error
                  ? usageQuery.error.message
                  : t('An unexpected error occurred')}
                <div className='mt-3'>
                  <Button
                    type='button'
                    variant='outline'
                    size='sm'
                    onClick={() => void usageQuery.refetch()}
                  >
                    {t('Retry')}
                  </Button>
                </div>
              </AlertDescription>
            </Alert>
          ) : null}
          {usageQuery.data && props.apiKey ? (
            <UsageDetailsContent
              details={usageQuery.data}
              locale={locale}
              onViewLogs={handleViewLogs}
            />
          ) : null}
        </div>
      </SheetContent>
    </Sheet>
  )
}
