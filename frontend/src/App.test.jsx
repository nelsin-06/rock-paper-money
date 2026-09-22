import { StrictMode } from 'react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { AccountPanel, GameApp } from './App.jsx'
import * as api from './api.js'
import { connectToRoom } from './realtime.js'
import { APP_VERSION, STORAGE_KEY } from './storage.js'

vi.mock('./api.js')
vi.mock('./realtime.js')

const roomSession = { roomCode: 'ABC234' }
const persistedSession = { ...roomSession, version: APP_VERSION }
const waitingState = {
  roomCode: 'ABC234', revision: 1, playerRole: 'host', ready: false, resolved: false, round: 1, closed: false, forfeit: false,
  players: [{ role: 'host', wins: 0, submitted: false, wantsNextRound: false }], result: null, moves: [], presence: { host: true },
}
const readyState = {
  ...waitingState, revision: 2, ready: true,
  players: [
    { role: 'host', wins: 0, submitted: false, wantsNextRound: false },
    { role: 'guest', wins: 0, submitted: false, wantsNextRound: false },
  ],
}
const resolvedState = {
  ...readyState, revision: 3, resolved: true, result: 'player_one_wins',
  players: [
    { role: 'host', wins: 1, submitted: true, wantsNextRound: false },
    { role: 'guest', wins: 0, submitted: true, wantsNextRound: false },
  ],
  moves: [{ role: 'host', move: 'paper' }, { role: 'guest', move: 'rock' }],
}

let activeSocket
let closeSocket

function streamRoomState(initialState) {
  connectToRoom.mockImplementation((_code, handlers) => {
    activeSocket = handlers
    closeSocket = vi.fn()
    handlers.onOpen?.()
    handlers.onState(initialState)
    return closeSocket
  })
}

describe('token-free room flow', () => {
  afterEach(() => vi.restoreAllMocks())

  beforeEach(() => {
    localStorage.clear()
    Object.values(api).forEach((value) => value?.mockReset?.())
    connectToRoom.mockReset()
  })

  it('creates a room and persists only its non-secret room reference', async () => {
    api.createRoom.mockResolvedValue(roomSession)
    streamRoomState(waitingState)
    render(<GameApp />)
    await userEvent.click(screen.getByRole('button', { name: 'Create room' }))

    expect(await screen.findByRole('heading', { name: /waiting for a guest/i })).toBeInTheDocument()
    expect(JSON.parse(localStorage.getItem(STORAGE_KEY))).toEqual(persistedSession)
    expect(localStorage.getItem(STORAGE_KEY)).not.toMatch(/token|role/i)
  })

  it('restores by room code and derives the player role from the socket snapshot', async () => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(persistedSession))
    streamRoomState(readyState)
    api.submitMove.mockResolvedValue()
    render(<GameApp />)

    expect(await screen.findByText('You')).toBeInTheDocument()
    await userEvent.click(within(screen.getByLabelText('Choose a move')).getByRole('button', { name: 'Rock' }))
    expect(api.submitMove).toHaveBeenCalledWith('ABC234', 'rock')
    expect(connectToRoom).toHaveBeenCalledWith('ABC234', expect.any(Object))
  })

  it('locks same-tick duplicate create and move actions', async () => {
    api.createRoom.mockResolvedValue(roomSession)
    streamRoomState(readyState)
    render(<StrictMode><GameApp /></StrictMode>)
    const create = screen.getByRole('button', { name: 'Create room' })
    act(() => { create.click(); create.click() })
    expect(api.createRoom).toHaveBeenCalledTimes(1)
    const rock = await screen.findByRole('button', { name: 'Rock' })
    act(() => { rock.click(); rock.click() })
    expect(api.submitMove).toHaveBeenCalledTimes(1)
  })

  it('uses token-free next-round and leave commands', async () => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(persistedSession))
    streamRoomState(resolvedState)
    api.startNextRound.mockResolvedValue()
    api.leaveRoom.mockResolvedValue()
    render(<GameApp />)

    await userEvent.click(await screen.findByRole('button', { name: 'Request next round' }))
    expect(api.startNextRound).toHaveBeenCalledWith('ABC234', 1)
    await userEvent.click(screen.getByRole('button', { name: 'Leave room' }))
    expect(api.leaveRoom).toHaveBeenCalledWith('ABC234')
    expect(localStorage.getItem(STORAGE_KEY)).toBeNull()
    expect(closeSocket).toHaveBeenCalledOnce()
  })

  it('removes the local room when an authoritative snapshot closes it', async () => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(persistedSession))
    streamRoomState(readyState)
    render(<GameApp />)
    await screen.findByRole('heading', { name: 'Choose your move' })
    act(() => activeSocket.onState({ ...readyState, revision: 3, closed: true }))
    expect(screen.getByRole('button', { name: 'Create room' })).toBeInTheDocument()
    expect(localStorage.getItem(STORAGE_KEY)).toBeNull()
  })

  it('replaces the WebSocket connection when the user retries', async () => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(persistedSession))
    const firstClose = vi.fn()
    connectToRoom
      .mockImplementationOnce((_code, handlers) => {
        handlers.onError(new Error('Network unavailable'))
        return firstClose
      })
      .mockImplementationOnce((_code, handlers) => {
        handlers.onState(waitingState)
        return vi.fn()
      })
    render(<GameApp />)
    await userEvent.click(await screen.findByRole('button', { name: 'Retry' }))
    expect(await screen.findByRole('heading', { name: /waiting for a guest/i })).toBeInTheDocument()
    expect(firstClose).toHaveBeenCalledOnce()
    expect(connectToRoom).toHaveBeenCalledTimes(2)
  })

  it('keeps the WebSocket connection open across wallet-driven rerenders', async () => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(persistedSession))
    streamRoomState(readyState)
    const { rerender, unmount } = render(<GameApp walletBalance="100" />)
    await screen.findByRole('heading', { name: 'Choose your move' })

    rerender(<GameApp walletBalance="50" />)

    expect(connectToRoom).toHaveBeenCalledOnce()
    expect(closeSocket).not.toHaveBeenCalled()

    unmount()
    expect(closeSocket).toHaveBeenCalledOnce()
  })

  it('shows wallet analytics and reuses a recharge key after ambiguity', async () => {
    api.getWallet.mockResolvedValue({ balance: '0' })
    api.getRoundAnalytics.mockResolvedValue({ totalHouseEarnings: '0', rounds: [] })
    api.rechargeWallet.mockRejectedValueOnce(new Error('Connection lost')).mockResolvedValueOnce({ balance: '50' })
    vi.spyOn(crypto, 'randomUUID').mockReturnValue('stable-retry-key')
    render(<AccountPanel onBalance={() => {}} />)
    await screen.findByText('0 coins')
    await userEvent.type(screen.getByLabelText('Recharge amount'), '50')
    await userEvent.click(screen.getByRole('button', { name: 'Add coins' }))
    await screen.findByText('Connection lost')
    await userEvent.click(screen.getByRole('button', { name: 'Add coins' }))
    await waitFor(() => expect(api.rechargeWallet).toHaveBeenNthCalledWith(2, '50', 'stable-retry-key'))
  })
})
