import { beforeEach, describe, expect, it, vi } from 'vitest'
import { act, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'

const auth = vi.hoisted(() => ({
  getSession: vi.fn(),
  onAuthStateChange: vi.fn(),
  signInWithPassword: vi.fn(),
  signUp: vi.fn(),
  signOut: vi.fn(),
}))

vi.mock('./supabase.js', () => ({ supabase: { auth } }))
vi.mock('./api.js')

import App from './App.jsx'

describe('account authentication boundary', () => {
  let authStateChanged

  beforeEach(() => {
    localStorage.clear()
    Object.values(auth).forEach((mock) => mock.mockReset())
    auth.getSession.mockResolvedValue({ data: { session: null }, error: null })
    auth.onAuthStateChange.mockImplementation((listener) => {
      authStateChanged = listener
      return { data: { subscription: { unsubscribe: vi.fn() } } }
    })
  })

  it('restores a Supabase session before enabling game actions', async () => {
    let resolveSession
    auth.getSession.mockReturnValue(new Promise((resolve) => { resolveSession = resolve }))
    render(<App />)

    expect(screen.queryByRole('button', { name: 'Create room' })).not.toBeInTheDocument()
    resolveSession({ data: { session: { access_token: 'current' } }, error: null })

    expect(await screen.findByRole('button', { name: 'Create room' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Sign out' })).toBeInTheDocument()
  })

  it('signs in with email and password', async () => {
    auth.signInWithPassword.mockResolvedValue({ data: { session: { access_token: 'current' } }, error: null })
    render(<App />)
    await screen.findByRole('heading', { name: 'Sign in to play' })

    await userEvent.type(screen.getByLabelText('Email address'), 'player@example.com')
    await userEvent.type(screen.getByLabelText('Password'), 'secret12')
    await userEvent.click(screen.getByRole('button', { name: 'Sign in' }))

    expect(auth.signInWithPassword).toHaveBeenCalledWith({ email: 'player@example.com', password: 'secret12' })
    expect(await screen.findByRole('button', { name: 'Create room' })).toBeInTheDocument()
  })

  it('reacts to Supabase auth state changes', async () => {
    render(<App />)
    await screen.findByRole('heading', { name: 'Sign in to play' })

    act(() => authStateChanged('SIGNED_IN', { access_token: 'current' }))

    expect(await screen.findByRole('button', { name: 'Create room' })).toBeInTheDocument()
  })

  it('does not let stale session restoration overwrite a newer auth event', async () => {
    let resolveSession
    auth.getSession.mockReturnValue(new Promise((resolve) => { resolveSession = resolve }))
    render(<App />)
    await waitFor(() => expect(auth.onAuthStateChange).toHaveBeenCalledOnce())

    act(() => authStateChanged('SIGNED_IN', { access_token: 'current' }))
    expect(await screen.findByRole('button', { name: 'Create room' })).toBeInTheDocument()

    await act(async () => resolveSession({ data: { session: null }, error: null }))
    expect(screen.getByRole('button', { name: 'Create room' })).toBeInTheDocument()
  })

  it('shows email confirmation pending when registration has no session', async () => {
    auth.signUp.mockResolvedValue({ data: { user: { id: 'user' }, session: null }, error: null })
    render(<App />)
    await screen.findByRole('heading', { name: 'Sign in to play' })
    await userEvent.click(screen.getByRole('button', { name: 'Need an account? Register' }))
    await userEvent.type(screen.getByLabelText('Email address'), 'player@example.com')
    await userEvent.type(screen.getByLabelText('Password'), 'secret12')
    await userEvent.click(screen.getByRole('button', { name: 'Register' }))

    expect(await screen.findByRole('heading', { name: 'Check your inbox' })).toBeInTheDocument()
    expect(screen.getByText(/player@example.com/)).toBeInTheDocument()
  })

  it('signs out through Supabase', async () => {
    auth.getSession.mockResolvedValue({ data: { session: { access_token: 'current' } }, error: null })
    auth.signOut.mockResolvedValue({ error: null })
    render(<App />)
    await userEvent.click(await screen.findByRole('button', { name: 'Sign out' }))

    await waitFor(() => expect(auth.signOut).toHaveBeenCalledOnce())
    expect(await screen.findByRole('heading', { name: 'Sign in to play' })).toBeInTheDocument()
  })
})
