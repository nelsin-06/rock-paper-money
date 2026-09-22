import { ApiError, mapRoomState } from './api.js'

function socketURL(roomCode) {
  const url = new URL(`/api/rooms/${encodeURIComponent(roomCode)}/socket`, window.location.href)
  url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:'
  return url.toString()
}

function mapFrame(frame) {
  if (
    frame?.type !== 'snapshot' ||
    !Number.isSafeInteger(frame.revision) || frame.revision < 0 ||
    typeof frame.player?.account_id !== 'string' || !frame.player.account_id ||
    !['host', 'guest'].includes(frame.player?.role)
  ) {
    throw new ApiError('The game server returned an invalid room update.')
  }
  return {
    ...mapRoomState(frame.room),
    revision: frame.revision,
    playerRole: frame.player.role,
    presence: frame.presence ?? {},
  }
}

export function connectToRoom(roomCode, { onState, onOpen, onError } = {}, options = {}) {
  const reconnectDelay = options.reconnectDelay ?? ((attempt) => Math.min(1000 * (2 ** attempt), 10000))
  let socket
  let retryTimer
  let stopped = false
  let reconnectAttempt = 0
  let lastRevision = -1
  let initialFrame = true

  const scheduleReconnect = () => {
    if (stopped || retryTimer !== undefined) return
    retryTimer = window.setTimeout(() => {
      retryTimer = undefined
      open()
    }, reconnectDelay(reconnectAttempt++))
  }

  const open = () => {
    if (stopped) return
    initialFrame = true
    socket = new WebSocket(socketURL(roomCode))
    socket.onopen = () => {
      reconnectAttempt = 0
      onOpen?.()
    }
    socket.onmessage = (event) => {
      try {
        const next = mapFrame(JSON.parse(event.data))
        if (next.revision <= lastRevision) return
        if (!initialFrame && lastRevision >= 0 && next.revision > lastRevision + 1) {
          onError?.(new ApiError('Live room updates skipped a revision. Reconnecting…'))
          socket.close()
          scheduleReconnect()
          return
        }
        initialFrame = false
        lastRevision = next.revision
        onState?.(next)
      } catch (error) {
        onError?.(error instanceof ApiError ? error : new ApiError('The game server returned an invalid room update.'))
      }
    }
    socket.onerror = () => onError?.(new ApiError('Live room updates disconnected. Reconnecting…'))
    socket.onclose = () => scheduleReconnect()
  }

  open()
  return () => {
    stopped = true
    if (retryTimer !== undefined) window.clearTimeout(retryTimer)
    if (socket?.readyState === WebSocket.OPEN) socket.close(1000, 'room closed')
  }
}
