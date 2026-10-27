import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import {
  ApiError,
  bootstrapSession,
  createRoom,
  clearSessionSecurity,
  getRoundAnalytics,
  getWallet,
  joinRoom,
  leaveRoom,
  logoutSession,
  rechargeWallet,
  startNextRound,
  submitMove,
} from './api.js'

function response(body, { status = 200, requestId = 'request-123' } = {}) {
  return new Response(body === null ? null : JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json', 'X-Request-ID': requestId, 'X-CSRF-Token': 'csrf-token' },
  })
}

describe('cookie session API client', () => {
  afterEach(() => vi.unstubAllEnvs())

  beforeEach(async () => {
    vi.stubEnv('VITE_BACKEND_PROXY_TARGET', '')
    vi.stubGlobal('fetch', vi.fn())
    vi.spyOn(crypto, 'randomUUID').mockReturnValue('generated-key')
    fetch.mockResolvedValueOnce(response(null, { status: 204 }))
    await bootstrapSession('initial-account')
    fetch.mockClear()
  })

  it.each(['http://backend.example:8080', 'https://backend.example/'])('routes HTTP requests to %s without changing session options or path encoding', async (target) => {
    vi.stubEnv('VITE_BACKEND_PROXY_TARGET', target)
    fetch.mockResolvedValue(response({ room_code: 'A/B ?' }, { status: 201 }))

    await expect(joinRoom(' a/b ? ')).resolves.toEqual({ roomCode: 'A/B ?' })

    expect(fetch).toHaveBeenCalledExactlyOnceWith(`${target.replace(/\/$/, '')}/api/rooms/A%2FB%20%3F/join`, expect.objectContaining({
      method: 'POST', credentials: 'include',
      headers: { 'X-CSRF-Token': 'csrf-token', 'Idempotency-Key': 'generated-key' },
    }))
  })

  it.each([undefined, ''])('keeps relative HTTP destinations when the backend target is %s', async (target) => {
    vi.stubEnv('VITE_BACKEND_PROXY_TARGET', target)
    fetch.mockResolvedValue(response({ balance: '50' }))
    await expect(getWallet()).resolves.toEqual({ balance: '50' })
    expect(fetch).toHaveBeenCalledExactlyOnceWith('/api/wallet', { credentials: 'include', signal: undefined })
  })

  it('uses bootstrap CSRF without frontend cookies and clears it on logout', async () => {
    vi.stubEnv('VITE_BACKEND_PROXY_TARGET', 'https://api.example.com')
    document.cookie = 'rpm_csrf=unrelated-frontend-cookie; Path=/'
    fetch.mockResolvedValueOnce(new Response(null, { status: 204, headers: { 'X-CSRF-Token': 'backend-token' } }))
    await bootstrapSession('external-access-token')
    fetch.mockResolvedValue(response(null, { status: 204 }))
    await logoutSession()
    expect(fetch.mock.calls[1][1]).toMatchObject({ credentials: 'include', headers: { 'X-CSRF-Token': 'backend-token' } })
    await expect(createRoom()).rejects.toMatchObject({ status: 401 })
    expect(fetch).toHaveBeenCalledTimes(2)
  })

  it('bootstraps cookies once, then logs out with session-bound CSRF', async () => {
    fetch.mockResolvedValue(response(null, { status: 204 }))
    await bootstrapSession('external-access-token')
    await logoutSession()

    expect(fetch).toHaveBeenNthCalledWith(1, '/api/session', expect.objectContaining({
      method: 'POST', credentials: 'include', headers: { Authorization: 'Bearer external-access-token' },
    }))
    expect(fetch).toHaveBeenNthCalledWith(2, '/api/session', expect.objectContaining({
      method: 'DELETE', credentials: 'include', headers: { 'X-CSRF-Token': 'csrf-token' },
    }))
  })

  it('uses cookie, CSRF, and idempotency headers without bearer or room tokens', async () => {
    fetch
      .mockResolvedValueOnce(response({ room_code: 'ABC234' }, { status: 201 }))
      .mockResolvedValueOnce(response({ room_code: 'ABC234' }, { status: 201 }))
      .mockResolvedValue(response(null, { status: 204 }))

    await expect(createRoom()).resolves.toEqual({ roomCode: 'ABC234' })
    await expect(joinRoom('abc234')).resolves.toEqual({ roomCode: 'ABC234' })
    await submitMove('ABC234', 'paper')
    await startNextRound('ABC234', 2)
    await leaveRoom('ABC234')

    for (const [, options] of fetch.mock.calls) {
      expect(options.credentials).toBe('include')
      expect(options.headers).toMatchObject({ 'X-CSRF-Token': 'csrf-token', 'Idempotency-Key': 'generated-key' })
      expect(options.headers).not.toHaveProperty('Authorization')
      expect(options.headers).not.toHaveProperty('X-Room-Token')
    }
    expect(fetch).toHaveBeenNthCalledWith(3, '/api/rooms/ABC234/moves', expect.objectContaining({ body: '{"move":"paper"}' }))
  })

  it('retries an ambiguous network failure with the same idempotency key', async () => {
    fetch.mockRejectedValueOnce(new TypeError('offline')).mockResolvedValueOnce(response(null, { status: 204 }))
    await submitMove('ABC234', 'rock')
    expect(fetch).toHaveBeenCalledTimes(2)
    expect(fetch.mock.calls[0][1].headers['Idempotency-Key']).toBe(fetch.mock.calls[1][1].headers['Idempotency-Key'])
  })

  it('requires browser CSRF context before issuing a mutation', async () => {
    document.cookie = 'rpm_csrf=; Max-Age=0; Path=/'
    clearSessionSecurity()
    await expect(createRoom()).rejects.toMatchObject({ status: 401 })
    expect(fetch).not.toHaveBeenCalled()
  })

  it('uses stable server errors and safe malformed-response fallbacks', async () => {
    fetch
      .mockResolvedValueOnce(response({
        status: 409,
        code: 'room_full',
        message: 'Room is full.',
        meta: { time: '2026-09-16T10:00:00Z', requestId: 'request-123' },
      }, { status: 409 }))
      .mockResolvedValueOnce(new Response('bad gateway', { status: 502 }))

    await expect(createRoom()).rejects.toMatchObject({ message: 'Room is full.', code: 'room_full', status: 409 })
    await expect(createRoom()).rejects.toMatchObject({ message: 'Request failed (502).', status: 502 })
  })

  it('maps exact coin values and preserves caller-owned recharge keys', async () => {
    fetch
      .mockResolvedValueOnce(response({ balance: '9007199254740993' }))
      .mockResolvedValueOnce(response({ balance: '9007199254741043' }))
      .mockResolvedValueOnce(response({
        total_house_earnings: '25',
        rounds: [{ room_code: 'ABC234', round: 1, result: 'player_one_wins', winner_role: 'host', forfeit: false, house_earnings: '25', resolved_at: '2026-09-16T12:00:00Z' }],
      }))

    await expect(getWallet()).resolves.toEqual({ balance: '9007199254740993' })
    await expect(rechargeWallet('50', 'retry-key')).resolves.toEqual({ balance: '9007199254741043' })
    await expect(getRoundAnalytics()).resolves.toMatchObject({ totalHouseEarnings: '25' })
    expect(fetch.mock.calls[1][1].headers['Idempotency-Key']).toBe('retry-key')
  })

  it.each([401, 500, 204])('clears the old token when bootstrap fails or omits its header (%s)', async (status) => {
    fetch.mockResolvedValueOnce(new Response(null, { status }))
    await expect(bootstrapSession('another-account')).rejects.toBeInstanceOf(ApiError)
    await expect(createRoom()).rejects.toMatchObject({ status: 401 })
    expect(fetch).toHaveBeenCalledTimes(1)
  })

  it('does not restore a stale bootstrap token after an account change or auth reset', async () => {
    let resolveOld
    fetch.mockReturnValueOnce(new Promise((resolve) => { resolveOld = resolve }))
    const old = bootstrapSession('old-account')
    fetch.mockResolvedValueOnce(new Response(null, { status: 204, headers: { 'X-CSRF-Token': 'new-token' } }))
    await bootstrapSession('new-account')
    resolveOld(new Response(null, { status: 204, headers: { 'X-CSRF-Token': 'old-token' } }))
    await old
    fetch.mockResolvedValueOnce(response({ room_code: 'ABC234' }))
    await createRoom()
    expect(fetch.mock.calls[2][1].headers['X-CSRF-Token']).toBe('new-token')
    clearSessionSecurity()
    await expect(createRoom()).rejects.toMatchObject({ status: 401 })
    expect(fetch).toHaveBeenCalledTimes(3)
  })

  it('retries failed logout with the same CSRF and clears it only after success', async () => {
    fetch.mockResolvedValueOnce(response(null, { status: 500 }))
    await expect(logoutSession()).rejects.toMatchObject({ status: 500 })
    fetch.mockResolvedValueOnce(response(null, { status: 204 }))
    await expect(logoutSession()).resolves.toBeUndefined()
    expect(fetch).toHaveBeenCalledTimes(2)
    for (const [url, options] of fetch.mock.calls) {
      expect(url).toBe('/api/session')
      expect(options).toEqual({ method: 'DELETE', credentials: 'include', headers: { 'X-CSRF-Token': 'csrf-token' }, signal: undefined })
    }
    await expect(createRoom()).rejects.toMatchObject({ status: 401 })
    expect(fetch).toHaveBeenCalledTimes(2)
  })

  it.each([
    ['reset', 500], ['reset', 204],
    ['bootstrap', 500], ['bootstrap', 204],
    ['logout', 500], ['logout', 204],
  ])('does not restore or clear newer security state after concurrent %s (old logout %s)', async (intervening, status) => {
    let resolveLogout
    fetch.mockReturnValueOnce(new Promise((resolve) => { resolveLogout = resolve }))
    const pending = logoutSession()
    if (intervening === 'reset') {
      clearSessionSecurity()
    } else if (intervening === 'bootstrap') {
      fetch.mockResolvedValueOnce(new Response(null, { status: 204, headers: { 'X-CSRF-Token': 'new-account-token' } }))
      await bootstrapSession('new-account')
    } else {
      fetch.mockResolvedValueOnce(response(null, { status: 204 }))
      await logoutSession()
    }
    resolveLogout(response(null, { status }))
    if (status === 500) await expect(pending).rejects.toMatchObject({ status: 500 })
    else await expect(pending).resolves.toBeUndefined()

    const calls = fetch.mock.calls.length
    if (intervening === 'bootstrap') {
      fetch.mockResolvedValueOnce(response(null, { status: 204 }))
      await logoutSession()
      expect(fetch).toHaveBeenLastCalledWith('/api/session', {
        method: 'DELETE', credentials: 'include', headers: { 'X-CSRF-Token': 'new-account-token' }, signal: undefined,
      })
      expect(fetch).toHaveBeenCalledTimes(calls + 1)
    } else {
      await expect(logoutSession()).rejects.toMatchObject({ status: 401 })
      expect(fetch).toHaveBeenCalledTimes(calls)
    }
  })

  it('reports unreachable services as API errors', async () => {
    fetch.mockRejectedValue(new TypeError('offline'))
    await expect(getWallet()).rejects.toEqual(expect.any(ApiError))
  })
})
