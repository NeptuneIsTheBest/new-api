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
import type { ReactNode } from 'react'

import { api } from '@/lib/api'
import {
  GROUP_COLORS_QUERY_KEY,
  isGroupColors,
  type GroupColors,
} from '@/lib/group-colors'
import { ROLE } from '@/lib/roles'
import { requireServerSuccess } from '@/lib/server-error-message'
import { useAuthStore } from '@/stores/auth-store'

import { GroupColorsContext } from './group-colors-context'

const EMPTY_GROUP_COLORS: GroupColors = {}

export function GroupColorsProvider(props: { children: ReactNode }) {
  const userId = useAuthStore((state) => state.auth.user?.id)
  const role = useAuthStore((state) => state.auth.user?.role ?? ROLE.GUEST)
  const group = useAuthStore((state) => state.auth.user?.group ?? '')
  // Public pages may intentionally leave authentication bootstrap idle.
  const ready = useAuthStore(
    (state) => state.auth.bootstrapState !== 'checking'
  )

  let endpoint = '/api/user/groups'
  if (role >= ROLE.ADMIN) {
    endpoint = '/api/group/'
  } else if (userId != null) {
    endpoint = '/api/user/self/groups'
  }

  const colors = useQuery({
    queryKey: [...GROUP_COLORS_QUERY_KEY, userId ?? null, role, group],
    enabled: ready,
    queryFn: async () => {
      const response = await api.get<{
        success: boolean
        message?: string
        group_colors?: unknown
      }>(endpoint)
      const data = requireServerSuccess(response.data)
      return isGroupColors(data.group_colors)
        ? data.group_colors
        : EMPTY_GROUP_COLORS
    },
    staleTime: 5 * 60 * 1000,
    meta: { errorToast: false },
  })

  return (
    <GroupColorsContext
      value={ready ? (colors.data ?? EMPTY_GROUP_COLORS) : EMPTY_GROUP_COLORS}
    >
      {props.children}
    </GroupColorsContext>
  )
}
