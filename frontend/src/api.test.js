import { beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, createRoom, getRoomState, leaveRoom, startNextRound, submitMove, subscribeToRoom } from './api.js'

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
        room_code: 'ABC234', ready: true, resolved: false, round: 3, closed: false,
        players: [{ role: 'host', wins: 2, submitted: true, wants_next_round: false }],
      }))

    await expect(createRoom()).resolves.toEqual({ roomCode: 'ABC234', playerToken: 'private-token', role: 'host' })
    await expect(getRoomState('ABC234')).resolves.toEqual({
      roomCode: 'ABC234', ready: true, resolved: false, round: 3, closed: false,
      players: [{ role: 'host', wins: 2, submitted: true, wantsNextRound: false }], result: null, moves: [],
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

  it('sends authenticated next-round consensus and leave commands', async () => {
    fetch.mockResolvedValue(response(null, { status: 204 }))

    await startNextRound('ABC234', 'secret', 2)
    expect(fetch).toHaveBeenNthCalledWith(1, '/api/rooms/ABC234/next-round', expect.objectContaining({
      method: 'POST',
      headers: { Authorization: 'Bearer secret', 'Content-Type': 'application/json' },
      body: '{"round":2}',
    }))

    await leaveRoom('ABC234', 'secret')
    expect(fetch).toHaveBeenNthCalledWith(2, '/api/rooms/ABC234/leave', expect.objectContaining({
      method: 'POST',
      headers: { Authorization: 'Bearer secret' },
    }))
  })

  it('subscribes to public room events, maps snapshots, and closes cleanly', () => {
    class FakeEventSource {
      constructor(url) {
        this.url = url
        this.close = vi.fn()
        FakeEventSource.instance = this
      }
    }
    vi.stubGlobal('EventSource', FakeEventSource)
    const onState = vi.fn()
    const onError = vi.fn()

    const close = subscribeToRoom('ABC234', { onState, onError })
    expect(FakeEventSource.instance.url).toBe('/api/rooms/ABC234/events')
    expect(FakeEventSource.instance.url).not.toContain('token')

    FakeEventSource.instance.onmessage({ data: JSON.stringify({
      room_code: 'ABC234', ready: false, resolved: false, round: 1, closed: false,
      players: [{ role: 'host', wins: 0, submitted: false, wants_next_round: false }],
    }) })
    expect(onState).toHaveBeenCalledWith(waitingFrontendState())

    FakeEventSource.instance.onmessage({ data: 'not JSON' })
    expect(onError).toHaveBeenCalledWith(expect.objectContaining({
      message: 'The game server returned an invalid room update.',
    }))

    close()
    expect(FakeEventSource.instance.close).toHaveBeenCalledOnce()
  })
})

function waitingFrontendState() {
  return {
    roomCode: 'ABC234', ready: false, resolved: false, round: 1, closed: false,
    players: [{ role: 'host', wins: 0, submitted: false, wantsNextRound: false }], result: null, moves: [],
  }
}
