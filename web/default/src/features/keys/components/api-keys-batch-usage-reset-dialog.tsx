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
import { useMutation, useQueryClient } from '@tanstack/react-query'
import type { Table } from '@tanstack/react-table'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { ConfirmDialog } from '@/components/confirm-dialog'

import { batchResetApiKeyUsage } from '../api'
import { ERROR_MESSAGES, SUCCESS_MESSAGES } from '../constants'
import type { ApiKey } from '../types'

type ApiKeysBatchUsageResetDialogProps<TData> = {
  open: boolean
  onOpenChange: (open: boolean) => void
  table: Table<TData>
}

export function ApiKeysBatchUsageResetDialog<TData>(
  props: ApiKeysBatchUsageResetDialogProps<TData>
) {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const selectedRows = props.table.getFilteredSelectedRowModel().rows

  const resetMutation = useMutation({
    mutationFn: batchResetApiKeyUsage,
    onSuccess: (result, ids) => {
      if (!result.success) {
        toast.error(result.message || t(ERROR_MESSAGES.RESET_USAGE_FAILED))
        return
      }

      const count = result.data ?? ids.length
      toast.success(t(SUCCESS_MESSAGES.API_KEY_USAGE_BATCH_RESET, { count }))
      props.table.resetRowSelection()
      props.onOpenChange(false)
      void queryClient.invalidateQueries({ queryKey: ['keys'] })
    },
    onError: () => {
      toast.error(t(ERROR_MESSAGES.RESET_USAGE_FAILED))
    },
  })

  const handleConfirm = () => {
    const ids = selectedRows.map((row) => (row.original as ApiKey).id)
    if (ids.length === 0) return
    resetMutation.mutate(ids)
  }

  return (
    <ConfirmDialog
      destructive
      open={props.open}
      onOpenChange={props.onOpenChange}
      handleConfirm={handleConfirm}
      isLoading={resetMutation.isPending}
      disabled={selectedRows.length === 0}
      className='max-w-md'
      title={t('Confirm usage reset')}
      desc={t(
        'Reset the displayed usage for {{count}} selected API key(s)? Historical logs, remaining quota, and API key access will stay unchanged.',
        { count: selectedRows.length }
      )}
      confirmText={t('Reset')}
    />
  )
}
