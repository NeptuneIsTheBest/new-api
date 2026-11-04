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
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'

import { api } from '@/lib/api'

import { CodexOAuthDialog } from '../dialogs/codex-oauth-dialog'

const authorization = {
  authorize_url: 'https://auth.openai.com/oauth/authorize?state=first',
  flow_token: 'first',
  expires_at: 2000000000,
}
const credential = {
  key: '{"access_token":"private-access","refresh_token":"private-refresh","account_id":"account-1"}',
  account_id: 'account-1',
}
let client: QueryClient

function Harness(props: { onKeyGenerated: (key: string) => void }) {
  const [open, setOpen] = useState(true)
  return (
    <QueryClientProvider client={client}>
      <button type='button' onClick={() => setOpen(true)}>
        Reopen
      </button>
      {open && (
        <CodexOAuthDialog
          open
          proxy='http://proxy.example:8080'
          onOpenChange={setOpen}
          onKeyGenerated={props.onKeyGenerated}
        />
      )}
    </QueryClientProvider>
  )
}

beforeEach(() => {
  client = new QueryClient()
  vi.spyOn(window, 'open').mockReturnValue(null)
})

afterEach(() => {
  client.clear()
  vi.restoreAllMocks()
})

test('blocked popups retain an accessible authorization link and shared copy action', async () => {
  const user = userEvent.setup()
  const clipboard = vi
    .spyOn(navigator.clipboard, 'writeText')
    .mockResolvedValue()
  vi.spyOn(api, 'post').mockResolvedValue({
    data: { success: true, data: authorization },
  })
  render(<Harness onKeyGenerated={vi.fn()} />)
  expect(
    screen.getByRole('button', { name: 'Generate credential' })
  ).toBeDisabled()
  await user.click(screen.getByRole('button', { name: 'Start authorization' }))
  expect(
    await screen.findByRole('link', { name: 'Open authorization page' })
  ).toHaveAttribute('href', authorization.authorize_url)
  await user.click(
    screen.getByRole('button', { name: 'Copy authorization link' })
  )
  expect(clipboard).toHaveBeenCalledWith(authorization.authorize_url)
  expect(screen.getByRole('textbox', { name: 'Callback URL' })).toBeEnabled()
})

test('generating a credential passes the proxy and uses one-time request options', async () => {
  const user = userEvent.setup()
  const onKeyGenerated = vi.fn()
  const post = vi
    .spyOn(api, 'post')
    .mockResolvedValueOnce({ data: { success: true, data: authorization } })
    .mockResolvedValueOnce({ data: { success: true, data: credential } })
  render(<Harness onKeyGenerated={onKeyGenerated} />)
  await user.click(screen.getByRole('button', { name: 'Start authorization' }))
  fireEvent.change(
    await screen.findByRole('textbox', { name: 'Callback URL' }),
    {
      target: {
        value: 'http://localhost:1455/auth/callback?code=private&state=first',
      },
    }
  )
  await user.click(screen.getByRole('button', { name: 'Generate credential' }))
  await waitFor(() => expect(onKeyGenerated).toHaveBeenCalledOnce())
  expect(JSON.parse(onKeyGenerated.mock.calls[0][0])).toEqual(
    JSON.parse(credential.key)
  )
  expect(post).toHaveBeenLastCalledWith(
    '/api/channel/codex/oauth/complete',
    expect.objectContaining({
      flow_token: 'first',
      proxy: 'http://proxy.example:8080',
    }),
    expect.objectContaining({ singleUseAuthorization: true })
  )
  expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
})

