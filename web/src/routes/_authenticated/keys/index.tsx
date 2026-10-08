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
import { createFileRoute } from '@tanstack/react-router'
import z from 'zod'

import { ApiKeys } from '@/features/keys'
import { API_KEY_STATUS_OPTIONS } from '@/features/keys/constants'

const apiKeySearchSchema = z
  .object({
    page: z.number().optional().catch(1),
    pageSize: z.number().optional().catch(undefined),
    status: z
      .array(z.enum(API_KEY_STATUS_OPTIONS.map((s) => s.value as `${number}`)))
      .optional()
      .catch([]),
    filter: z.string().optional().catch(''),
    token: z.string().optional().catch(''),
    startTime: z
      .number()
      .int()
      .min(0)
      .max(8_640_000_000_000_000)
      .optional()
      .catch(undefined),
    endTime: z
      .number()
      .int()
      .min(0)
      .max(8_640_000_000_000_000)
      .optional()
      .catch(undefined),
  })
  .transform((search) => {
    if (
      search.startTime !== undefined &&
      search.endTime !== undefined &&
      search.startTime <= search.endTime
    ) {
      return search
    }
    return { ...search, startTime: undefined, endTime: undefined }
  })

export const Route = createFileRoute('/_authenticated/keys/')({
  validateSearch: apiKeySearchSchema,
  component: ApiKeys,
})
