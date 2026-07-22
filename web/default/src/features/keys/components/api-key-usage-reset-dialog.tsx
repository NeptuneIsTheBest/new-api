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
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from '@/components/ui/alert-dialog'
import { Spinner } from '@/components/ui/spinner'

import { resetApiKeyUsage } from '../api'
import { ERROR_MESSAGES, SUCCESS_MESSAGES } from '../constants'
import { useApiKeys } from './api-keys-provider'

export function ApiKeyUsageResetDialog() {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const { open, setOpen, currentRow } = useApiKeys()

  const resetMutation = useMutation({
    mutationFn: resetApiKeyUsage,
    onSuccess: (result, tokenId) => {
      if (!result.success) {
        toast.error(result.message || t(ERROR_MESSAGES.RESET_USAGE_FAILED))
        return
      }
      toast.success(t(SUCCESS_MESSAGES.API_KEY_USAGE_RESET))
      setOpen(null)
      void queryClient.invalidateQueries({ queryKey: ['keys'] })
      void queryClient.invalidateQueries({
        queryKey: ['api-key-usage-details', tokenId],
      })
    },
    onError: () => {
      toast.error(t(ERROR_MESSAGES.RESET_USAGE_FAILED))
    },
  })

  return (
    <AlertDialog
      open={open === 'reset-usage'}
      onOpenChange={(isOpen) => !isOpen && setOpen(null)}
    >
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{t('Confirm usage reset')}</AlertDialogTitle>
          <AlertDialogDescription>
            {t(
              'Reset the displayed usage for {{name}}? Historical logs, remaining quota, and API key access will stay unchanged.',
              { name: currentRow?.name || '-' }
            )}
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel disabled={resetMutation.isPending}>
            {t('Cancel')}
          </AlertDialogCancel>
          <AlertDialogAction
            onClick={() => {
              if (currentRow) resetMutation.mutate(currentRow.id)
            }}
            disabled={!currentRow || resetMutation.isPending}
            variant='destructive'
          >
            {resetMutation.isPending && <Spinner data-icon='inline-start' />}
            {t('Reset')}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}
