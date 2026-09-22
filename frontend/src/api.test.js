import { beforeEach, describe, expect, it, vi } from 'vitest'

import {
  ApiError,
  bootstrapSession,
  createRoom,
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
    headers: { 'Content-Type': 'application/json', 'X-Request-ID': requestId },
  })
}

describe('cookie session API client', () => {
  beforeEach(() => {
    vi.stubGlobal('fetch', vi.fn())
    vi.spyOn(crypto, 'randomUUID').mockReturnValue('generated-key')
    document.cookie = 'rpm_csrf=csrf-token; Path=/'
  })

  it('bootstraps cookies once, then logs out with session-bound CSRF', async () => {
    fetch.mockResolvedValue(response(null, { status: 204 }))
    await bootstrapSession('external-access-token')
    await logoutSession()

    expect(fetch).toHaveBeenNthCalledWith(1, '/api/session', expect.objectContaining({
      method: 'POST', credentials: 'same-origin', headers: { Authorization: 'Bearer external-access-token' },
    }))
    expect(fetch).toHaveBeenNthCalledWith(2, '/api/session', expect.objectContaining({
      method: 'DELETE', credentials: 'same-origin', headers: { 'X-CSRF-Token': 'csrf-token' },
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
      expect(options.credentials).toBe('same-origin')
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

  it('reports unreachable services as API errors', async () => {
    fetch.mockRejectedValue(new TypeError('offline'))
    await expect(getWallet()).rejects.toEqual(expect.any(ApiError))
  })
})
