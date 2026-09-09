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
import { Analytics01Icon } from '@hugeicons/core-free-icons'
import { HugeiconsIcon } from '@hugeicons/react'
import { Check, Copy, Loader2 } from 'lucide-react'
import { useState, useCallback } from 'react'
import { useTranslation } from 'react-i18next'

import { BadgeCell } from '@/components/data-table'
import { MaskedValueTrigger } from '@/components/masked-value-display'
import { StatusBadge } from '@/components/status-badge'
import { Button } from '@/components/ui/button'
import {
  Popover,
  PopoverContent,
  PopoverTitle,
  PopoverTrigger,
} from '@/components/ui/popover'
import { Separator } from '@/components/ui/separator'
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from '@/components/ui/tooltip'
import { toIntlLocale } from '@/i18n/languages'
import { copyToClipboard } from '@/lib/copy-to-clipboard'
import { formatBillingCurrencyFromUSD } from '@/lib/currency'
import { formatNumber } from '@/lib/format'
import { cn } from '@/lib/utils'
import { useSystemConfigStore } from '@/stores/system-config-store'

import type { ApiKey } from '../types'
import { useApiKeys } from './api-keys-provider'

export function ApiKeyCell({ apiKey }: { apiKey: ApiKey }) {
  const { t } = useTranslation()
  const {
    resolveRealKey,
    resolvedKeys,
    loadingKeys,
    copiedKeyId,
    markKeyCopied,
  } = useApiKeys()
  const [popoverOpen, setPopoverOpen] = useState(false)

  const isLoading = !!loadingKeys[apiKey.id]
  const resolvedFullKey = resolvedKeys[apiKey.id]
  const isCopied = copiedKeyId === apiKey.id
  const maskedKey = `sk-${apiKey.key}`

  const handlePopoverOpen = useCallback(
    (open: boolean) => {
      setPopoverOpen(open)
      if (open && !resolvedFullKey) {
        resolveRealKey(apiKey.id)
      }
    },
    [resolvedFullKey, resolveRealKey, apiKey.id]
  )

  const handleCopy = useCallback(async () => {
    const realKey = resolvedFullKey || (await resolveRealKey(apiKey.id))
    if (!realKey) return

    const ok = await copyToClipboard(realKey)
    if (ok) markKeyCopied(apiKey.id)
  }, [resolvedFullKey, resolveRealKey, apiKey.id, markKeyCopied])

  let copyIcon = <Copy className='size-3.5' />
  let copyTooltip = t('Copy API key')
  if (isLoading) {
    copyIcon = <Loader2 className='size-3.5 animate-spin' />
    copyTooltip = t('Loading...')
  } else if (isCopied) {
    copyIcon = <Check className='size-3.5 text-green-600' />
    copyTooltip = t('Copied!')
  }

  return (
    <div className='flex max-w-full min-w-0 items-center'>
      <Popover open={popoverOpen} onOpenChange={handlePopoverOpen}>
        <PopoverTrigger render={<MaskedValueTrigger />}>
          <span className='truncate'>{maskedKey}</span>
        </PopoverTrigger>
        <PopoverContent
          className='w-auto max-w-[min(90vw,28rem)]'
          align='start'
        >
          <div className='space-y-2'>
            <p className='text-muted-foreground text-xs'>{t('Full API Key')}</p>
            {isLoading ? (
              <div className='flex items-center gap-2 py-2'>
                <Loader2 className='size-3.5 animate-spin' />
                <span className='text-muted-foreground text-xs'>
                  {t('Loading...')}
                </span>
              </div>
            ) : (
              <input
                readOnly
                value={resolvedFullKey || maskedKey}
                autoFocus
                onFocus={(e) => e.target.select()}
                className='bg-muted/50 w-full min-w-[280px] rounded-md border px-3 py-2 font-mono text-xs outline-none'
              />
            )}
          </div>
        </PopoverContent>
      </Popover>
      <Tooltip>
        <TooltipTrigger
          render={
            <Button
              variant='ghost'
              size='icon'
              className='size-7 shrink-0'
              onClick={handleCopy}
              disabled={isLoading}
            />
          }
        >
          {copyIcon}
        </TooltipTrigger>
        <TooltipContent>{copyTooltip}</TooltipContent>
      </Tooltip>
    </div>
  )
}

