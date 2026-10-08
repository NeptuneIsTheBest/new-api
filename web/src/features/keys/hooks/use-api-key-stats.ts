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
import { useEffect, useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import dayjs from '@/lib/dayjs'
import { requireServerSuccess } from '@/lib/server-error-message'
import { useAuthStore } from '@/stores/auth-store'

import { getApiKeyStats } from '../api'
import { useApiKeys } from '../components/api-keys-provider'
import type { ApiKey, ApiKeyUsageStat } from '../types'

const route = getRouteApi('/_authenticated/keys/')

export function useApiKeyStats(apiKeys: ApiKey[], isPlaceholderData: boolean) {
  const { t } = useTranslation()
  const search = route.useSearch()
  const navigate = route.useNavigate()
  const userID = useAuthStore((state) => state.auth.user?.id)
  const { refreshTrigger } = useApiKeys()
  const [defaultRange] = useState(() => {
    const today = dayjs()
    return {
      startTime: today.startOf('day').valueOf(),
      endTime: today.endOf('day').valueOf(),
    }
  })
  const startTime = search.startTime ?? defaultRange.startTime
  const endTime = search.endTime ?? defaultRange.endTime

  useEffect(() => {
    if (search.startTime === undefined || search.endTime === undefined) {
      void navigate({
        search: (previous) => ({ ...previous, ...defaultRange }),
        replace: true,
      })
    }
  }, [defaultRange, navigate, search.startTime, search.endTime])

  const tokenIDs = apiKeys.map((key) => key.id).sort((a, b) => a - b)
  const startTimestamp = Math.floor(startTime / 1000)
  const endTimestamp = Math.floor(endTime / 1000)
  const enabled = Boolean(userID) && tokenIDs.length > 0 && !isPlaceholderData
  const query = useQuery({
    queryKey: [
      'keys-stats',
      userID,
      tokenIDs,
      startTimestamp,
      endTimestamp,
      refreshTrigger,
    ],
    queryFn: async ({ signal }) => {
      const result = requireServerSuccess(
        await getApiKeyStats(
          {
            token_ids: tokenIDs,
            start_timestamp: startTimestamp,
            end_timestamp: endTimestamp,
          },
          signal
        )
      )
      return result.data ?? []
    },
    enabled,
    placeholderData: () => undefined,
  })
  const statsByToken = useMemo(() => {
    const stats = new Map<number, ApiKeyUsageStat>()
    if (enabled && !query.isError) {
      for (const item of query.data ?? []) stats.set(item.token_id, item)
    }
    return stats
  }, [enabled, query.data, query.isError])

  const onRangeChange = (range: { start?: Date; end?: Date }) => {
    const start = range.start?.getTime()
    const end = range.end?.getTime()
    if (
      start === undefined ||
      end === undefined ||
      !Number.isFinite(start) ||
      !Number.isFinite(end) ||
      start < 0 ||
      end < start
    ) {
      toast.error(t('Select a valid start and end time.'))
      return
    }
    if (start === startTime && end === endTime) {
      if (enabled) void query.refetch()
      return
    }
    void navigate({
      search: (previous) => ({ ...previous, startTime: start, endTime: end }),
    })
  }

  return {
    statsByToken,
    start: new Date(startTime),
    end: new Date(endTime),
    onRangeChange,
    isLoading: isPlaceholderData || (enabled && query.isPending),
    isFetching: query.isFetching,
    isError: enabled && query.isError,
    canRefresh: enabled,
    refetch: query.refetch,
  }
}
