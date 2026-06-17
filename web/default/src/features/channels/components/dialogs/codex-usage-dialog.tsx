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
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  Copy,
  Check,
  RefreshCw,
  ChevronDown,
  ChevronUp,
  User,
  Mail,
  Hash,
  Zap,
  Clock,
} from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'
import dayjs from '@/lib/dayjs'
import { useCopyToClipboard } from '@/hooks/use-copy-to-clipboard'
import { Button } from '@/components/ui/button'
import { Progress } from '@/components/ui/progress'
import { ScrollArea } from '@/components/ui/scroll-area'
import { ConfirmDialog } from '@/components/confirm-dialog'
import { Dialog } from '@/components/dialog'
import { StatusBadge, type StatusBadgeProps } from '@/components/status-badge'
import {
  getCodexResetCredits,
  consumeCodexResetCredit,
} from '@/features/channels/api'

type CodexRateLimitWindow = {
  used_percent?: number
  reset_at?: string
  reset_after_seconds?: number
  limit_window_seconds?: number
}

type CodexRateLimit = {
  plan_type?: string
  allowed?: boolean
  limit_reached?: boolean
  primary_window?: CodexRateLimitWindow
  secondary_window?: CodexRateLimitWindow
}

type CodexAdditionalRateLimit = {
  limit_name?: string
  metered_feature?: string
  rate_limit?: CodexRateLimit
  primary_window?: CodexRateLimitWindow
  secondary_window?: CodexRateLimitWindow
  plan_type?: string
}

type CodexUsagePayload = {
  plan_type?: string
  user_id?: string
  email?: string
  account_id?: string
  rate_limit?: CodexRateLimit
  additional_rate_limits?: CodexAdditionalRateLimit[]
}

type CodexResetCredit = {
  id?: string
  status?: string
  granted_at?: string
  expires_at?: string
}

type CodexResetCreditsState = {
  channelId: number
  data: CodexResetCredit[]
}

type CodexSelectedCreditState = {
  channelId: number
  creditId: string
}

export type CodexUsageDialogData = {
  success: boolean
  message?: string
  upstream_status?: number
  data?: Record<string, unknown>
}

type CodexUsageDialogProps = {
  open: boolean
  onOpenChange: (open: boolean) => void
  channelName?: string
  channelId?: number
  response: CodexUsageDialogData | null
  onRefresh?: () => void
  isRefreshing?: boolean
}

function clampPercent(value: unknown): number {
  const v = Number(value)
  return Number.isFinite(v) ? Math.max(0, Math.min(100, v)) : 0
}

function formatIsoDateTime(value: unknown): string {
  if (value == null || typeof value === 'number') return '-'

  const text = String(value).trim()
  if (!text) return '-'
  if (Number.isFinite(Number(text))) return '-'

  const parsed = dayjs(text)
  return parsed.isValid() ? parsed.format('YYYY-MM-DD HH:mm:ss') : text
}

function formatDurationSeconds(
  seconds: unknown,
  t: (key: string) => string
): string {
  const s = Number(seconds)
  if (!Number.isFinite(s) || s <= 0) return '-'

  const total = Math.floor(s)
  const hours = Math.floor(total / 3600)
  const minutes = Math.floor((total % 3600) / 60)
  const secs = total % 60

  if (hours > 0) return `${hours}${t('h')} ${minutes}${t('m')}`
  if (minutes > 0) return `${minutes}${t('m')} ${secs}${t('s')}`
  return `${secs}${t('s')}`
}

function normalizePlanType(value: unknown): string {
  if (value == null) return ''
  return String(value).trim().toLowerCase()
}

function classifyWindowByDuration(
  windowData?: CodexRateLimitWindow | null
): 'weekly' | 'fiveHour' | null {
  const seconds = Number(windowData?.limit_window_seconds)
  if (!Number.isFinite(seconds) || seconds <= 0) return null
  return seconds >= 24 * 60 * 60 ? 'weekly' : 'fiveHour'
}

type RateLimitSource = {
  plan_type?: string
  rate_limit?: CodexRateLimit
}

