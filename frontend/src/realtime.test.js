import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { connectToRoom } from './realtime.js'

class FakeWebSocket {
  static OPEN = 1
  static instances = []

  constructor(url) {
    this.url = url
    this.readyState = FakeWebSocket.OPEN
    this.close = vi.fn(() => {
      this.readyState = 3
    })
    FakeWebSocket.instances.push(this)
  }

  open() {
    this.onopen?.()
  }

  message(value) {
    this.onmessage?.({ data: JSON.stringify(value) })
  }

  disconnect() {
    this.readyState = 3
    this.onclose?.({ code: 1006 })
  }
}

function frame(revision, role = 'host') {
  return {
    type: 'snapshot', revision,
    player: { account_id: 'account-1', role },
    presence: { host: true, guest: false },
    room: {
      room_code: 'ABC234', ready: false, resolved: false, round: 1, closed: false, forfeit: false,
      players: [{ role: 'host', wins: 0, submitted: false, wants_next_round: false }],
    },
  }
}

describe('room WebSocket client', () => {
  afterEach(() => {
    vi.unstubAllEnvs()
    vi.useRealTimers()
  })

  beforeEach(() => {
    vi.stubEnv('VITE_BACKEND_PROXY_TARGET', '')
    vi.useFakeTimers()
    FakeWebSocket.instances = []
    vi.stubGlobal('WebSocket', FakeWebSocket)
  })

  it.each([
    ['http://backend.example:8080', 'ws://backend.example:8080'],
    ['https://backend.example/', 'wss://backend.example'],
  ])('uses configured backend %s for initial and reconnected sockets', (target, expected) => {
    vi.stubEnv('VITE_BACKEND_PROXY_TARGET', target)
    const close = connectToRoom('A/B ?', {}, { reconnectDelay: () => 5 })
    expect(FakeWebSocket.instances[0].url).toBe(`${expected}/api/rooms/A%2FB%20%3F/socket`)
    FakeWebSocket.instances[0].disconnect()
    vi.advanceTimersByTime(5)
    expect(FakeWebSocket.instances[1].url).toBe(`${expected}/api/rooms/A%2FB%20%3F/socket`)
    close()
  })

  it.each([undefined, ''])('uses the current origin when the backend target is %s', (target) => {
    vi.stubEnv('VITE_BACKEND_PROXY_TARGET', target)
    const close = connectToRoom('A/B ?')
    expect(FakeWebSocket.instances[0].url).toBe('ws://localhost:3000/api/rooms/A%2FB%20%3F/socket')
    close()
  })

  it('accepts monotonic authoritative snapshots, ignores duplicates, and reconnects across gaps', () => {
    const onState = vi.fn()
    const close = connectToRoom('ABC234', { onState }, { reconnectDelay: () => 10 })
    const first = FakeWebSocket.instances[0]
    expect(first.url).toBe('ws://localhost:3000/api/rooms/ABC234/socket')

    first.open()
    first.message(frame(4))
    first.message(frame(4))
    expect(onState).toHaveBeenCalledTimes(1)
    expect(onState).toHaveBeenLastCalledWith(expect.objectContaining({ revision: 4, playerRole: 'host' }))

    first.message(frame(6))
    expect(first.close).toHaveBeenCalled()
    vi.advanceTimersByTime(10)
    const second = FakeWebSocket.instances[1]
    second.open()
    second.message(frame(7, 'guest'))
    expect(onState).toHaveBeenLastCalledWith(expect.objectContaining({ revision: 7, playerRole: 'guest' }))

    close()
    second.disconnect()
    vi.runOnlyPendingTimers()
    expect(FakeWebSocket.instances).toHaveLength(2)
  })

  it('reconnects after an unplanned disconnect without relying on event history', () => {
    connectToRoom('ABC234', {}, { reconnectDelay: () => 5 })
    FakeWebSocket.instances[0].disconnect()
    vi.advanceTimersByTime(5)
    expect(FakeWebSocket.instances).toHaveLength(2)
  })
})
