import { beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, createRoom, getRoomState, submitMove } from './api.js'

function response(body, { status = 200 } = {}) {
  return new Response(body === null ? null : JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

describe('API client', () => {
  beforeEach(() => vi.stubGlobal('fetch', vi.fn()))

  it('maps credential and state DTOs into frontend properties', async () => {
    fetch
      .mockResolvedValueOnce(response({ room_code: 'ABC234', player_token: 'private-token' }, { status: 201 }))
      .mockResolvedValueOnce(response({
        room_code: 'ABC234', ready: true, resolved: false,
        players: [{ role: 'host', wins: 2, submitted: true }],
      }))

    await expect(createRoom()).resolves.toEqual({ roomCode: 'ABC234', playerToken: 'private-token', role: 'host' })
    await expect(getRoomState('ABC234')).resolves.toEqual({
      roomCode: 'ABC234', ready: true, resolved: false,
      players: [{ role: 'host', wins: 2, submitted: true }], result: null, moves: [],
    })
  })

  it('uses stable server errors and falls back safely for malformed errors', async () => {
    fetch
      .mockResolvedValueOnce(response({ error: 'room is full' }, { status: 409 }))
      .mockResolvedValueOnce(new Response('bad gateway', { status: 502 }))

    await expect(createRoom()).rejects.toMatchObject({ message: 'room is full', status: 409 })
    await expect(createRoom()).rejects.toMatchObject({ message: 'Request failed (502).', status: 502 })
  })

  it('reports network failures and sends authenticated move JSON', async () => {
    fetch.mockRejectedValueOnce(new TypeError('offline')).mockResolvedValueOnce(response(null, { status: 204 }))

    await expect(getRoomState('ABC234')).rejects.toEqual(expect.any(ApiError))
    await submitMove('ABC234', 'secret', 'paper')

    expect(fetch).toHaveBeenLastCalledWith('/api/rooms/ABC234/moves', expect.objectContaining({
      method: 'POST',
      headers: { Authorization: 'Bearer secret', 'Content-Type': 'application/json' },
      body: '{"move":"paper"}',
    }))
  })
})
