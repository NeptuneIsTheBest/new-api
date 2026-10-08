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

import { CompactDateTimeRangePicker } from '@/components/compact-date-time-range-picker'
import { Button } from '@/components/ui/button'

import type { useApiKeyStats } from '../hooks/use-api-key-stats'

export function ApiKeyStatsFilter(props: {
  stats: ReturnType<typeof useApiKeyStats>
}) {
  const { t } = useTranslation()
  return (
    <>
      <CompactDateTimeRangePicker
        start={props.stats.start}
        end={props.stats.end}
        onChange={props.stats.onRangeChange}
        className='sm:w-auto'
      />
      <Button
        variant='outline'
        disabled={!props.stats.canRefresh || props.stats.isFetching}
        onClick={() => void props.stats.refetch()}
      >
        {props.stats.isError ? t('Retry') : t('Refresh Stats')}
      </Button>
      {props.stats.isError && (
        <span role='status' className='text-destructive text-sm'>
          {t('Failed to load statistics.')}
        </span>
      )}
    </>
  )
}