function resolveRateLimitWindows(data: RateLimitSource | null): {
  fiveHourWindow: CodexRateLimitWindow | null
  weeklyWindow: CodexRateLimitWindow | null
} {
  const rateLimit = data?.rate_limit ?? {}
  const primary = rateLimit?.primary_window ?? null
  const secondary = rateLimit?.secondary_window ?? null
  const windows = [primary, secondary].filter(Boolean) as CodexRateLimitWindow[]
  const planType = normalizePlanType(data?.plan_type ?? rateLimit?.plan_type)

  let fiveHourWindow: CodexRateLimitWindow | null = null
  let weeklyWindow: CodexRateLimitWindow | null = null

  for (const w of windows) {
    const bucket = classifyWindowByDuration(w)
    if (bucket === 'fiveHour' && !fiveHourWindow) {
      fiveHourWindow = w
      continue
    }
    if (bucket === 'weekly' && !weeklyWindow) {
      weeklyWindow = w
    }
  }

  if (planType === 'free') {
    if (!weeklyWindow) weeklyWindow = primary ?? secondary ?? null
    return { fiveHourWindow: null, weeklyWindow }
  }

  if (!fiveHourWindow && !weeklyWindow) {
    return { fiveHourWindow: primary, weeklyWindow: secondary }
  }

  if (!fiveHourWindow) {
    fiveHourWindow = windows.find((w) => w !== weeklyWindow) ?? null
  }
  if (!weeklyWindow) {
    weeklyWindow = windows.find((w) => w !== fiveHourWindow) ?? null
  }

  return { fiveHourWindow, weeklyWindow }
}

const PLAN_TYPE_BADGE: Record<
  string,
  { label: string; variant: StatusBadgeProps['variant'] }
> = {
  enterprise: { label: 'Enterprise', variant: 'success' },
  team: { label: 'Team', variant: 'info' },
  prolite: { label: 'Pro 5x', variant: 'blue' },
  pro: { label: 'Pro 20x', variant: 'blue' },
  plus: { label: 'Plus', variant: 'purple' },
  free: { label: 'Free', variant: 'warning' },
}

function getAccountTypeBadge(
  value: unknown,
  t: (key: string) => string
): { label: string; variant: StatusBadgeProps['variant'] } {
  const normalized = normalizePlanType(value)
  return (
    PLAN_TYPE_BADGE[normalized] ?? {
      label: String(value || '') || t('Unknown'),
      variant: 'neutral' as const,
    }
  )
}

function windowLabel(windowData?: CodexRateLimitWindow | null) {
  const percent = clampPercent(windowData?.used_percent)
  const variant: StatusBadgeProps['variant'] =
    percent >= 95 ? 'danger' : percent >= 80 ? 'warning' : 'info'
  return { percent, variant }
}

type RateLimitWindowProps = {
  title: string
  window?: CodexRateLimitWindow | null
}

function RateLimitWindow(props: RateLimitWindowProps) {
  const { t } = useTranslation()
  const hasData =
    !!props.window &&
    typeof props.window === 'object' &&
    Object.keys(props.window).length > 0
  const { percent, variant } = windowLabel(props.window)

  return (
    <div className='rounded-lg border p-4'>
      <div className='flex items-center justify-between gap-2'>
        <div className='text-sm font-medium'>{props.title}</div>
        <StatusBadge label={`${percent}%`} variant={variant} copyable={false} />
      </div>
      <div className='mt-3'>
        <Progress
          value={percent}
          aria-label={`${props.title} usage: ${percent}%`}
        />
      </div>
      {hasData ? (
        <div className='text-muted-foreground mt-2 space-y-1 text-xs'>
          <div>
            {t('Reset at:')} {formatIsoDateTime(props.window?.reset_at)}
          </div>
          <div>
            {t('Resets in:')}{' '}
            {formatDurationSeconds(props.window?.reset_after_seconds, t)}
          </div>
          <div>
            {t('Window:')}{' '}
            {formatDurationSeconds(props.window?.limit_window_seconds, t)}
          </div>
        </div>
      ) : (
        <div className='text-muted-foreground mt-2 text-xs'>-</div>
      )}
    </div>
  )
}

type RateLimitGroupSectionProps = {
  title: string
  description?: string
  source: RateLimitSource | null
  meteredFeature?: string
}

