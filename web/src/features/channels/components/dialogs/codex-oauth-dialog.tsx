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
import {
  Copy01Icon,
  LinkSquare01Icon,
  Tick02Icon,
} from '@hugeicons/core-free-icons'
import { HugeiconsIcon } from '@hugeicons/react'
import { useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { Dialog } from '@/components/dialog'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Spinner } from '@/components/ui/spinner'
import { useCopyToClipboard } from '@/hooks/use-copy-to-clipboard'
import { tryPrettyJson } from '@/lib/utils'

import { completeCodexOAuth, startCodexOAuth } from '../../api'

type CodexOAuthDialogProps = {
  open: boolean
  onOpenChange: (open: boolean) => void
  channelId?: number
  onCredentialGenerated: (credential: string) => void
}

export function CodexOAuthDialog(props: CodexOAuthDialogProps) {
  const { t } = useTranslation()
  const { copiedText, copyToClipboard } = useCopyToClipboard({ notify: false })
  const [authorizeUrl, setAuthorizeUrl] = useState('')
  const [callbackInput, setCallbackInput] = useState('')
  const [isStarting, setIsStarting] = useState(false)
  const [isCompleting, setIsCompleting] = useState(false)

  useEffect(() => {
    if (props.open) return
    setAuthorizeUrl('')
    setCallbackInput('')
    setIsStarting(false)
    setIsCompleting(false)
  }, [props.open])

  const canComplete =
    callbackInput.trim() !== '' && !isCompleting && !isStarting

  async function handleStart() {
    setIsStarting(true)
    try {
      const response = await startCodexOAuth(props.channelId)
      const nextAuthorizeUrl = response.data?.authorize_url?.trim() ?? ''
      if (!response.success || !nextAuthorizeUrl) {
        throw new Error(
          response.message || 'Failed to start Codex authorization'
        )
      }
      setAuthorizeUrl(nextAuthorizeUrl)
      toast.success(t('Authorization link ready'))
    } catch (error) {
      const message =
        error instanceof Error
          ? error.message
          : 'Failed to start Codex authorization'
      toast.error(t(message))
    } finally {
      setIsStarting(false)
    }
  }

  async function handleComplete() {
    const input = callbackInput.trim()
    if (!input) return

    setIsCompleting(true)
    try {
      const response = await completeCodexOAuth(input)
      const credential = response.data?.key?.trim() ?? ''
      if (!response.success || !credential) {
        throw new Error(response.message || 'Codex authorization failed')
      }
      props.onCredentialGenerated(tryPrettyJson(credential))
      toast.success(t('Credential generated'))
      props.onOpenChange(false)
    } catch (error) {
      const message =
        error instanceof Error ? error.message : 'Codex authorization failed'
      toast.error(t(message))
    } finally {
      setIsCompleting(false)
    }
  }

  return (
    <Dialog
      open={props.open}
      onOpenChange={props.onOpenChange}
      title={t('Codex Authorization')}
      description={t(
        'Generate a Codex OAuth credential and paste it into the channel key field.'
      )}
      contentClassName='sm:max-w-2xl'
      bodyClassName='space-y-4'
      footer={
        <>
          <Button
            type='button'
            variant='outline'
            onClick={() => props.onOpenChange(false)}
            disabled={isStarting || isCompleting}
          >
            {t('Cancel')}
          </Button>
          <Button
            type='button'
            onClick={handleComplete}
            disabled={!canComplete}
          >
            {isCompleting ? (
              <Spinner data-icon='inline-start' aria-hidden='true' />
            ) : null}
            {isCompleting ? t('Generating...') : t('Generate credential')}
          </Button>
        </>
      }
    >
      <Alert>
        <AlertDescription>
          {t(
            'Generate an authorization link, open it and sign in with ChatGPT. After the browser redirects to localhost, copy the full URL from the address bar and paste it below.'
          )}
        </AlertDescription>
      </Alert>

      <div className='flex flex-wrap gap-2'>
        <Button
          type='button'
          onClick={handleStart}
          disabled={isStarting || isCompleting}
        >
          {isStarting ? (
            <Spinner data-icon='inline-start' aria-hidden='true' />
          ) : (
            <HugeiconsIcon icon={LinkSquare01Icon} data-icon='inline-start' />
          )}
          {isStarting ? t('Starting...') : t('Start authorization')}
        </Button>

        {authorizeUrl ? (
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
              nativeButton={false}
            >
              <HugeiconsIcon icon={LinkSquare01Icon} data-icon='inline-start' />
              {t('Open authorization page')}
            </Button>
            <Button
              type='button'
              variant='outline'
              onClick={() => copyToClipboard(authorizeUrl)}
              aria-label={t('Copy authorization link')}
            >
              <HugeiconsIcon
                icon={copiedText === authorizeUrl ? Tick02Icon : Copy01Icon}
                data-icon='inline-start'
              />
              {t('Copy authorization link')}
            </Button>
          </>
        ) : null}
      </div>

      <div className='space-y-2'>
        <Label htmlFor='codex-authorization-callback'>
          {t('Authorization callback URL')}
        </Label>
        <Input
          id='codex-authorization-callback'
          value={callbackInput}
          onChange={(event) => setCallbackInput(event.target.value)}
          placeholder={t(
            'Paste the full localhost callback URL (including code and state)'
          )}
          autoComplete='off'
          spellCheck={false}
        />
        <p className='text-muted-foreground text-xs'>
          {t(
            'The generated credential will fill the key field. Save the channel to persist it.'
          )}
        </p>
      </div>
    </Dialog>
  )
}
