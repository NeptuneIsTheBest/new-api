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
import { useEffect, useMemo, useState } from 'react'
import { Check, Copy, ExternalLink, Loader2 } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'
import { tryPrettyJson } from '@/lib/utils'
import { useCopyToClipboard } from '@/hooks/use-copy-to-clipboard'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Dialog } from '@/components/dialog'
import { completeCodexOAuth, startCodexOAuth } from '../../api'

type CodexOAuthDialogProps = {
  open: boolean
  onOpenChange: (open: boolean) => void
  onKeyGenerated: (key: string) => void
}

export function CodexOAuthDialog(props: CodexOAuthDialogProps) {
  const { t } = useTranslation()
  const { copiedText, copyToClipboard } = useCopyToClipboard({ notify: false })

  const [authorizeUrl, setAuthorizeUrl] = useState('')
  const [callbackUrl, setCallbackUrl] = useState('')
  const [isStarting, setIsStarting] = useState(false)
  const [isCompleting, setIsCompleting] = useState(false)

  useEffect(() => {
    if (!props.open) {
      setAuthorizeUrl('')
      setCallbackUrl('')
      setIsStarting(false)
      setIsCompleting(false)
    }
  }, [props.open])

  const canCopyAuthorizeUrl = Boolean(authorizeUrl && !isStarting)
  const canComplete = useMemo(
    () => Boolean(callbackUrl.trim()) && !isCompleting,
    [callbackUrl, isCompleting]
  )

  const handleStart = async () => {
    setIsStarting(true)
    try {
      const res = await startCodexOAuth()
      if (!res.success) {
        throw new Error(res.message || t('OAuth start failed'))
      }

      const url = res.data?.authorize_url || ''
      if (!url) {
        throw new Error(t('Missing authorize_url in response'))
      }

      setAuthorizeUrl(url)
      try {
        window.open(url, '_blank', 'noopener,noreferrer')
        toast.success(t('Opened authorization page'))
      } catch (error) {
        console.warn('Failed to open authorization page:', error)
        toast.warning(t('Please manually copy and open the authorization link'))
      }
    } catch (error) {
      toast.error(
        error instanceof Error ? error.message : t('OAuth start failed')
      )
    } finally {
      setIsStarting(false)
    }
  }

  const handleComplete = async () => {
    if (!callbackUrl.trim()) return

    setIsCompleting(true)
    try {
      const res = await completeCodexOAuth(callbackUrl.trim())
      if (!res.success) {
        throw new Error(res.message || t('OAuth failed'))
      }

      const rawKey = res.data?.key || ''
      if (!rawKey) {
        throw new Error(t('Missing key in response'))
      }

      props.onKeyGenerated(tryPrettyJson(rawKey))
      toast.success(t('Credential generated'))
      props.onOpenChange(false)
    } catch (error) {
      toast.error(error instanceof Error ? error.message : t('OAuth failed'))
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
      contentHeight='auto'
      bodyClassName='flex flex-col gap-4'
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
          <Button onClick={handleComplete} disabled={!canComplete}>
            {isCompleting && <Loader2 data-icon='inline-start' />}
            {isCompleting ? t('Generating...') : t('Generate credential')}
          </Button>
        </>
      }
    >
      <Alert>
        <AlertDescription>
          {t(
            '1) Click "Open authorization page" and complete login. 2) Your browser may redirect to localhost (it is OK if the page does not load). 3) Copy the full URL from the address bar and paste it below. 4) Click "Generate credential".'
          )}
        </AlertDescription>
      </Alert>

      <div className='flex flex-wrap gap-2'>
        <Button onClick={handleStart} disabled={isStarting}>
          {isStarting ? (
            <Loader2 data-icon='inline-start' />
          ) : (
            <ExternalLink data-icon='inline-start' />
          )}
          {t('Open authorization page')}
        </Button>

        <Button
          type='button'
          variant='outline'
          disabled={!canCopyAuthorizeUrl}
          onClick={async () => {
            if (!authorizeUrl) return
            await copyToClipboard(authorizeUrl)
          }}
          aria-label={t('Copy authorization link')}
          title={t('Copy authorization link')}
        >
          {copiedText === authorizeUrl ? (
            <Check data-icon='inline-start' />
          ) : (
            <Copy data-icon='inline-start' />
          )}
          {t('Copy authorization link')}
        </Button>
      </div>

      <div className='flex flex-col gap-2'>
        <div className='text-sm font-medium'>{t('Callback URL')}</div>
        <Input
          value={callbackUrl}
          onChange={(event) => setCallbackUrl(event.target.value)}
          placeholder={t('Paste the full callback URL (includes code & state)')}
          autoComplete='off'
          spellCheck={false}
        />
        <div className='text-muted-foreground text-xs'>
          {t(
            'Tip: The generated key is a JSON credential including access_token / refresh_token / account_id.'
          )}
        </div>
      </div>
    </Dialog>
  )
}
