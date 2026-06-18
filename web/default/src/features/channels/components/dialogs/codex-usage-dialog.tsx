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
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from 'react'
import {
  Copy,
  Check,
  RefreshCw,
  ChevronDown,
  ChevronUp,
  User,
  Mail,
  Hash,
} from 'lucide-react'
import { useTranslation } from 'react-i18next'
import dayjs from '@/lib/dayjs'
import { cn } from '@/lib/utils'
import { useCopyToClipboard } from '@/hooks/use-copy-to-clipboard'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Avatar, AvatarFallback, AvatarImage } from '@/components/ui/avatar'
import { Button } from '@/components/ui/button'
import { Progress } from '@/components/ui/progress'
import { ScrollArea } from '@/components/ui/scroll-area'
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Spinner } from '@/components/ui/spinner'
import { Dialog } from '@/components/dialog'
import { StatusBadge, type StatusBadgeProps } from '@/components/status-badge'
import {
  consumeCodexRateLimitResetCredit,
  getCodexRateLimitResetCredits,
  type CodexRateLimitResetCredit,
  type CodexRateLimitResetCreditsResponse,
  type ConsumeCodexRateLimitResetCreditResponse,
} from '../../api'

type CodexRateLimitWindow = {
  used_percent?: number
  reset_at?: number
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
  onRefresh?: () => void | Promise<void>
  isRefreshing?: boolean
}

function clampPercent(value: unknown): number {
  const v = Number(value)
  return Number.isFinite(v) ? Math.max(0, Math.min(100, v)) : 0
}