type ApiKeyRestrictionProps = {
  apiKey: ApiKey
  detailsTrigger?: 'hover' | 'click'
}

export function ModelLimitsCell(props: ApiKeyRestrictionProps) {
  const { t } = useTranslation()
  const models = props.apiKey.model_limits_enabled
    ? (props.apiKey.model_limits || '').split(',').filter(Boolean)
    : []

  return (
    <ApiKeyRestrictionCell
      items={models}
      label={t('{{count}} models', { count: models.length })}
      title={t('Models')}
      emptyLabel={t('Unlimited')}
      detailsTrigger={props.detailsTrigger}
    />
  )
}

type ApiKeyUsageCellProps = {
  stats: ApiKey['usage_stats']
  className?: string
}

export function ApiKeyUsageCell(props: ApiKeyUsageCellProps) {
  const { t, i18n } = useTranslation()
  const currencyConfig = useSystemConfigStore((state) => state.config.currency)

  if (!props.stats) {
    return <span className='text-muted-foreground'>—</span>
  }

  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const quotaPerUnit = currencyConfig.quotaPerUnit
  const periodTokens = formatNumber(props.stats.period.total_tokens, locale)
  const cumulativeTokens = formatNumber(
    props.stats.cumulative.total_tokens,
    locale
  )
  const periodCost = formatBillingCurrencyFromUSD(
    props.stats.period.net_quota / quotaPerUnit,
    {
      digitsLarge: 4,
      digitsSmall: 6,
      abbreviate: false,
      locale,
    }
  )
  const cumulativeCost = formatBillingCurrencyFromUSD(
    props.stats.cumulative.net_quota / quotaPerUnit,
    {
      digitsLarge: 4,
      digitsSmall: 6,
      abbreviate: false,
      locale,
    }
  )
  const summary = `${t('Period')}: ${t('Tokens')} ${periodTokens}, ${t('Cost')} ${periodCost}; ${t('Total:')} ${t('Tokens')} ${cumulativeTokens}, ${t('Cost')} ${cumulativeCost}`

  return (
    <Tooltip>
      <TooltipTrigger
        type='button'
        aria-label={summary}
        data-api-key-usage-cell=''
        className={cn(
          'focus-visible:ring-ring/50 flex w-max min-w-full max-w-60 cursor-help items-center gap-3 rounded-md py-0.5 text-xs focus-visible:ring-[3px] focus-visible:outline-none [&_[data-icon]]:size-5 [&_[data-icon]]:shrink-0',
          props.className
        )}
      >
        <HugeiconsIcon
          icon={Analytics01Icon}
          strokeWidth={2}
          aria-hidden='true'
          data-icon='inline-start'
          className='text-muted-foreground'
        />
        <span className='grid min-w-0 flex-1 grid-cols-[auto_minmax(0,1fr)] items-baseline gap-x-3 gap-y-1'>
          <span className='text-muted-foreground'>{t('Tokens')}</span>
          <span
            data-api-key-usage-value='tokens'
            className='min-w-0 truncate text-right font-mono font-medium tabular-nums'
          >
            {periodTokens}
          </span>
          <span className='text-muted-foreground'>{t('Cost')}</span>
          <span
            data-api-key-usage-value='cost'
            className='min-w-0 truncate text-right font-mono font-medium tabular-nums'
          >
            {periodCost}
          </span>
        </span>
      </TooltipTrigger>
      <TooltipContent
        role='tooltip'
        variant='card'
        sideOffset={8}
        className='w-72 max-w-[calc(100vw-2rem)]'
      >
        <div className='flex min-w-0 flex-col gap-2'>
          <p className='font-medium'>{t('Period')}</p>
          <dl className='grid grid-cols-[minmax(0,1fr)_minmax(0,2fr)] items-baseline gap-x-3 gap-y-1.5'>
            <dt className='text-muted-foreground'>{t('Tokens')}</dt>
            <dd className='min-w-0 text-right font-mono font-medium wrap-anywhere tabular-nums'>
              {periodTokens}
            </dd>
            <dt className='text-muted-foreground'>{t('Cost')}</dt>
            <dd className='min-w-0 text-right font-mono font-medium wrap-anywhere tabular-nums'>
              {periodCost}
            </dd>
          </dl>
        </div>
        <Separator />
        <div className='flex min-w-0 flex-col gap-2'>
          <p className='font-medium'>{t('Total')}</p>
          <dl className='grid grid-cols-[minmax(0,1fr)_minmax(0,2fr)] items-baseline gap-x-3 gap-y-1.5'>
            <dt className='text-muted-foreground'>{t('Tokens')}</dt>
            <dd className='min-w-0 text-right font-mono font-medium wrap-anywhere tabular-nums'>
              {cumulativeTokens}
            </dd>
            <dt className='text-muted-foreground'>{t('Cost')}</dt>
            <dd className='min-w-0 text-right font-mono font-medium wrap-anywhere tabular-nums'>
              {cumulativeCost}
            </dd>
          </dl>
        </div>
      </TooltipContent>
    </Tooltip>
  )
}

