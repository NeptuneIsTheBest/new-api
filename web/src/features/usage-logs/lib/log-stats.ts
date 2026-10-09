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
import { queryOptions } from '@tanstack/react-query'

import { requireServerSuccess } from '@/lib/server-error-message'

import { getLogStats, getUserLogStats } from '../api'
import { DEFAULT_LOG_STATS } from '../constants'
import type { GetLogStatsParams } from '../types'
import { buildApiParams } from './utils'

export function buildLogStatsParams(
  searchParams: Record<string, unknown>,
  isAdmin: boolean
): GetLogStatsParams {
  const {
    p: _page,
    page_size: _pageSize,
    ...params
  } = buildApiParams({
    page: 1,
    pageSize: 1,
    searchParams,
    isAdmin,
  })
  return { ...params, type: params.type ?? 0 }
}

export function logStatsQueryOptions(
  userId: number | undefined,
  isAdmin: boolean,
  params: GetLogStatsParams
) {
  return queryOptions({
    queryKey: ['usage-logs-stats', userId, isAdmin, params],
    queryFn: async ({ signal }) => {
      const result = requireServerSuccess(
        await (isAdmin
          ? getLogStats(params, signal)
          : getUserLogStats(params, signal))
      )
      return result.data ?? DEFAULT_LOG_STATS
    },
  })
}
