import { APP_VERSION } from './version.js'

const STORAGE_KEY = 'rock-paper-money.session'
const ROLES = new Set(['host', 'guest'])
const SEMANTIC_VERSION = /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/

function parseVersion(version) {
  if (typeof version !== 'string') return null
  const match = SEMANTIC_VERSION.exec(version)
  return match ? match.slice(1).map(Number) : null
}

function isCompatibleVersion(storedVersion, currentVersion) {
  const stored = parseVersion(storedVersion)
  const current = parseVersion(currentVersion)
  return Boolean(
    stored &&
      current &&
      stored[0] === current[0] &&
      stored[1] === current[1] &&
      stored[2] <= current[2],
  )
}

export function isValidSession(value) {
  return Boolean(
    value &&
      typeof value.roomCode === 'string' &&
      /^[A-Z0-9]{6}$/.test(value.roomCode) &&
      typeof value.playerToken === 'string' &&
      value.playerToken.length > 0 &&
      !/\s/.test(value.playerToken) &&
      ROLES.has(value.role),
  )
}

export function loadSession(storage = localStorage, currentVersion = APP_VERSION) {
  try {
    const value = JSON.parse(storage.getItem(STORAGE_KEY))
    if (!isValidSession(value) || !isCompatibleVersion(value.version, currentVersion)) {
      storage.removeItem(STORAGE_KEY)
      return null
    }

    const session = { roomCode: value.roomCode, playerToken: value.playerToken, role: value.role }
    if (value.version !== currentVersion) {
      storage.setItem(STORAGE_KEY, JSON.stringify({ ...session, version: currentVersion }))
    }
    return session
  } catch {
    try {
      storage.removeItem(STORAGE_KEY)
    } catch {
      // Storage can be unavailable in privacy-restricted browser contexts.
    }
    return null
  }
}

export function saveSession(session, storage = localStorage, currentVersion = APP_VERSION) {
  if (!isValidSession(session)) throw new Error('Cannot save an invalid room session.')
  if (!parseVersion(currentVersion)) throw new Error('Cannot save a session with an invalid application version.')
  storage.setItem(STORAGE_KEY, JSON.stringify({ ...session, version: currentVersion }))
}

export function clearSession(storage = localStorage) {
  storage.removeItem(STORAGE_KEY)
}

export { APP_VERSION, STORAGE_KEY }