function RateLimitGroupSection(props: RateLimitGroupSectionProps) {
  const { t } = useTranslation()
  const { fiveHourWindow, weeklyWindow } = resolveRateLimitWindows(props.source)

  return (
    <section className='space-y-3'>
      <div className='space-y-1'>
        <div className='text-sm font-semibold'>{props.title}</div>
        {(props.description || props.meteredFeature) && (
          <div className='text-muted-foreground flex flex-wrap items-center gap-2 text-xs'>
            {props.description && <span>{props.description}</span>}
            {props.meteredFeature && (
              <span className='bg-muted/60 inline-flex max-w-full items-center gap-2 rounded-md px-2 py-0.5'>
                <span className='text-[11px]'>metered_feature</span>
                <span className='min-w-0 font-mono text-xs break-all'>
                  {props.meteredFeature}
                </span>
              </span>
            )}
          </div>
        )}
      </div>
      <div className='grid grid-cols-1 gap-4 md:grid-cols-2'>
        <RateLimitWindow title={t('5-Hour Window')} window={fiveHourWindow} />
        <RateLimitWindow title={t('Weekly Window')} window={weeklyWindow} />
      </div>
    </section>
  )
}

function CopyableField(props: {
  icon: React.ReactNode
  label: string
  value?: string | null
  mono?: boolean
}) {
  const { copyToClipboard, copiedText } = useCopyToClipboard({ notify: false })
  const text = props.value?.trim() || ''
  const hasCopied = copiedText === text

  return (
    <div className='flex items-center justify-between gap-2 py-1'>
      <div className='flex min-w-0 items-center gap-2'>
        <span className='text-muted-foreground flex-shrink-0'>
          {props.icon}
        </span>
        <span className='text-muted-foreground flex-shrink-0 text-xs'>
          {props.label}
        </span>
        <span
          className={`min-w-0 truncate text-xs ${props.mono ? 'font-mono' : ''}`}
        >
          {text || '-'}
        </span>
      </div>
      {text && (
        <Button
          type='button'
          variant='ghost'
          size='sm'
          className='h-6 w-6 flex-shrink-0 p-0'
          onClick={() => copyToClipboard(text)}
        >
          {hasCopied ? (
            <Check className='h-3 w-3 text-green-600' />
          ) : (
            <Copy className='h-3 w-3' />
          )}
        </Button>
      )}
    </div>
  )
}

