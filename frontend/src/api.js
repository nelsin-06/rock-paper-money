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

function csrfToken() {
  const prefix = 'rpm_csrf='
  const cookie = document.cookie.split(';').map((value) => value.trim()).find((value) => value.startsWith(prefix))
  return cookie ? decodeURIComponent(cookie.slice(prefix.length)) : ''
}

async function request(path, options = {}) {
  const { retryNetwork = false, ...fetchOptions } = options
  const requestOptions = { credentials: 'same-origin', ...fetchOptions }
  let response
  for (let attempt = 0; attempt < 2; attempt += 1) {
    try {
      response = await fetch(path, requestOptions)
      break
    } catch (error) {
      if (error?.name === 'AbortError') throw error
      if (attempt === 1 || !retryNetwork) {
        throw new ApiError('Cannot reach the game server. Check your connection and try again.')
      }
    }
  }

  if (!response.ok) {
    const fallback = new ApiError(`Request failed (${response.status}).`, response.status, {
      requestId: response.headers.get('X-Request-ID') ?? '',
    })
    let body
    try {
      body = await response.json()
    } catch {
      // Preserve the status-based fallback for non-JSON upstream failures.
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

function mutationHeaders({ json = false, idempotent = true, idempotencyKey } = {}) {
  const csrf = csrfToken()
  if (!csrf) throw new ApiError('Your browser session is missing its security token. Sign in again.', 401)
  const headers = { 'X-CSRF-Token': csrf }
  if (json) headers['Content-Type'] = 'application/json'
  if (idempotent) headers['Idempotency-Key'] = idempotencyKey || crypto.randomUUID()
  return headers
}

function mapRoomReference(dto) {
  if (typeof dto?.room_code !== 'string') throw new ApiError('The game server returned an invalid room reference.')
  return { roomCode: dto.room_code }
}

export function mapRoomState(dto) {
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
  if (typeof value !== 'string' || !/^\d+$/.test(value)) throw new ApiError(`The game server returned an invalid ${field}.`)
  return value
}

export async function bootstrapSession(accessToken, { signal } = {}) {
  if (typeof accessToken !== 'string' || !accessToken) throw new ApiError('Your account session has expired. Sign in again.', 401)
  await request('/api/session', {
    method: 'POST',
    credentials: 'same-origin',
    headers: { Authorization: `Bearer ${accessToken}` },
    signal,
  })
}

export async function logoutSession({ signal } = {}) {
  await request('/api/session', {
    method: 'DELETE',
    credentials: 'same-origin',
    headers: mutationHeaders({ idempotent: false }),
    signal,
  })
}

export async function createRoom({ signal } = {}) {
  return mapRoomReference(await request('/api/rooms', {
    method: 'POST', headers: mutationHeaders(), retryNetwork: true, signal,
  }))
}

export async function joinRoom(code, { signal } = {}) {
  const normalized = code.trim().toUpperCase()
  return mapRoomReference(await request(`/api/rooms/${encodeURIComponent(normalized)}/join`, {
    method: 'POST', headers: mutationHeaders(), retryNetwork: true, signal,
  }))
}

export async function getRoomState(code, { signal } = {}) {
  return mapRoomState(await request(`/api/rooms/${encodeURIComponent(code)}/state`, { signal }))
}

export async function submitMove(code, move, { signal } = {}) {
  await request(`/api/rooms/${encodeURIComponent(code)}/moves`, {
    method: 'POST', headers: mutationHeaders({ json: true }), body: JSON.stringify({ move }), retryNetwork: true, signal,
  })
}

export async function startNextRound(code, round, { signal } = {}) {
  await request(`/api/rooms/${encodeURIComponent(code)}/next-round`, {
    method: 'POST', headers: mutationHeaders({ json: true }), body: JSON.stringify({ round }), retryNetwork: true, signal,
  })
}

export async function leaveRoom(code, { signal } = {}) {
  await request(`/api/rooms/${encodeURIComponent(code)}/leave`, {
    method: 'POST', headers: mutationHeaders(), retryNetwork: true, signal,
  })
}

export async function getWallet({ signal } = {}) {
  const dto = await request('/api/wallet', { signal })
  return { balance: coinValue(dto?.balance, 'wallet balance') }
}

export async function rechargeWallet(amount, idempotencyKey, { signal } = {}) {
  if (typeof amount !== 'string' || !/^[1-9]\d*$/.test(amount)) throw new ApiError('Enter a positive whole coin amount.')
  const dto = await request('/api/wallet/recharges', {
    method: 'POST',
    headers: mutationHeaders({ json: true, idempotencyKey }),
    body: `{"amount":${amount}}`,
    retryNetwork: true,
    signal,
  })
  return { balance: coinValue(dto?.balance, 'wallet balance') }
}

export async function getRoundAnalytics({ signal } = {}) {
  const dto = await request('/api/analytics/rounds', { signal })
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
