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
import { z } from 'zod'

// ============================================================================
// API Key Schema & Types
// ============================================================================

export const apiKeyUsageSchema = z.object({
  total_tokens: z.number().nullable(),
  total_quota: z.number(),
  reset_at: z.number(),
})

export const apiKeyUsageSummarySchema = z.object({
  settled_requests: z.number(),
  failed_requests: z.number(),
  prompt_tokens: z.number(),
  completion_tokens: z.number(),
  total_tokens: z.number(),
  cache_tokens: z.number(),
  cache_input_tokens: z.number(),
  cache_hit_rate: z.number(),
  charged_quota: z.number(),
  refunded_quota: z.number(),
  total_quota: z.number(),
})

export const apiKeyUsageTrendSchema = z.object({
  bucket: z.string().regex(/^\d{4}-\d{2}-\d{2}$/),
  settled_requests: z.number(),
  prompt_tokens: z.number(),
  completion_tokens: z.number(),
  total_tokens: z.number(),
  total_quota: z.number(),
})

export const apiKeyUsageModelSchema = z.object({
  model_name: z.string(),
  settled_requests: z.number(),
  failed_requests: z.number(),
  prompt_tokens: z.number(),
  completion_tokens: z.number(),
  total_tokens: z.number(),
  total_quota: z.number(),
})

export const apiKeyUsageDetailsSchema = z.object({
  available: z.boolean(),
  reset_at: z.number(),
  range_start: z.number(),
  range_start_nano: z
    .string()
    .regex(/^[1-9]\d*$/)
    .optional(),
  range_start_exclusive: z.boolean(),
  range_end: z.number(),
  range_end_nano: z.string().regex(/^[1-9]\d*$/),
  timezone: z.string(),
  bucket_unit: z.literal('day'),
  summary: apiKeyUsageSummarySchema,
  trend: z.array(apiKeyUsageTrendSchema),
  models: z.array(apiKeyUsageModelSchema),
})

export const apiKeySchema = z.object({
  id: z.number(),
  name: z.string(),
  key: z.string(),
  status: z.number(), // 1: enabled, 2: disabled, 3: expired, 4: exhausted
  remain_quota: z.number(),
  used_quota: z.number(),
  unlimited_quota: z.boolean(),
  expired_time: z.number(), // -1 for never expires
  created_time: z.number(),
  accessed_time: z.number(),
  group: z.string().nullish().default(''),
  cross_group_retry: z
    .preprocess((v) => {
      if (v === 1) return true
      if (v === 0) return false
      return v
    }, z.boolean())
    .optional()
    .default(false),
  model_limits_enabled: z.boolean(),
  model_limits: z.string().nullish().default(''),
  allow_ips: z.string().nullish().default(''),
  usage: apiKeyUsageSchema.optional(),
})

export type ApiKey = z.infer<typeof apiKeySchema>
export type ApiKeyUsage = z.infer<typeof apiKeyUsageSchema>
export type ApiKeyUsageDetails = z.infer<typeof apiKeyUsageDetailsSchema>
export type ApiKeyUsageTrend = z.infer<typeof apiKeyUsageTrendSchema>

// ============================================================================
// API Request/Response Types
// ============================================================================

export interface ApiResponse<T = unknown> {
  success: boolean
  message?: string
  data?: T
}

export interface GetApiKeysParams {
  p?: number
  size?: number
}

export interface GetApiKeysResponse {
  success: boolean
  message?: string
  data?: {
    items: ApiKey[]
    total: number
    page: number
    page_size: number
  }
}

export interface SearchApiKeysParams {
  keyword?: string
  token?: string
  p?: number
  size?: number
}

export interface ApiKeyFormData {
  name: string
  remain_quota: number
  expired_time: number
  unlimited_quota: boolean
  model_limits_enabled: boolean
  model_limits: string
  allow_ips: string
  group: string
  cross_group_retry: boolean
}

// ============================================================================
// Dialog Types
// ============================================================================

export type ApiKeysDialogType =
  | 'create'
  | 'update'
  | 'delete'
  | 'batch-delete'
  | 'cc-switch'
  | 'reset-usage'
  | 'usage-details'