export function CodexUsageDialog({
  open,
  onOpenChange,
  channelName,
  channelId,
  response,
  onRefresh,
  isRefreshing,
}: CodexUsageDialogProps) {
  const { t } = useTranslation()
  const { copiedText, copyToClipboard } = useCopyToClipboard({ notify: false })
  const [showRawJson, setShowRawJson] = useState(false)
  const [resetCreditsOpenChannelId, setResetCreditsOpenChannelId] = useState<
    number | null
  >(null)
  const [resetCreditsState, setResetCreditsState] =
    useState<CodexResetCreditsState | null>(null)
  const [loadingResetCreditsChannelId, setLoadingResetCreditsChannelId] =
    useState<number | null>(null)
  const [consumingCreditState, setConsumingCreditState] =
    useState<CodexSelectedCreditState | null>(null)
  const [selectedCreditState, setSelectedCreditState] =
    useState<CodexSelectedCreditState | null>(null)
  const [consumeConfirmOpen, setConsumeConfirmOpen] = useState(false)
  const activeChannelIdRef = useRef(channelId)
  const dialogOpenRef = useRef(open)
  const resetCreditsRequestRef = useRef(0)
  const consumeResetCreditRequestRef = useRef(0)

  const resetResetCreditsState = useCallback(() => {
    resetCreditsRequestRef.current += 1
    consumeResetCreditRequestRef.current += 1
    setResetCreditsOpenChannelId(null)
    setResetCreditsState(null)
    setLoadingResetCreditsChannelId(null)
    setConsumingCreditState(null)
    setSelectedCreditState(null)
    setConsumeConfirmOpen(false)
  }, [])

  useEffect(() => {
    activeChannelIdRef.current = channelId
    resetCreditsRequestRef.current += 1
    consumeResetCreditRequestRef.current += 1
  }, [channelId])

  useEffect(() => {
    dialogOpenRef.current = open
    if (!open) {
      resetCreditsRequestRef.current += 1
      consumeResetCreditRequestRef.current += 1
    }
  }, [open])

  const payload: CodexUsagePayload | null = useMemo(() => {
    const raw = response?.data
    if (!raw || typeof raw !== 'object') return null
    return raw as CodexUsagePayload
  }, [response?.data])

  const rateLimit = payload?.rate_limit
  const accountType = payload?.plan_type ?? rateLimit?.plan_type
  const accountBadge = getAccountTypeBadge(accountType, t)
  const additionalRateLimits = (payload?.additional_rate_limits ?? []).filter(
    (item) => item && Object.keys(item).length > 0
  )

  const statusBadge = (() => {
    if (!rateLimit || Object.keys(rateLimit).length === 0) {
      return (
        <StatusBadge label={t('Pending')} variant='neutral' copyable={false} />
      )
    }
    if (rateLimit.allowed && !rateLimit.limit_reached) {
      return (
        <StatusBadge
          label={t('Available')}
          variant='success'
          copyable={false}
        />
      )
    }
    return (
      <StatusBadge label={t('Limited')} variant='danger' copyable={false} />
    )
  })()

  const errorMessage =
    response?.success === false
      ? response?.message?.trim() || t('Failed to fetch usage')
      : ''

  const rawJsonText = useMemo(() => {
    if (!response) return ''
    try {
      return JSON.stringify(
        {
          success: response.success,
          message: response.message,
          upstream_status: response.upstream_status,
          data: response.data,
        },
        null,
        2
      )
    } catch {
      return String(response?.data ?? '')
    }
  }, [response])

  const resetCreditsOpen = Boolean(
    open && channelId && resetCreditsOpenChannelId === channelId
  )
  const resetCreditsData =
    open && channelId && resetCreditsState?.channelId === channelId
      ? resetCreditsState.data
      : null
  const isLoadingResetCredits = Boolean(
    open && channelId && loadingResetCreditsChannelId === channelId
  )
  const selectedCreditId =
    open && channelId && selectedCreditState?.channelId === channelId
      ? selectedCreditState.creditId
      : null
  const isConsuming = Boolean(
    open && channelId && consumingCreditState?.channelId === channelId
  )
  const effectiveConsumeConfirmOpen = Boolean(
    consumeConfirmOpen && selectedCreditId
  )

  const isActiveDialogChannel = useCallback((targetChannelId: number) => {
    return (
      dialogOpenRef.current && activeChannelIdRef.current === targetChannelId
    )
  }, [])

  const fetchResetCredits = useCallback(
    async (targetChannelId = channelId) => {
      if (!targetChannelId) return

      const requestId = ++resetCreditsRequestRef.current
      setLoadingResetCreditsChannelId(targetChannelId)
      try {
        const res = await getCodexResetCredits(targetChannelId)
        if (
          resetCreditsRequestRef.current !== requestId ||
          !isActiveDialogChannel(targetChannelId)
        ) {
          return
        }

        if (res.success && Array.isArray(res.data)) {
          setResetCreditsState({
            channelId: targetChannelId,
            data: res.data as CodexResetCredit[],
          })
        } else if (res.success && res.data && typeof res.data === 'object') {
          const obj = res.data as Record<string, unknown>
          setResetCreditsState({
            channelId: targetChannelId,
            data: (Array.isArray(obj.credits)
              ? obj.credits
              : []) as CodexResetCredit[],
          })
        } else {
          setResetCreditsState({ channelId: targetChannelId, data: [] })
          if (!res.success) {
            toast.error(res.message || t('Failed to fetch reset credits'))
          }
        }
      } catch {
        if (
          resetCreditsRequestRef.current === requestId &&
          isActiveDialogChannel(targetChannelId)
        ) {
          setResetCreditsState({ channelId: targetChannelId, data: [] })
          toast.error(t('Failed to fetch reset credits'))
        }
      } finally {
        if (
          resetCreditsRequestRef.current === requestId &&
          isActiveDialogChannel(targetChannelId)
        ) {
          setLoadingResetCreditsChannelId(null)
        }
      }
    },
    [channelId, isActiveDialogChannel, t]
  )

  const handleToggleResetCredits = () => {
    if (!channelId) return

    const next = !resetCreditsOpen
    setResetCreditsOpenChannelId(next ? channelId : null)
    if (next && resetCreditsData === null) {
      fetchResetCredits(channelId)
    }
  }

  const handleConsumeClick = (creditId: string) => {
    if (!channelId || !creditId) return

    setSelectedCreditState({ channelId, creditId })
    setConsumeConfirmOpen(true)
  }

  const handleConsumeConfirm = async () => {
    const targetChannelId = channelId
    const targetCreditId = selectedCreditId
    if (!targetChannelId || !targetCreditId) return

    const requestId = ++consumeResetCreditRequestRef.current
    setConsumeConfirmOpen(false)
    setConsumingCreditState({
      channelId: targetChannelId,
      creditId: targetCreditId,
    })
    try {
      const res = await consumeCodexResetCredit(targetChannelId, targetCreditId)
      if (
        consumeResetCreditRequestRef.current !== requestId ||
        !isActiveDialogChannel(targetChannelId)
      ) {
        return
      }

      if (res.success) {
        onRefresh?.()
        setResetCreditsState(null)
        await fetchResetCredits(targetChannelId)
        toast.success(t('Rate limit reset successfully'))
      } else {
        toast.error(res.message || t('Failed to reset rate limit'))
      }
    } catch {
      if (
        consumeResetCreditRequestRef.current === requestId &&
        isActiveDialogChannel(targetChannelId)
      ) {
        toast.error(t('Failed to consume reset credit'))
      }
    } finally {
      if (
        consumeResetCreditRequestRef.current === requestId &&
        isActiveDialogChannel(targetChannelId)
      ) {
        setConsumingCreditState((current) =>
          current?.channelId === targetChannelId &&
          current.creditId === targetCreditId
            ? null
            : current
        )
        setSelectedCreditState((current) =>
          current?.channelId === targetChannelId &&
          current.creditId === targetCreditId
            ? null
            : current
        )
      }
    }
  }

  const handleDialogOpenChange = (nextOpen: boolean) => {
    if (!nextOpen) {
      resetResetCreditsState()
    }
    onOpenChange(nextOpen)
  }

  const availableCredits = (resetCreditsData ?? []).filter(
    (c) => c.status === 'available'
  )

  return (
    <Dialog
      open={open}
      onOpenChange={handleDialogOpenChange}
      title={t('Codex Account & Usage')}
      description={
        <>
          {t('Channel:')}
          <strong>{channelName || '-'}</strong>{' '}
          {channelId ? `(#${channelId})` : ''}
        </>
      }
      contentClassName='sm:max-w-3xl'
      titleClassName='flex items-center gap-2'
      contentHeight='auto'
      bodyClassName='space-y-4'
      footer={
        <>
          <Button
            type='button'
            variant='outline'
            onClick={() => handleDialogOpenChange(false)}
          >
            {t('Close')}
          </Button>
        </>
      }
    >
      <div className='space-y-4'>
        {errorMessage && (
          <div className='rounded-lg border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700 dark:border-red-800 dark:bg-red-950/30 dark:text-red-400'>
            {errorMessage}
          </div>
        )}

        {/* Account summary */}
        <div className='rounded-lg border p-4'>
          <div className='flex flex-wrap items-center justify-between gap-2'>
            <div className='flex flex-wrap items-center gap-2'>
              <StatusBadge
                label={accountBadge.label}
                variant={accountBadge.variant}
                copyable={false}
              />
              {statusBadge}
              {typeof response?.upstream_status === 'number' && (
                <StatusBadge
                  label={`${t('Status:')} ${response.upstream_status}`}
                  variant='neutral'
                  copyable={false}
                />
              )}
            </div>
            {onRefresh && (
              <Button
                type='button'
                variant='outline'
                size='sm'
                onClick={onRefresh}
                disabled={Boolean(isRefreshing)}
              >
                <RefreshCw className='mr-1.5 h-3.5 w-3.5' />
                {t('Refresh')}
              </Button>
            )}
          </div>

          {/* Account identity info */}
          <div className='bg-muted/30 mt-3 rounded-md px-3 py-2'>
            <CopyableField
              icon={<User className='h-3.5 w-3.5' />}
              label='User ID'
              value={payload?.user_id}
              mono
            />
            <CopyableField
              icon={<Mail className='h-3.5 w-3.5' />}
              label={t('Email')}
              value={payload?.email}
            />
            <CopyableField
              icon={<Hash className='h-3.5 w-3.5' />}
              label='Account ID'
              value={payload?.account_id}
              mono
            />
          </div>
        </div>

        {/* Rate limit windows */}
        <div className='space-y-5'>
          <div>
            <div className='mb-1 text-sm font-medium'>
              {t('Rate Limit Windows')}
            </div>
            <p className='text-muted-foreground mb-3 text-xs'>
              {t(
                'Tracks current account base limits and additional metered usage on Codex upstream.'
              )}
            </p>
            <RateLimitGroupSection
              title={t('Base Limits')}
              description={t('Base rate limit windows for this account.')}
              source={payload}
            />
          </div>

          {additionalRateLimits.length > 0 && (
            <div className='space-y-4 border-t pt-4'>
              <div>
                <div className='text-sm font-medium'>
                  {t('Additional Limits')}
                </div>
                <p className='text-muted-foreground text-xs'>
                  {t(
                    'Per-feature metered windows split by model or capability.'
                  )}
                </p>
              </div>
              <div className='space-y-4'>
                {additionalRateLimits.map((item, index) => {
                  const limitName =
                    item.limit_name ||
                    item.metered_feature ||
                    `${t('Additional Limit')} ${index + 1}`
                  return (
                    <div
                      key={`${limitName}-${item.metered_feature ?? ''}-${index}`}
                      className={index > 0 ? 'border-t pt-4' : ''}
                    >
                      <RateLimitGroupSection
                        title={limitName}
                        description={t('Additional metered capability')}
                        source={item}
                        meteredFeature={item.metered_feature}
                      />
                    </div>
                  )
                })}
              </div>
            </div>
          )}
        </div>

        {/* Rate Limit Reset Credits */}
        <div className='rounded-lg border'>
          <button
            type='button'
            className='hover:bg-muted/40 flex w-full items-center justify-between gap-2 p-3 transition-colors'
            onClick={handleToggleResetCredits}
          >
            <div className='flex items-center gap-2'>
              <div className='text-sm font-medium'>
                {t('Rate Limit Reset Credits')}
              </div>
              {availableCredits.length > 0 && (
                <StatusBadge
                  label={`${availableCredits.length}`}
                  variant='success'
                  copyable={false}
                />
              )}
            </div>
            <div className='flex items-center gap-2'>
              {resetCreditsOpen && (
                <Button
                  type='button'
                  variant='ghost'
                  size='sm'
                  className='h-6 w-6 p-0'
                  onClick={(e) => {
                    e.stopPropagation()
                    fetchResetCredits()
                  }}
                  disabled={isLoadingResetCredits}
                >
                  <RefreshCw
                    className={`h-3.5 w-3.5 ${isLoadingResetCredits ? 'animate-spin' : ''}`}
                  />
                </Button>
              )}
              {resetCreditsOpen ? (
                <ChevronUp className='text-muted-foreground h-4 w-4' />
              ) : (
                <ChevronDown className='text-muted-foreground h-4 w-4' />
              )}
            </div>
          </button>
          {resetCreditsOpen && (
            <div className='border-t px-3 py-3'>
              {isLoadingResetCredits ? (
                <div className='text-muted-foreground py-4 text-center text-sm'>
                  {t('Loading...')}
                </div>
              ) : resetCreditsData === null ? (
                <div className='text-muted-foreground py-4 text-center text-sm'>
                  {t('Loading...')}
                </div>
              ) : resetCreditsData.length === 0 ? (
                <div className='text-muted-foreground py-4 text-center text-sm'>
                  {t('No reset credits available')}
                </div>
              ) : (
                <div className='space-y-3'>
                  <p className='text-muted-foreground text-xs'>
                    {t(
                      'Available reset credits for this Codex account. Consume one to immediately reset rate limit windows.'
                    )}
                  </p>
                  <ScrollArea className='max-h-[40vh]'>
                    <div className='space-y-2'>
                      {resetCreditsData.map((credit, index) => {
                        const isAvailable = credit.status === 'available'
                        return (
                          <div
                            key={credit.id || index}
                            className='bg-muted/30 flex items-center justify-between gap-3 rounded-md px-3 py-2'
                          >
                            <div className='min-w-0 space-y-1 text-xs'>
                              <div className='flex items-center gap-2'>
                                <StatusBadge
                                  label={
                                    isAvailable
                                      ? t('Available')
                                      : credit.status || '-'
                                  }
                                  variant={isAvailable ? 'success' : 'neutral'}
                                  copyable={false}
                                />
                                <span className='text-muted-foreground'>
                                  {t('Credit ID:')}
                                </span>
                                <span className='truncate font-mono'>
                                  {String(credit.id ?? '').slice(-20) || '-'}
                                </span>
                              </div>
                              <div className='text-muted-foreground flex flex-wrap gap-x-4 gap-y-1'>
                                {credit.granted_at && (
                                  <span>
                                    <Clock className='mr-1 inline h-3 w-3' />
                                    {t('Granted:')}{' '}
                                    {formatIsoDateTime(credit.granted_at)}
                                  </span>
                                )}
                                {credit.expires_at && (
                                  <span>
                                    {t('Expires:')}{' '}
                                    {formatIsoDateTime(credit.expires_at)}
                                  </span>
                                )}
                              </div>
                            </div>
                            {isAvailable && (
                              <Button
                                type='button'
                                variant='outline'
                                size='sm'
                                className='flex-shrink-0'
                                onClick={() =>
                                  handleConsumeClick(credit.id || '')
                                }
                                disabled={isConsuming || !credit.id}
                              >
                                <Zap className='mr-1 h-3 w-3' />
                                {isConsuming && selectedCreditId === credit.id
                                  ? t('Consuming...')
                                  : t('Consume Reset Credit')}
                              </Button>
                            )}
                          </div>
                        )
                      })}
                    </div>
                  </ScrollArea>
                </div>
              )}
            </div>
          )}
        </div>

        <ConfirmDialog
          open={effectiveConsumeConfirmOpen}
          onOpenChange={(nextOpen) => {
            setConsumeConfirmOpen(nextOpen)
            if (!nextOpen) {
              setSelectedCreditState(null)
            }
          }}
          title={t('Consume Reset Credit')}
          desc={t(
            'Are you sure you want to consume this reset credit? This will immediately reset the rate limit windows.'
          )}
          confirmText={t('Consume Reset Credit')}
          destructive
          handleConfirm={handleConsumeConfirm}
        />

        {/* Raw JSON collapsible */}
        <div className='rounded-lg border'>
          <button
            type='button'
            className='hover:bg-muted/40 flex w-full items-center justify-between gap-2 p-3 transition-colors'
            onClick={() => setShowRawJson((v) => !v)}
          >
            <div className='text-sm font-medium'>{t('Raw JSON')}</div>
            {showRawJson ? (
              <ChevronUp className='text-muted-foreground h-4 w-4' />
            ) : (
              <ChevronDown className='text-muted-foreground h-4 w-4' />
            )}
          </button>
          {showRawJson && (
            <>
              <div className='flex justify-end border-t px-3 py-2'>
                <Button
                  type='button'
                  variant='outline'
                  size='sm'
                  onClick={() => copyToClipboard(rawJsonText)}
                  disabled={!rawJsonText}
                >
                  {copiedText === rawJsonText ? (
                    <Check className='mr-1.5 h-3.5 w-3.5 text-green-600' />
                  ) : (
                    <Copy className='mr-1.5 h-3.5 w-3.5' />
                  )}
                  {t('Copy')}
                </Button>
              </div>
              <ScrollArea className='max-h-[50vh]'>
                <pre className='bg-muted/30 m-0 p-3 text-xs break-words whitespace-pre-wrap'>
                  {rawJsonText || '-'}
                </pre>
              </ScrollArea>
            </>
          )}
        </div>
      </div>
    </Dialog>
  )
}