function formatUnixSeconds(unixSeconds: unknown): string {
  const v = Number(unixSeconds)
  if (!Number.isFinite(v) || v <= 0) return '-'
  try {
    return dayjs(v * 1000).format('YYYY-MM-DD HH:mm:ss')
  } catch {
    return String(unixSeconds)
  }
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
            {t('Reset at:')} {formatUnixSeconds(props.window?.reset_at)}
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

type ConsumeNotice = {
  type: 'success' | 'error'
  message: string
}

function trimDisplayValue(value: unknown): string {
  if (value == null) return ''
  return String(value).trim()
}

function formatISODateTime(value: unknown): string {
  const text = trimDisplayValue(value)
  if (!text) return '-'
  const parsed = dayjs(text)
  return parsed.isValid() ? parsed.format('YYYY-MM-DD HH:mm:ss') : '-'
}

function humanizeRawValue(value: string): string {
  return value
    .split(/[_\s-]+/)
    .filter(Boolean)
    .map((part) => part.slice(0, 1).toUpperCase() + part.slice(1))
    .join(' ')
}

function getResetCreditsPayload(
  response: CodexRateLimitResetCreditsResponse | null
) {
  const raw = response?.data
  if (!raw || typeof raw !== 'object' || Array.isArray(raw)) return null
  return raw
}

function getResetCreditID(credit: CodexRateLimitResetCredit): string {
  return trimDisplayValue(credit.id)
}

function getResetCreditTitle(
  credit: CodexRateLimitResetCredit | null,
  t: (key: string) => string
): string {
  if (!credit) return ''
  return (
    trimDisplayValue(credit.title) ||
    getResetCreditID(credit) ||
    t('Reset credit')
  )
}

function getResetCreditDescription(
  credit: CodexRateLimitResetCredit | null,
  t: (key: string) => string
): string {
  if (!credit) return ''
  return trimDisplayValue(credit.description) || t('No description')
}

function getResetCreditResetType(credit: CodexRateLimitResetCredit | null) {
  if (!credit) return ''
  return trimDisplayValue(credit.reset_type)
}

function getResetCreditStatus(credit: CodexRateLimitResetCredit | null) {
  if (!credit) return ''
  return trimDisplayValue(credit.status)
}

function getResetCreditStatusBadge(
  status: string,
  t: (key: string) => string
): { label: string; variant: StatusBadgeProps['variant'] } {
  const normalized = status.toLowerCase()
  if (normalized === 'available') {
    return { label: t('Available'), variant: 'success' }
  }
  if (normalized === 'redeemed') {
    return { label: t('Redeemed'), variant: 'neutral' }
  }
  if (normalized === 'expired') {
    return { label: t('Expired'), variant: 'warning' }
  }
  if (normalized === 'redeeming' || normalized === 'in_progress') {
    return { label: t('Redeeming'), variant: 'info' }
  }
  if (normalized === 'unavailable') {
    return { label: t('Unavailable'), variant: 'neutral' }
  }
  return {
    label: status ? humanizeRawValue(status) : t('Unknown'),
    variant: 'neutral',
  }
}

function getResetCreditProfileUserID(
  credit: CodexRateLimitResetCredit | null
): string {
  if (!credit) return ''
  return trimDisplayValue(credit.profile_user_id)
}

function getResetCreditProfileImageURL(
  credit: CodexRateLimitResetCredit | null
): string {
  if (!credit) return ''
  return trimDisplayValue(credit.profile_image_url)
}

function getResetCreditAvatarFallback(
  credit: CodexRateLimitResetCredit | null,
  t: (key: string) => string
): string {
  const title = getResetCreditTitle(credit, t)
  return title.slice(0, 1).toUpperCase() || 'R'
}

function canRedeemResetCredit(credit: CodexRateLimitResetCredit | null) {
  if (!credit) return false
  if (trimDisplayValue(credit.redeemed_at) !== '') return false

  const normalizedStatus = getResetCreditStatus(credit).toLowerCase()
  return normalizedStatus === '' || normalizedStatus === 'available'
}

function ResetCreditField(props: {
  label: string
  value: string
  mono?: boolean
}) {
  return (
    <div className='flex min-w-0 items-center gap-2'>
      <span className='text-muted-foreground flex-shrink-0'>{props.label}</span>
      <span
        className={cn('min-w-0 truncate', props.mono && 'font-mono')}
        title={props.value}
      >
        {props.value || '-'}
      </span>
    </div>
  )
}

function ResetCreditCard(props: {
  credit: CodexRateLimitResetCredit
  selected: boolean
  t: (key: string) => string
}) {
  const { credit, selected, t } = props
  const title = getResetCreditTitle(credit, t)
  const status = getResetCreditStatus(credit)
  const statusBadge = getResetCreditStatusBadge(status, t)
  const resetType = getResetCreditResetType(credit)
  const profileImageURL = getResetCreditProfileImageURL(credit)
  const profileUserID = getResetCreditProfileUserID(credit)

  return (
    <div
      className={cn('rounded-md border px-3 py-2', selected && 'bg-muted/40')}
    >
      <div className='flex min-w-0 items-start gap-3'>
        {profileImageURL && (
          <Avatar size='sm' className='mt-0.5'>
            <AvatarImage src={profileImageURL} alt={title} />
            <AvatarFallback>
              {getResetCreditAvatarFallback(credit, t)}
            </AvatarFallback>
          </Avatar>
        )}
        <div className='min-w-0 flex-1'>
          <div className='flex flex-wrap items-center gap-2'>
            <div className='min-w-0 flex-1 truncate text-sm font-medium'>
              {title}
            </div>
            <StatusBadge
              label={statusBadge.label}
              variant={statusBadge.variant}
              copyable={false}
            />
          </div>
          <div className='text-muted-foreground mt-1 text-xs'>
            {getResetCreditDescription(credit, t)}
          </div>
          <div className='text-muted-foreground mt-2 grid grid-cols-1 gap-1 text-xs md:grid-cols-2'>
            <ResetCreditField
              label={t('Credit ID:')}
              value={getResetCreditID(credit)}
              mono
            />
            <ResetCreditField label={t('Reset type:')} value={resetType} mono />
            <ResetCreditField
              label={t('Status:')}
              value={status ? humanizeRawValue(status) : '-'}
            />
            <ResetCreditField
              label={t('Granted at:')}
              value={formatISODateTime(credit.granted_at)}
            />
            <ResetCreditField
              label={t('Expires at:')}
              value={formatISODateTime(credit.expires_at)}
            />
            <ResetCreditField
              label={t('Redeem started at:')}
              value={formatISODateTime(credit.redeem_started_at)}
            />
            <ResetCreditField
              label={t('Redeemed at:')}
              value={formatISODateTime(credit.redeemed_at)}
            />
            {profileUserID && (
              <ResetCreditField
                label={t('Profile user ID:')}
                value={profileUserID}
                mono
              />
            )}
          </div>
        </div>
      </div>
    </div>
  )
}

function createRedeemRequestID(): string | undefined {
  if (
    typeof crypto !== 'undefined' &&
    typeof crypto.randomUUID === 'function'
  ) {
    return crypto.randomUUID()
  }
  return undefined
}

function getConsumeResponseData(
  response: ConsumeCodexRateLimitResetCreditResponse
) {
  const raw = response.data
  if (!raw || typeof raw !== 'object' || Array.isArray(raw)) return null
  return raw
}

function formatConsumeFailureMessage(
  response: ConsumeCodexRateLimitResetCreditResponse,
  t: (key: string) => string
): string {
  const data = getConsumeResponseData(response)
  const code = trimDisplayValue(data?.code)
  const message =
    trimDisplayValue(data?.message) || trimDisplayValue(response.message)

  if (code === 'already_redeemed') {
    return t('This reset credit has already been redeemed.')
  }
  if (code === 'no_credit') {
    return t('No reset credit is available for this account.')
  }
  if (code === 'nothing_to_reset') {
    return t('There is no active rate limit to reset.')
  }
  if (message && code) return `${message} (${code})`
  return message || code || t('Failed to reset rate limit')
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
  const [resetCreditsResponse, setResetCreditsResponse] =
    useState<CodexRateLimitResetCreditsResponse | null>(null)
  const [resetCreditsLoading, setResetCreditsLoading] = useState(false)
  const [resetCreditsError, setResetCreditsError] = useState('')
  const [selectedResetCreditId, setSelectedResetCreditId] = useState('')
  const [consumingResetCredit, setConsumingResetCredit] = useState(false)
  const [consumeNotice, setConsumeNotice] = useState<ConsumeNotice | null>(null)
  const dialogStateRef = useRef({ open, channelId })
  const resetCreditsRequestRef = useRef(0)
  const consumeResetCreditRequestRef = useRef(0)

  const payload: CodexUsagePayload | null = useMemo(() => {
    const raw = response?.data
    if (!raw || typeof raw !== 'object') return null
    return raw as CodexUsagePayload
  }, [response?.data])

  useLayoutEffect(() => {
    dialogStateRef.current = { open, channelId }
    resetCreditsRequestRef.current += 1
    consumeResetCreditRequestRef.current += 1
  }, [channelId, open])

  const isCurrentDialogRequest = useCallback(
    (requestId: number, requestedChannelId: number) => {
      const state = dialogStateRef.current
      return (
        state.open &&
        state.channelId === requestedChannelId &&
        resetCreditsRequestRef.current === requestId
      )
    },
    []
  )

  const fetchResetCredits = useCallback(async () => {
    const requestedChannelId = channelId
    if (!requestedChannelId) return

    const requestId = resetCreditsRequestRef.current + 1
    resetCreditsRequestRef.current = requestId

    setResetCreditsLoading(true)
    setResetCreditsError('')
    try {
      const res = await getCodexRateLimitResetCredits(requestedChannelId)
      if (!isCurrentDialogRequest(requestId, requestedChannelId)) return
      setResetCreditsResponse(res)
      if (!res.success) {
        setResetCreditsError(
          res.message?.trim() || t('Failed to load reset credits')
        )
      }
    } catch (error) {
      if (!isCurrentDialogRequest(requestId, requestedChannelId)) return
      setResetCreditsResponse(null)
      setResetCreditsError(
        error instanceof Error
          ? error.message
          : t('Failed to load reset credits')
      )
    } finally {
      if (isCurrentDialogRequest(requestId, requestedChannelId)) {
        setResetCreditsLoading(false)
      }
    }
  }, [channelId, isCurrentDialogRequest, t])

  useEffect(() => {
    /* eslint-disable react-hooks/set-state-in-effect */
    setResetCreditsResponse(null)
    setResetCreditsError('')
    setSelectedResetCreditId('')
    setConsumeNotice(null)
    setResetCreditsLoading(false)
    setConsumingResetCredit(false)
    /* eslint-enable react-hooks/set-state-in-effect */

    if (!open) {
      setShowRawJson(false)
      return
    }

    void fetchResetCredits()
  }, [channelId, fetchResetCredits, open])

  const resetCreditsPayload = useMemo(
    () => getResetCreditsPayload(resetCreditsResponse),
    [resetCreditsResponse]
  )

  const resetCredits = useMemo(() => {
    return (resetCreditsPayload?.credits ?? []).filter((credit) => {
      return credit && getResetCreditID(credit) !== ''
    })
  }, [resetCreditsPayload])

  const redeemableResetCredits = useMemo(() => {
    return resetCredits.filter((credit) => canRedeemResetCredit(credit))
  }, [resetCredits])

  const activeSelectedResetCreditId = useMemo(() => {
    if (redeemableResetCredits.length === 0) return ''
    const selectedStillExists = redeemableResetCredits.some(
      (credit) => getResetCreditID(credit) === selectedResetCreditId
    )
    return selectedStillExists
      ? selectedResetCreditId
      : getResetCreditID(redeemableResetCredits[0])
  }, [selectedResetCreditId, redeemableResetCredits])

  const selectedResetCredit = useMemo(() => {
    return (
      resetCredits.find(
        (credit) => getResetCreditID(credit) === activeSelectedResetCreditId
      ) ?? null
    )
  }, [activeSelectedResetCreditId, resetCredits])

  const availableResetCreditsCount = useMemo(() => {
    const count = Number(resetCreditsPayload?.available_count)
    if (Number.isFinite(count)) return count
    return redeemableResetCredits.length
  }, [redeemableResetCredits.length, resetCreditsPayload?.available_count])

  const handleConsumeResetCredit = useCallback(async () => {
    const requestedChannelId = channelId
    const requestedCreditId = activeSelectedResetCreditId
    if (!requestedChannelId || !requestedCreditId) return

    const requestId = consumeResetCreditRequestRef.current + 1
    consumeResetCreditRequestRef.current = requestId

    const isCurrentConsumeRequest = () => {
      const state = dialogStateRef.current
      return (
        state.open &&
        state.channelId === requestedChannelId &&
        consumeResetCreditRequestRef.current === requestId
      )
    }

    setConsumingResetCredit(true)
    setConsumeNotice(null)
    try {
      const res = await consumeCodexRateLimitResetCredit(requestedChannelId, {
        credit_id: requestedCreditId,
        redeem_request_id: createRedeemRequestID(),
      })
      if (!isCurrentConsumeRequest()) return
      const data = getConsumeResponseData(res)
      const code = trimDisplayValue(data?.code)

      if (code === 'reset') {
        setConsumeNotice({
          type: 'success',
          message: t('Rate limit reset successfully.'),
        })
        await Promise.all([fetchResetCredits(), Promise.resolve(onRefresh?.())])
        return
      }

      setConsumeNotice({
        type: 'error',
        message: formatConsumeFailureMessage(res, t),
      })
    } catch (error) {
      if (!isCurrentConsumeRequest()) return
      setConsumeNotice({
        type: 'error',
        message:
          error instanceof Error
            ? error.message
            : t('Failed to reset rate limit'),
      })
    } finally {
      if (isCurrentConsumeRequest()) {
        setConsumingResetCredit(false)
      }
    }
  }, [activeSelectedResetCreditId, channelId, fetchResetCredits, onRefresh, t])

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

  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
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
            onClick={() => onOpenChange(false)}
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

        {/* Rate limit reset credits */}
        <div className='rounded-lg border p-4'>
          <div className='flex flex-wrap items-start justify-between gap-3'>
            <div className='min-w-0'>
              <div className='text-sm font-medium'>
                {t('Rate Limit Resets')}
              </div>
              <p className='text-muted-foreground mt-1 text-xs'>
                {t(
                  'Use an available reset credit to clear the current Codex rate limit.'
                )}
              </p>
            </div>
            <div className='flex flex-wrap items-center gap-2'>
              <StatusBadge
                label={`${t('Available:')} ${availableResetCreditsCount}`}
                variant={
                  availableResetCreditsCount > 0 ? 'success' : 'neutral'
                }
                copyable={false}
              />
              <Button
                type='button'
                variant='outline'
                size='sm'
                onClick={fetchResetCredits}
                disabled={resetCreditsLoading}
              >
                {resetCreditsLoading ? (
                  <Spinner data-icon='inline-start' />
                ) : (
                  <RefreshCw data-icon='inline-start' />
                )}
                {t('Refresh')}
              </Button>
            </div>
          </div>

          {resetCreditsError && (
            <Alert variant='destructive' className='mt-3'>
              <AlertDescription>{resetCreditsError}</AlertDescription>
            </Alert>
          )}

          {consumeNotice && (
            <Alert
              variant={
                consumeNotice.type === 'error' ? 'destructive' : 'default'
              }
              className='mt-3'
            >
              <AlertDescription className='flex items-center gap-2'>
                <StatusBadge
                  label={
                    consumeNotice.type === 'success'
                      ? t('Success')
                      : t('Failed')
                  }
                  variant={
                    consumeNotice.type === 'success' ? 'success' : 'danger'
                  }
                  copyable={false}
                />
                <span>{consumeNotice.message}</span>
              </AlertDescription>
            </Alert>
          )}

          {resetCreditsLoading ? (
            <div className='text-muted-foreground mt-3 flex items-center gap-2 text-xs'>
              <Spinner />
              <span>{t('Loading reset credits...')}</span>
            </div>
          ) : resetCredits.length === 0 ? (
            <div className='text-muted-foreground mt-3 text-xs'>
              {t('No reset credits available.')}
            </div>
          ) : (
            <div className='mt-3 flex flex-col gap-3'>
              {redeemableResetCredits.length > 0 ? (
                <div className='flex flex-col gap-2 sm:flex-row sm:items-center'>
                  <Select
                    items={redeemableResetCredits.map((credit) => ({
                      value: getResetCreditID(credit),
                      label: getResetCreditTitle(credit, t),
                    }))}
                    value={activeSelectedResetCreditId}
                    onValueChange={(value) => {
                      if (value) setSelectedResetCreditId(value)
                    }}
                  >
                    <SelectTrigger className='w-full sm:w-[260px]'>
                      <SelectValue placeholder={t('Select a reset credit')} />
                    </SelectTrigger>
                    <SelectContent alignItemWithTrigger={false}>
                      <SelectGroup>
                        {redeemableResetCredits.map((credit) => {
                          const creditId = getResetCreditID(credit)
                          return (
                            <SelectItem key={creditId} value={creditId}>
                              {getResetCreditTitle(credit, t)}
                            </SelectItem>
                          )
                        })}
                      </SelectGroup>
                    </SelectContent>
                  </Select>
                  <Button
                    type='button'
                    size='sm'
                    onClick={handleConsumeResetCredit}
                    disabled={
                      !activeSelectedResetCreditId || consumingResetCredit
                    }
                  >
                    {consumingResetCredit ? (
                      <Spinner data-icon='inline-start' />
                    ) : (
                      <RefreshCw data-icon='inline-start' />
                    )}
                    {t('Reset rate limit')}
                  </Button>
                </div>
              ) : (
                <div className='text-muted-foreground text-xs'>
                  {t('No redeemable reset credits available.')}
                </div>
              )}

              <div className='flex max-h-80 flex-col gap-2 overflow-y-auto pr-1'>
                {resetCredits.map((credit) => {
                  const creditId = getResetCreditID(credit)
                  return (
                    <ResetCreditCard
                      key={creditId}
                      credit={credit}
                      selected={
                        creditId ===
                        (selectedResetCredit
                          ? getResetCreditID(selectedResetCredit)
                          : '')
                      }
                      t={t}
                    />
                  )
                })}
              </div>
            </div>
          )}
        </div>

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
