import { getAccessToken } from './supabase.js'

export class ApiError extends Error {
  constructor(message, status = 0, { code = '', requestId = '', rawError } = {}) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
    this.requestId = requestId
    if (typeof rawError === 'string' && rawError) this.rawError = rawError
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
    const fallback = new ApiError(`Request failed (${response.status}).`, response.status, {
      requestId: response.headers.get('X-Request-ID') ?? '',
    })
    let body
    try {
      body = await response.json()
    } catch {
      // Keep the status-based fallback when an upstream response is not JSON.
    }
    if (
      body?.status === response.status &&
      typeof body.code === 'string' && body.code &&
      typeof body.message === 'string' && body.message &&
      typeof body.meta?.time === 'string' && body.meta.time &&
      typeof body.meta?.requestId === 'string' && body.meta.requestId
    ) {
      throw new ApiError(body.message, response.status, {
        code: body.code,
        requestId: body.meta.requestId,
        rawError: body.rawError,
      })
    }
    throw fallback
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

function coinValue(value, field) {
  if (typeof value !== 'string' || !/^\d+$/.test(value)) {
    throw new ApiError(`The game server returned an invalid ${field}.`)
  }
  return value
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

export async function getWallet({ signal } = {}) {
  const dto = await request('/api/wallet', { headers: await protectedHeaders(), signal })
  return { balance: coinValue(dto?.balance, 'wallet balance') }
}

export async function rechargeWallet(amount, idempotencyKey, { signal } = {}) {
  if (typeof amount !== 'string' || !/^[1-9]\d*$/.test(amount)) {
    throw new ApiError('Enter a positive whole coin amount.')
  }
  const dto = await request('/api/wallet/recharges', {
    method: 'POST',
    headers: { ...await protectedHeaders(), 'Content-Type': 'application/json', 'Idempotency-Key': idempotencyKey },
    body: `{"amount":${amount}}`,
    signal,
  })
  return { balance: coinValue(dto?.balance, 'wallet balance') }
}

export async function getRoundAnalytics({ signal } = {}) {
  const dto = await request('/api/analytics/rounds', { headers: await protectedHeaders(), signal })
  if (!Array.isArray(dto?.rounds)) throw new ApiError('The game server returned invalid round analytics.')
  return {
    totalHouseEarnings: coinValue(dto.total_house_earnings, 'house earnings'),
    rounds: dto.rounds.map((round) => ({
      roomCode: round.room_code,
      round: round.round,
      result: round.result,
      winnerRole: round.winner_role || null,
      forfeit: round.forfeit,
      houseEarnings: coinValue(round.house_earnings, 'round house earnings'),
      resolvedAt: round.resolved_at,
    })),
  }
}
