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
import { useMutation } from '@tanstack/react-query'
import { ExternalLink, Loader2 } from 'lucide-react'
import { useEffect, useId, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { CopyButton } from '@/components/copy-button'
import { Dialog } from '@/components/dialog'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { handleServerError } from '@/lib/handle-server-error'
import { AuthOperationError } from '@/lib/secure-verification'
import { tryPrettyJson } from '@/lib/utils'

import {
  completeCodexOAuth,
  startCodexOAuth,
  type CodexOAuthAuthorization,
} from '../../api'

type CodexOAuthDialogProps = {
  open: boolean
  proxy?: string
  onOpenChange: (open: boolean) => void
  onKeyGenerated: (key: string) => void
}

export function CodexOAuthDialog(props: CodexOAuthDialogProps) {
  const { t } = useTranslation()
  const inputId = useId()
  const [authorization, setAuthorization] =
    useState<CodexOAuthAuthorization | null>(null)
  const [callbackUrl, setCallbackUrl] = useState('')
  const request = useRef<AbortController | null>(null)
  const start = useMutation({
    mutationFn: startCodexOAuth,
    retry: false,
    gcTime: 0,
    meta: { errorToast: false },
  })
  const complete = useMutation({
    mutationFn: (variables: {
      input: Parameters<typeof completeCodexOAuth>[0]
      signal: AbortSignal
    }) => completeCodexOAuth(variables.input, variables.signal),
    retry: false,
    gcTime: 0,
    meta: { errorToast: false },
  })
  const busy = start.isPending || complete.isPending
  const resetStart = start.reset
  const resetComplete = complete.reset

  useEffect(() => {
    if (!props.open) {
      setAuthorization(null)
      setCallbackUrl('')
      resetStart()
      resetComplete()
    }
    return () => {
      request.current?.abort()
      request.current = null
    }
  }, [props.open, resetStart, resetComplete])

  const handleOpenChange = (open: boolean) => {
    if (!open) {
      request.current?.abort()
      request.current = null
      setAuthorization(null)
      setCallbackUrl('')
      start.reset()
      complete.reset()
    }
    props.onOpenChange(open)
  }

  const handleStart = async () => {
    if (request.current) return
    const controller = new AbortController()
    request.current = controller
    setAuthorization(null)
    setCallbackUrl('')
    complete.reset()
    try {
      const result = await start.mutateAsync(controller.signal)
      if (controller.signal.aborted) return
      setAuthorization(result)
      // The shared OAuth popup expects a same-origin postMessage callback.
      // This flow returns to localhost and deliberately uses manual URL input.
      try {
        window.open(result.authorize_url, '_blank', 'noopener,noreferrer')
      } catch {
        // The visible link below also works when popups are blocked.
      }
    } catch (error) {
      if (!controller.signal.aborted) handleServerError(error)
    } finally {
      if (request.current === controller) {
        request.current = null
        start.reset()
      }
    }
  }

  const handleComplete = async () => {
    if (!authorization || !callbackUrl.trim() || request.current) return
    const controller = new AbortController()
    request.current = controller
    try {
      const result = await complete.mutateAsync({
        input: {
          flow_token: authorization.flow_token,
          input: callbackUrl.trim(),
          proxy: props.proxy,
        },
        signal: controller.signal,
      })
      if (controller.signal.aborted) return
      props.onKeyGenerated(tryPrettyJson(result.key))
      toast.success(t('Credential generated. Save the channel to apply it.'))
      handleOpenChange(false)
    } catch (error) {
      if (controller.signal.aborted) return
      // Validation failures have not spent the flow; all other failures need
      // a fresh authorization, including ambiguous transport failures.
      if (
        !(error instanceof AuthOperationError) ||
        !['CODEX_OAUTH_INPUT_INVALID', 'CODEX_OAUTH_PROXY_INVALID'].includes(
          error.code ?? ''
        )
      ) {
        setAuthorization(null)
        setCallbackUrl('')
      }
      handleServerError(error)
    } finally {
      if (request.current === controller) {
        request.current = null
        complete.reset()
      }
    }
  }

  return (
    <Dialog
      open={props.open}
      onOpenChange={handleOpenChange}
      title={t('ChatGPT subscription assisted login')}
      description={t(
        'If authorization succeeds, the generated JSON will be inserted into the key field. You still need to save the channel to persist it.'
      )}
      bodyClassName='space-y-4'
      footer={
        <>
          <Button
            type='button'
            variant='outline'
            onClick={() => handleOpenChange(false)}
          >
            {t('Cancel')}
          </Button>
          <Button
            type='button'
            onClick={handleComplete}
            disabled={!authorization || !callbackUrl.trim() || busy}
          >
            {complete.isPending && (
              <Loader2 className='animate-spin' aria-hidden='true' />
            )}
            {t('Generate credential')}
          </Button>
        </>
      }
    >
      <Alert>
        <AlertDescription>
          {t(
            'Sign in on the authorization page, then copy the full localhost callback URL here, even if that page does not load. The authorization link expires after 10 minutes.'
          )}
        </AlertDescription>
      </Alert>
      <div className='flex flex-wrap gap-2'>
        <Button
          type='button'
          variant={authorization ? 'outline' : 'default'}
          onClick={handleStart}
          disabled={busy}
        >
          {start.isPending && (
            <Loader2 className='animate-spin' aria-hidden='true' />
          )}
          {authorization ? t('Start again') : t('Start authorization')}
        </Button>
        {authorization && (
          <>
            <Button
              role='link'
              render={
                <a
                  href={authorization.authorize_url}
                  target='_blank'
                  rel='noopener noreferrer'
                />
              }
            >
              <ExternalLink aria-hidden='true' />
              {t('Open authorization page')}
            </Button>
            <CopyButton
              value={authorization.authorize_url}
              variant='outline'
              size='default'
              aria-label={t('Copy authorization link')}
            >
              {t('Copy authorization link')}
            </CopyButton>
          </>
        )}
      </div>
      <div className='space-y-2'>
        <Label htmlFor={inputId}>{t('Callback URL')}</Label>
        <Input
          id={inputId}
          value={callbackUrl}
          onChange={(event) => setCallbackUrl(event.target.value)}
          placeholder={t('Paste the full callback URL (includes code & state)')}
          disabled={!authorization || busy}
          maxLength={8192}
          autoComplete='off'
          spellCheck={false}
        />
      </div>
    </Dialog>
  )
}
