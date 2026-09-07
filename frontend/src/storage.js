const STORAGE_KEY = 'rock-paper-money.session'
const ROLES = new Set(['host', 'guest'])

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

export function loadSession(storage = localStorage) {
  try {
    const value = JSON.parse(storage.getItem(STORAGE_KEY))
    return isValidSession(value) ? value : null
  } catch {
    return null
  }
}

export function saveSession(session, storage = localStorage) {
  if (!isValidSession(session)) throw new Error('Cannot save an invalid room session.')
  storage.setItem(STORAGE_KEY, JSON.stringify(session))
}

export function clearSession(storage = localStorage) {
  storage.removeItem(STORAGE_KEY)
}

export { STORAGE_KEY }
