import { beforeEach, describe, expect, it } from 'vitest'
import { clearSession, loadSession, saveSession, STORAGE_KEY } from './storage.js'

const valid = { roomCode: 'ABC234', playerToken: 'private-token', role: 'host' }

describe('room session storage', () => {
  beforeEach(() => localStorage.clear())

  it('persists, restores, and clears valid credentials', () => {
    saveSession(valid)
    expect(loadSession()).toEqual(valid)
    clearSession()
    expect(loadSession()).toBeNull()
  })

  it.each([
    '{bad json',
    JSON.stringify({ ...valid, roomCode: 'short' }),
    JSON.stringify({ ...valid, playerToken: '' }),
    JSON.stringify({ ...valid, role: 'observer' }),
  ])('rejects invalid stored data without throwing', (stored) => {
    localStorage.setItem(STORAGE_KEY, stored)
    expect(loadSession()).toBeNull()
  })

  it('refuses to persist invalid credentials', () => {
    expect(() => saveSession({ ...valid, playerToken: 'token with spaces' })).toThrow(/invalid room session/i)
  })
})
