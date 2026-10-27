import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import { act, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'

const auth = vi.hoisted(() => ({
  getSession: vi.fn(),
  onAuthStateChange: vi.fn(),
  signOut: vi.fn(),
}))
vi.mock('./supabase.js', () => ({ supabase: { auth } }))

import App from './App.jsx'
import { clearSessionSecurity, createRoom } from './api.js'

const account = { access_token: 'account-a', user: { id: 'a' } }
let authStateChanged

function bootstrapResponse(token) {
  return new Response(null, { status: 204, headers: { 'X-CSRF-Token': token } })
}

beforeEach(() => {
  localStorage.clear()
  clearSessionSecurity()
  vi.stubEnv('VITE_BACKEND_PROXY_TARGET', '')
  Object.values(auth).forEach((mock) => mock.mockReset())
  auth.getSession.mockResolvedValue({ data: { session: account }, error: null })
  auth.onAuthStateChange.mockImplementation((listener) => {
    authStateChanged = listener
    return { data: { subscription: { unsubscribe: vi.fn() } } }
  })
  auth.signOut.mockResolvedValue({ error: null })
  vi.stubGlobal('fetch', vi.fn())
})

afterEach(() => {
  clearSessionSecurity()
  vi.unstubAllEnvs()
  vi.unstubAllGlobals()
})

it('retries a failed logout through App with the same valid token, then clears it', async () => {
  let deletes = 0
  fetch.mockImplementation(async (url, options) => {
    if (url === '/api/session' && options.method === 'POST') return bootstrapResponse('account-a-csrf')
    if (url === '/api/session' && options.method === 'DELETE') {
      deletes += 1
      return new Response(null, { status: deletes === 1 ? 500 : 204 })
    }
    if (url === '/api/wallet') return Response.json({ balance: '125' })
    throw new Error(`Unexpected request: ${url}`)
  })
  render(<App />)
  await userEvent.click(await screen.findByRole('button', { name: 'Sign out' }))
  expect(await screen.findByRole('alert')).toHaveTextContent('Request failed (500).')
  expect(auth.signOut).not.toHaveBeenCalled()
  await userEvent.click(screen.getByRole('button', { name: 'Sign out' }))
  expect(await screen.findByRole('heading', { name: 'Sign in to play' })).toBeInTheDocument()
  const requests = fetch.mock.calls.filter(([, options]) => options.method === 'DELETE')
  expect(requests).toHaveLength(2)
  for (const [url, options] of requests) {
    expect(url).toBe('/api/session')
    expect(options).toMatchObject({ credentials: 'include', headers: { 'X-CSRF-Token': 'account-a-csrf' } })
  }
  expect(auth.signOut).toHaveBeenCalledOnce()
  const calls = fetch.mock.calls.length
  await expect(createRoom()).rejects.toMatchObject({ status: 401 })
  expect(fetch).toHaveBeenCalledTimes(calls)
})

it.each(['pending', 'ready'])('rebootstraps after signout and same-account signin supersede a %s bootstrap', async (state) => {
  let resolveFirst
  let bootstraps = 0
  fetch.mockImplementation(async (url, options) => {
    if (url === '/api/session' && options.method === 'POST') {
      bootstraps += 1
      if (bootstraps === 1) {
        if (state === 'pending') return new Promise((resolve) => { resolveFirst = resolve })
        return bootstrapResponse('superseded-csrf')
      }
      return bootstrapResponse('latest-csrf')
    }
    if (url === '/api/wallet') return Response.json({ balance: '125' })
    if (url === '/api/rooms') return Response.json({ room_code: 'ABC234' }, { status: 201 })
    throw new Error(`Unexpected request: ${url}`)
  })
  render(<App />)
  await waitFor(() => expect(bootstraps).toBe(1))
  if (state === 'ready') await screen.findByRole('button', { name: 'Create room' })
  act(() => {
    authStateChanged('SIGNED_OUT', null)
    authStateChanged('SIGNED_IN', account)
  })
  expect(screen.queryByRole('button', { name: 'Create room' })).not.toBeInTheDocument()
  if (state === 'pending') await act(async () => resolveFirst(bootstrapResponse('superseded-csrf')))
  expect(await screen.findByRole('button', { name: 'Create room' })).toBeInTheDocument()
  await expect(createRoom()).resolves.toEqual({ roomCode: 'ABC234' })
  expect(bootstraps).toBe(2)
  expect(fetch).toHaveBeenLastCalledWith('/api/rooms', expect.objectContaining({
    method: 'POST', credentials: 'include',
    headers: { 'X-CSRF-Token': 'latest-csrf', 'Idempotency-Key': expect.any(String) },
  }))
})
