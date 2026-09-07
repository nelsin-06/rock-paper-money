import { beforeEach, describe, expect, it } from 'vitest'
import { APP_VERSION, clearSession, loadSession, saveSession, STORAGE_KEY } from './storage.js'

const valid = { roomCode: 'ABC234', playerToken: 'private-token', role: 'host' }

describe('room session storage', () => {
  beforeEach(() => localStorage.clear())

  it('persists, restores, and clears valid credentials', () => {
    saveSession(valid)
    expect(loadSession()).toEqual(valid)
    expect(JSON.parse(localStorage.getItem(STORAGE_KEY))).toEqual({ ...valid, version: APP_VERSION })
    clearSession()
    expect(loadSession()).toBeNull()
  })

  it('preserves a session across patch releases and migrates its stored version', () => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify({ ...valid, version: '1.0.0' }))

    expect(loadSession(localStorage, '1.0.1')).toEqual(valid)
    expect(JSON.parse(localStorage.getItem(STORAGE_KEY)).version).toBe('1.0.1')
  })

  it.each(['1.1.0', '2.0.0'])('clears a session after an incompatible %s release', (currentVersion) => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify({ ...valid, version: '1.0.0' }))

    expect(loadSession(localStorage, currentVersion)).toBeNull()
    expect(localStorage.getItem(STORAGE_KEY)).toBeNull()
  })

  it.each([
    { label: 'legacy', version: undefined },
    { label: 'malformed', version: 'one.0.0' },
    { label: 'future patch', version: '1.0.2' },
    { label: 'future minor', version: '1.1.0' },
    { label: 'future major', version: '2.0.0' },
  ])('clears $label stored sessions', ({ version }) => {
    const stored = { ...valid }
    if (version !== undefined) stored.version = version
    localStorage.setItem(STORAGE_KEY, JSON.stringify(stored))

    expect(loadSession(localStorage, '1.0.1')).toBeNull()
    expect(localStorage.getItem(STORAGE_KEY)).toBeNull()
  })

  it.each([
    '{bad json',
    JSON.stringify({ ...valid, roomCode: 'short' }),
    JSON.stringify({ ...valid, playerToken: '' }),
    JSON.stringify({ ...valid, role: 'observer' }),
  ])('rejects invalid stored data without throwing', (stored) => {
    localStorage.setItem(STORAGE_KEY, stored)
    expect(loadSession()).toBeNull()
    expect(localStorage.getItem(STORAGE_KEY)).toBeNull()
  })

  it('refuses to persist invalid credentials', () => {
    expect(() => saveSession({ ...valid, playerToken: 'token with spaces' })).toThrow(/invalid room session/i)
  })
})