export function IpRestrictionsCell(props: ApiKeyRestrictionProps) {
  const { t } = useTranslation()
  const ips = (props.apiKey.allow_ips || '')
    .split('\n')
    .map((ip) => ip.trim())
    .filter(Boolean)

  return (
    <ApiKeyRestrictionCell
      items={ips}
      label={t('{{count}} IP(s)', { count: ips.length })}
      title={t('IP Restriction')}
      emptyLabel={t('No restriction')}
      detailsTrigger={props.detailsTrigger}
    />
  )
}

function ApiKeyRestrictionCell(props: {
  items: string[]
  label: string
  title: string
  emptyLabel: string
  detailsTrigger?: 'hover' | 'click'
}) {
  if (!props.items.length) {
    if (props.detailsTrigger === 'click') {
      return (
        <span className='inline-flex items-center gap-1.5 text-xs'>
          <span className='text-muted-foreground'>{props.title}</span>
          <span>{props.emptyLabel}</span>
        </span>
      )
    }
    return (
      <StatusBadge
        label={props.emptyLabel}
        variant='neutral'
        copyable={false}
        className='-ml-1.5'
      />
    )
  }

  const details = (
    <div className='max-h-[200px] space-y-1 overflow-y-auto text-xs'>
      {props.items.map((item) => (
        <div key={item} className='font-mono break-all'>
          {item}
        </div>
      ))}
    </div>
  )

  if (props.detailsTrigger === 'click') {
    return (
      <Popover>
        <PopoverTrigger
          render={
            <Button
              variant='ghost'
              size='sm'
              aria-label={`${props.title}: ${props.label}`}
              className='h-7 max-w-full justify-start px-0 text-xs font-normal underline decoration-dotted underline-offset-4'
            />
          }
        >
          {props.label}
        </PopoverTrigger>
        <PopoverContent align='start' className='max-w-[calc(100vw-2rem)]'>
          <PopoverTitle>{props.title}</PopoverTitle>
          {details}
        </PopoverContent>
      </Popover>
    )
  }

  return (
    <Tooltip>
      <TooltipTrigger render={<BadgeCell />}>
        <StatusBadge label={props.label} variant='neutral' copyable={false} />
      </TooltipTrigger>
      <TooltipContent side='top' className='max-w-xs'>
        {details}
      </TooltipContent>
    </Tooltip>
  )
}
