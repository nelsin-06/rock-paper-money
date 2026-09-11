import { getAccessToken } from './supabase.js'

export class ApiError extends Error {
  constructor(message, status = 0) {
    super(message)
    this.name = 'ApiError'
    this.status = status
  }
}

export const PRESENCE_HEARTBEAT_MS = 3000

async function request(path, options = {}) {
  let response
  try {
    response = await fetch(path, options)
  } catch {
    throw new ApiError('Cannot reach the game server. Check your connection and try again.')
  }

  if (!response.ok) {
    let message = `Request failed (${response.status}).`
    try {
      const body = await response.json()
      if (typeof body.error === 'string' && body.error) message = body.error
    } catch {
      // Keep the status-based fallback when an upstream response is not JSON.
    }
    throw new ApiError(message, response.status)
  }

  if (response.status === 204) return null

  try {
    return await response.json()
  } catch {
    throw new ApiError('The game server returned an invalid response.', response.status)
  }
}

function mapCredentials(dto, role) {
  if (typeof dto?.room_code !== 'string' || typeof dto?.player_token !== 'string') {
    throw new ApiError('The game server returned invalid room credentials.')
  }
  return { roomCode: dto.room_code, playerToken: dto.player_token, role }
}

function mapRoomState(dto) {
  if (!Array.isArray(dto?.players)) throw new ApiError('The game server returned an invalid room state.')

  return {
    roomCode: dto.room_code,
    ready: dto.ready,
    resolved: dto.resolved,
    round: dto.round,
    closed: dto.closed,
    forfeit: dto.forfeit ?? false,
    players: dto.players.map((player) => ({
      role: player.role,
      wins: player.wins,
      submitted: player.submitted,
      wantsNextRound: player.wants_next_round,
    })),
    result: dto.result ?? null,
    moves: (dto.moves ?? []).map((move) => ({ role: move.role, move: move.move })),
  }
}

async function protectedHeaders(roomToken) {
  const headers = { Authorization: `Bearer ${await getAccessToken()}` }
  if (roomToken) headers['X-Room-Token'] = roomToken
  return headers
}

export async function createRoom({ signal } = {}) {
  return mapCredentials(await request('/api/rooms', { method: 'POST', headers: await protectedHeaders(), signal }), 'host')
}

export async function joinRoom(code, { signal } = {}) {
  const normalized = code.trim().toUpperCase()
  return mapCredentials(
    await request(`/api/rooms/${encodeURIComponent(normalized)}/join`, { method: 'POST', headers: await protectedHeaders(), signal }),
    'guest',
  )
}

export async function getRoomState(code, { signal } = {}) {
  const dto = await request(`/api/rooms/${encodeURIComponent(code)}/state`, { signal })
  return mapRoomState(dto)
}

export async function validateSession(code, token, role, { signal } = {}) {
  await request(`/api/rooms/${encodeURIComponent(code)}/validate-session`, {
    method: 'POST',
    headers: { ...await protectedHeaders(token), 'Content-Type': 'application/json' },
    body: JSON.stringify({ role }),
    signal,
  })
}

export function subscribeToRoom(code, { onState, onOpen, onError }) {
  const source = new EventSource(`/api/rooms/${encodeURIComponent(code)}/events`)
  source.onopen = () => onOpen?.()
  source.onmessage = (event) => {
    try {
      onState(mapRoomState(JSON.parse(event.data)))
    } catch {
      onError?.(new ApiError('The game server returned an invalid room update.'))
    }
  }
  source.onerror = () => {
    onError?.(new ApiError('Live room updates disconnected. Reconnecting…'))
  }
  return () => source.close()
}

export async function submitMove(code, token, move, { signal } = {}) {
  await request(`/api/rooms/${encodeURIComponent(code)}/moves`, {
    method: 'POST',
    headers: { ...await protectedHeaders(token), 'Content-Type': 'application/json' },
    body: JSON.stringify({ move }),
    signal,
  })
}

export async function startNextRound(code, token, round, { signal } = {}) {
  await request(`/api/rooms/${encodeURIComponent(code)}/next-round`, {
    method: 'POST',
    headers: { ...await protectedHeaders(token), 'Content-Type': 'application/json' },
    body: JSON.stringify({ round }),
    signal,
  })
}

export async function leaveRoom(code, token, { signal } = {}) {
  await request(`/api/rooms/${encodeURIComponent(code)}/leave`, {
    method: 'POST',
    headers: await protectedHeaders(token),
    signal,
  })
}

export async function refreshPresence(code, token, { signal } = {}) {
  await request(`/api/rooms/${encodeURIComponent(code)}/presence`, {
    method: 'POST',
    headers: await protectedHeaders(token),
    signal,
  })
}