test('closing during exchange ignores a late credential and reopening starts empty', async () => {
  const user = userEvent.setup()
  const onKeyGenerated = vi.fn()
  let finish!: (response: unknown) => void
  const pending = new Promise((resolve) => {
    finish = resolve
  })
  const post = vi
    .spyOn(api, 'post')
    .mockResolvedValueOnce({ data: { success: true, data: authorization } })
    .mockImplementationOnce(() => pending as ReturnType<typeof api.post>)
  render(<Harness onKeyGenerated={onKeyGenerated} />)
  await user.click(screen.getByRole('button', { name: 'Start authorization' }))
  fireEvent.change(screen.getByRole('textbox', { name: 'Callback URL' }), {
    target: { value: 'callback' },
  })
  const generate = screen.getByRole('button', { name: 'Generate credential' })
  fireEvent.click(generate)
  fireEvent.click(generate)
  await waitFor(() => expect(post).toHaveBeenCalledTimes(2))
  await user.click(screen.getByRole('button', { name: 'Cancel' }))
  const config = post.mock.calls[1][2]
  expect(config?.signal?.aborted).toBe(true)
  await act(async () => {
    finish({ data: { success: true, data: credential } })
    await pending
  })
  expect(onKeyGenerated).not.toHaveBeenCalled()
  await user.click(screen.getByRole('button', { name: 'Reopen' }))
  expect(screen.getByRole('textbox', { name: 'Callback URL' })).toHaveValue('')
  expect(
    screen.queryByRole('link', { name: 'Open authorization page' })
  ).not.toBeInTheDocument()
})

test('closing during start prevents a late authorization response from opening a window', async () => {
  const user = userEvent.setup()
  let finish!: (response: unknown) => void
  const pending = new Promise((resolve) => {
    finish = resolve
  })
  const post = vi
    .spyOn(api, 'post')
    .mockImplementationOnce(() => pending as ReturnType<typeof api.post>)
  render(<Harness onKeyGenerated={vi.fn()} />)
  fireEvent.click(screen.getByRole('button', { name: 'Start authorization' }))
  await waitFor(() => expect(post).toHaveBeenCalledOnce())
  await user.click(screen.getByRole('button', { name: 'Cancel' }))
  await act(async () => {
    finish({ data: { success: true, data: authorization } })
    await pending
  })
  expect(window.open).not.toHaveBeenCalled()
})

test('failed exchange is not retried and requires a new authorization', async () => {
  const user = userEvent.setup()
  const onKeyGenerated = vi.fn()
  const post = vi
    .spyOn(api, 'post')
    .mockResolvedValueOnce({ data: { success: true, data: authorization } })
    .mockResolvedValueOnce({
      data: {
        success: false,
        code: 'CODEX_OAUTH_FAILED',
        message:
          'Codex authorization failed. Start a new authorization and try again.',
      },
    })
  render(<Harness onKeyGenerated={onKeyGenerated} />)
  await user.click(screen.getByRole('button', { name: 'Start authorization' }))
  fireEvent.change(screen.getByRole('textbox', { name: 'Callback URL' }), {
    target: { value: 'callback' },
  })
  await user.click(screen.getByRole('button', { name: 'Generate credential' }))
  await waitFor(() =>
    expect(
      screen.getByRole('button', { name: 'Start authorization' })
    ).toBeEnabled()
  )
  expect(post).toHaveBeenCalledTimes(2)
  expect(onKeyGenerated).not.toHaveBeenCalled()
  expect(
    screen.getByRole('button', { name: 'Generate credential' })
  ).toBeDisabled()
})

test('starting again discards the old callback and uses a fresh authorization', async () => {
  const user = userEvent.setup()
  vi.spyOn(api, 'post')
    .mockResolvedValueOnce({ data: { success: true, data: authorization } })
    .mockResolvedValueOnce({
      data: {
        success: true,
        data: {
          ...authorization,
          flow_token: 'second',
          authorize_url: 'https://auth.openai.com/oauth/authorize?state=second',
        },
      },
    })
  render(<Harness onKeyGenerated={vi.fn()} />)
  await user.click(screen.getByRole('button', { name: 'Start authorization' }))
  fireEvent.change(screen.getByRole('textbox', { name: 'Callback URL' }), {
    target: { value: 'old-callback' },
  })
  await user.click(screen.getByRole('button', { name: 'Start again' }))
  expect(screen.getByRole('textbox', { name: 'Callback URL' })).toHaveValue('')
  expect(
    screen.getByRole('link', { name: 'Open authorization page' })
  ).toHaveAttribute(
    'href',
    'https://auth.openai.com/oauth/authorize?state=second'
  )
})
