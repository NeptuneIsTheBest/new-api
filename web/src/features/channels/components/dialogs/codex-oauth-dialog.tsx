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
import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation } from '@tanstack/react-query'
import { ExternalLink, Loader2, LogIn } from 'lucide-react'
import { useId, useLayoutEffect, useRef, useState } from 'react'
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'
import { z } from 'zod'

import { CopyButton } from '@/components/copy-button'
import { Dialog } from '@/components/dialog'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import {
  Form,
  FormControl,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import { handleServerError } from '@/lib/handle-server-error'
import { tryPrettyJson } from '@/lib/utils'

import { completeCodexOAuth, startCodexOAuth } from '../../api'

type CodexOAuthButtonProps = {
  proxy?: string
  disabled?: boolean
  onKeyGenerated: (key: string) => void
}

export function CodexOAuthButton(props: CodexOAuthButtonProps) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)

  return (
    <>
      <Button
        type='button'
        variant='outline'
        size='sm'
        disabled={props.disabled}
        onClick={() => setOpen(true)}
      >
        <LogIn className='mr-2 size-4' />
        {t('Assisted login')}
      </Button>
      {open && !props.disabled && (
        <CodexOAuthDialog
          proxy={props.proxy}
          onClose={() => setOpen(false)}
          onKeyGenerated={props.onKeyGenerated}
        />
      )}
    </>
  )
}

function CodexOAuthDialog(
  props: Omit<CodexOAuthButtonProps, 'disabled'> & { onClose: () => void }
) {
  const { t } = useTranslation()
  const formId = useId()
  const request = useRef<AbortController | null>(null)
  const [authorizeUrl, setAuthorizeUrl] = useState('')
  const schema = z.object({
    callbackUrl: z
      .string()
      .trim()
      .min(1, t('Paste the complete callback URL with code and state.'))
      .max(16000, t('Paste the complete callback URL with code and state.')),
  })
  const form = useForm<z.infer<typeof schema>>({
    resolver: zodResolver(schema),
    defaultValues: { callbackUrl: '' },
  })

  // Layout cleanup also covers a parent drawer/channel change before passive effects.
  useLayoutEffect(() => () => request.current?.abort(), [])

  const start = useMutation({
    retry: false,
    gcTime: 0,
    mutationFn: async (signal: AbortSignal) => {
      const response = await startCodexOAuth(props.proxy?.trim() || '', signal)
      if (signal.aborted) return
      if (!response.data?.authorize_url) {
        throw new Error(
          t('Failed to start Codex authorization. Please try again.')
        )
      }
      setAuthorizeUrl(response.data.authorize_url)
    },
    onError: (error, signal) => {
      if (!signal.aborted) handleServerError(error)
    },
  })

  const complete = useMutation({
    retry: false,
    gcTime: 0,
    mutationFn: async (input: { callbackUrl: string; signal: AbortSignal }) => {
      const response = await completeCodexOAuth(input.callbackUrl, input.signal)
      if (input.signal.aborted) return
      if (!response.data?.key) {
        throw new Error(t('Codex authorization failed. Start again.'))
      }
      // Return no credentials to the mutation cache; only the form keeps the key.
      props.onKeyGenerated(tryPrettyJson(response.data.key))
      toast.success(t('Credential generated'))
      props.onClose()
    },
    onError: (error, input) => {
      if (input.signal.aborted) return
      setAuthorizeUrl('')
      form.reset()
      handleServerError(error)
    },
  })

  const busy = start.isPending || complete.isPending

  const handleClose = () => {
    request.current?.abort()
    props.onClose()
  }

  const handleStart = () => {
    request.current?.abort()
    request.current = new AbortController()
    setAuthorizeUrl('')
    form.reset()
    start.mutate(request.current.signal)
  }

  const handleComplete = form.handleSubmit((values) => {
    if (!authorizeUrl || busy) return
    try {
      const expectedState = new URL(authorizeUrl).searchParams.get('state')
      if (
        !expectedState ||
        new URL(values.callbackUrl).searchParams.get('state') !== expectedState
      ) {
        form.setError('callbackUrl', {
          message: t('Use the callback URL from this authorization link.'),
        })
        return
      }
    } catch {
      form.setError('callbackUrl', {
        message: t('Paste the complete callback URL with code and state.'),
      })
      return
    }
    request.current?.abort()
    request.current = new AbortController()
    complete.mutate({
      callbackUrl: values.callbackUrl,
      signal: request.current.signal,
    })
  })

  return (
    <Dialog
      open
      onOpenChange={(nextOpen) => {
        if (!nextOpen) handleClose()
      }}
      title={t('Codex assisted login')}
      description={t(
        'Generate a link, open it and sign in. Paste the full localhost callback URL below, even if that page cannot load.'
      )}
      bodyClassName='space-y-4'
      footer={
        <>
          <Button type='button' variant='outline' onClick={handleClose}>
            {t('Cancel')}
          </Button>
          <Button type='submit' form={formId} disabled={!authorizeUrl || busy}>
            {complete.isPending && (
              <Loader2 className='mr-2 size-4 animate-spin' />
            )}
            {t('Generate credential')}
          </Button>
        </>
      }
    >
      <div className='flex flex-wrap items-center gap-2'>
        <Button
          type='button'
          variant={authorizeUrl ? 'outline' : 'default'}
          onClick={handleStart}
          disabled={busy}
        >
          {start.isPending && <Loader2 className='mr-2 size-4 animate-spin' />}
          {t('Generate authorization link')}
        </Button>
        {authorizeUrl && (
          <>
            <Button
              variant='outline'
              render={
                <a
                  href={authorizeUrl}
                  target='_blank'
                  rel='noopener noreferrer'
                />
              }
            >
              <ExternalLink className='mr-2 size-4' />
              {t('Open authorization page')}
            </Button>
            <CopyButton
              value={authorizeUrl}
              variant='outline'
              size='default'
              aria-label={t('Copy authorization link')}
            >
              {t('Copy authorization link')}
            </CopyButton>
          </>
        )}
      </div>
      <p className='text-muted-foreground text-sm'>
        {t(
          'Authorization links expire after 10 minutes. Generate a new link to retry.'
        )}
      </p>
      <Form {...form}>
        <form
          id={formId}
          onSubmit={(event) => {
            // Portal events still bubble through the enclosing channel form.
            event.stopPropagation()
            void handleComplete(event)
          }}
        >
          <FormField
            control={form.control}
            name='callbackUrl'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Callback URL')}</FormLabel>
                <FormControl>
                  <Input
                    {...field}
                    disabled={!authorizeUrl || busy}
                    placeholder='http://localhost:1455/auth/callback?code=...&state=...'
                    autoComplete='off'
                    spellCheck={false}
                  />
                </FormControl>
                <FormMessage />
              </FormItem>
            )}
          />
        </form>
      </Form>
      <Alert>
        <AlertDescription>
          {t(
            'The generated credential is filled into the key field. Save the channel to apply it.'
          )}
        </AlertDescription>
      </Alert>
    </Dialog>
  )
}
