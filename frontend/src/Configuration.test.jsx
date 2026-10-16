import { expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'

vi.mock('./supabase.js', () => ({ supabase: null }))

import App from './App.jsx'

it('shows the branded configuration-unavailable screen without enabling play', () => {
  render(<App />)
  expect(screen.getByRole('img', { name: 'Rock Paper Money' })).toBeInTheDocument()
  expect(screen.getByRole('heading', { name: 'Game unavailable' })).toBeInTheDocument()
  expect(screen.getByRole('alert')).toHaveTextContent('Supabase authentication is not configured.')
  expect(screen.queryByRole('button', { name: 'Create room' })).not.toBeInTheDocument()
})
