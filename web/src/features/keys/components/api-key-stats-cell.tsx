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
import { useTranslation } from 'react-i18next'

import { QuotaDetailsPopover } from '@/components/quota-details-popover'
import { Skeleton } from '@/components/ui/skeleton'
import { toIntlLocale } from '@/i18n/languages'
import { formatQuotaWithCurrency, getCurrencyDisplay } from '@/lib/currency'
import { formatNumber } from '@/lib/format'
import { useSystemConfigStore } from '@/stores/system-config-store'

import type { ApiKeyUsageStat } from '../types'

export function ApiKeyStatsCell(props: {
  stat?: ApiKeyUsageStat
  isLoading: boolean
  kind: 'tokens' | 'quota'
}) {
  const { t, i18n } = useTranslation()
  useSystemConfigStore((state) => state.config.currency)
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)

  if (props.isLoading) {
    return <Skeleton className='h-5 w-20' aria-label={t('Loading...')} />
  }
  if (!props.stat) return <span className='text-muted-foreground'>—</span>

  if (props.kind === 'quota') {
    const { meta: currency } = getCurrencyDisplay()
    return (
      <span className='text-sm break-all tabular-nums'>
        {formatQuotaWithCurrency(props.stat.quota, {
          digitsLarge: 4,
          digitsSmall: 6,
          abbreviate: false,
          locale,
        })}
        {currency.kind === 'tokens' && ` ${t('Tokens')}`}
      </span>
    )
  }

  const total = formatNumber(props.stat.total_tokens, locale)
  return (
    <QuotaDetailsPopover
      title={t('Tokens')}
      triggerLabel={`${t('Total Tokens')}: ${total}`}
      details={[
        { label: t('Total Tokens'), value: total },
        {
          label: t('Input Tokens'),
          value: formatNumber(props.stat.input_tokens, locale),
        },
        {
          label: t('Output Tokens'),
          value: formatNumber(props.stat.output_tokens, locale),
        },
      ]}
      description={t(
        'Total input includes cache reads and writes. Statistics cover all matching consumption logs.'
      )}
    >
      <span className='min-w-0 truncate text-sm tabular-nums'>{total}</span>
    </QuotaDetailsPopover>
  )
}
