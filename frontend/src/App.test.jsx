import { StrictMode } from 'react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
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

    await userEvent.click(await screen.findByRole('button', { name: 'Play again' }))
    expect(api.startNextRound).toHaveBeenCalledWith('ABC234', 1)
    await userEvent.click(screen.getByRole('button', { name: 'Menu' }))
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

  it('loads only the player wallet and reuses a recharge key after ambiguity', async () => {
    api.getWallet.mockResolvedValue({ balance: '0' })
    api.rechargeWallet.mockRejectedValueOnce(new Error('Connection lost')).mockResolvedValueOnce({ balance: '50' })
    vi.spyOn(crypto, 'randomUUID').mockReturnValue('stable-retry-key')
    render(<AccountPanel onBalance={() => {}} />)
    await screen.findByText('0 coins')
    expect(api.getRoundAnalytics).not.toHaveBeenCalled()
    expect(screen.queryByText(/house earnings|played rounds/i)).not.toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: /coin wallet/i }))
    await userEvent.type(screen.getByLabelText('Recharge amount'), '50')
    await userEvent.click(screen.getByRole('button', { name: 'Add coins' }))
    await screen.findByText('Connection lost')
    await userEvent.click(screen.getByRole('button', { name: 'Add coins' }))
    await waitFor(() => expect(api.rechargeWallet).toHaveBeenNthCalledWith(2, '50', 'stable-retry-key'))
    expect(within(screen.getByRole('dialog')).getByText('50 coins')).toBeInTheDocument()
  })

  it('opens a functional join dialog and locks duplicate join submissions', async () => {
    api.joinRoom.mockResolvedValue(roomSession)
    streamRoomState({ ...readyState, playerRole: 'guest' })
    render(<GameApp walletBalance="100" />)
    await userEvent.click(screen.getByRole('button', { name: 'Join room' }))
    const dialog = screen.getByRole('dialog', { name: 'Join a room' })
    await userEvent.type(within(dialog).getByLabelText('Six-character room code'), 'ab-c234')
    expect(within(dialog).getByLabelText('Six-character room code')).toHaveValue('ABC234')
    const join = within(dialog).getByRole('button', { name: 'Join room' })
    act(() => { join.click(); join.click() })
    expect(api.joinRoom).toHaveBeenCalledExactlyOnceWith('ABC234')
    await screen.findByRole('heading', { name: 'Choose your move' })
    expect(JSON.parse(localStorage.getItem(STORAGE_KEY))).toEqual(persistedSession)
  })

  it('closes the join dialog on cancel and returns focus to its trigger', async () => {
    render(<GameApp />)
    const trigger = screen.getByRole('button', { name: 'Join room' })
    await userEvent.click(trigger)
    fireEvent(screen.getByRole('dialog'), new Event('cancel', { bubbles: false, cancelable: true }))
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(trigger).toHaveFocus()
  })

  it('blocks underfunded entry and makes the wallet the recharge path', () => {
    render(<GameApp walletBalance="49" />)
    expect(screen.getByRole('button', { name: 'Create room' })).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Join room' })).toBeDisabled()
    expect(screen.getByText(/use your wallet to recharge/i)).toBeInTheDocument()
    expect(api.createRoom).not.toHaveBeenCalled()
  })

  it('copies the real waiting room code and permits an allowed exit', async () => {
    const user = userEvent.setup()
    const copy = vi.spyOn(navigator.clipboard, 'writeText').mockResolvedValue()
    localStorage.setItem(STORAGE_KEY, JSON.stringify(persistedSession))
    streamRoomState(waitingState)
    api.leaveRoom.mockResolvedValue()
    render(<GameApp />)
    await user.click(screen.getByRole('button', { name: 'Copy room code' }))
    expect(copy).toHaveBeenCalledWith('ABC234')
    expect(screen.getByText('Copied to clipboard.')).toBeInTheDocument()
    expect(screen.getByText('Awaiting funding')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Leave room' }))
    expect(api.leaveRoom).toHaveBeenCalledWith('ABC234')
    expect(await screen.findByRole('button', { name: 'Create room' })).toBeInTheDocument()
  })

  it('keeps a committed move locked until authoritative snapshots resolve it', async () => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(persistedSession))
    streamRoomState(readyState)
    api.submitMove.mockResolvedValue()
    render(<GameApp />)
    await userEvent.click(screen.getByRole('button', { name: 'Rock' }))
    expect(screen.getByRole('heading', { name: 'Move submitted' })).toBeInTheDocument()
    expect(screen.queryByRole('region', { name: 'Round result' })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Paper' })).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Leave room' })).toBeDisabled()
    act(() => activeSocket.onState({ ...readyState, revision: 3 }))
    expect(screen.getByRole('button', { name: 'Rock' })).toBeDisabled()
    act(() => activeSocket.onState(resolvedState))
    expect(screen.getByRole('heading', { name: 'You won' })).toBeInTheDocument()
  })

  it('unlocks a failed move and displays its error', async () => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(persistedSession))
    streamRoomState(readyState)
    api.submitMove.mockRejectedValue(new Error('Move rejected'))
    render(<GameApp />)
    await userEvent.click(screen.getByRole('button', { name: 'Rock' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('Move rejected')
    expect(screen.getByRole('button', { name: 'Rock' })).toBeEnabled()
  })

  it('uses actual round numbers without a fabricated countdown or round limit', () => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(persistedSession))
    streamRoomState({ ...readyState, round: 12 })
    render(<GameApp />)
    expect(screen.getByLabelText('Round details')).toHaveTextContent('Round 12')
    expect(screen.getByLabelText('Round details')).toHaveTextContent('100 coins · Pot')
    expect(screen.getByLabelText('Round details')).not.toHaveTextContent(/\d+\/3|\d+:\d+/)
  })

  it('waits for mutual rematch consent and preserves action locking', async () => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(persistedSession))
    streamRoomState(resolvedState)
    let finishRequest
    api.startNextRound.mockReturnValue(new Promise((resolve) => { finishRequest = resolve }))
    render(<GameApp walletBalance="100" />)
    const play = screen.getByRole('button', { name: 'Play again' })
    act(() => { play.click(); play.click() })
    expect(api.startNextRound).toHaveBeenCalledExactlyOnceWith('ABC234', 1)
    expect(screen.getByRole('button', { name: 'Menu' })).toBeDisabled()
    await act(async () => finishRequest())
    expect(screen.getByRole('heading', { name: 'You won' })).toBeInTheDocument()
    act(() => activeSocket.onState({ ...resolvedState, players: resolvedState.players.map((player) => ({ ...player, wantsNextRound: player.role === 'host' })) }))
    expect(screen.getByRole('button', { name: 'Waiting for opponent…' })).toBeDisabled()
    expect(screen.getByText(/only when both players agree/)).toBeInTheDocument()
    act(() => activeSocket.onState({ ...readyState, round: 2 }))
    expect(screen.getByRole('heading', { name: 'Choose your move' })).toBeInTheDocument()
  })

  it('requires enough balance to accept an opponent rematch request', () => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(persistedSession))
    streamRoomState({ ...resolvedState, players: resolvedState.players.map((player) => ({ ...player, wantsNextRound: player.role === 'guest' })) })
    const { rerender } = render(<GameApp walletBalance="49" />)
    expect(screen.getByRole('button', { name: 'Accept next round' })).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Menu' })).toBeEnabled()
    rerender(<GameApp walletBalance="50" />)
    expect(screen.getByRole('button', { name: 'Accept next round' })).toBeEnabled()
    expect(connectToRoom).toHaveBeenCalledOnce()
  })

  it('preserves the room and displays feedback when leaving fails', async () => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(persistedSession))
    streamRoomState(resolvedState)
    api.leaveRoom.mockRejectedValue(new Error('Unable to leave'))
    render(<GameApp />)
    const menu = screen.getByRole('button', { name: 'Menu' })
    act(() => { menu.click(); menu.click() })
    expect(api.leaveRoom).toHaveBeenCalledOnce()
    expect(await screen.findByRole('alert')).toHaveTextContent('Unable to leave')
    expect(localStorage.getItem(STORAGE_KEY)).not.toBeNull()
    expect(closeSocket).not.toHaveBeenCalled()
  })

  it('refreshes only the wallet on funding, resolution, and the next round without replacing the socket', async () => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(persistedSession))
    streamRoomState(waitingState)
    api.getWallet.mockResolvedValue({ balance: '100' })
    const onBalance = vi.fn()
    render(<><AccountPanel onBalance={onBalance} /><GameApp walletBalance="100" /></>)
    await waitFor(() => expect(api.getWallet).toHaveBeenCalledTimes(1))
    act(() => activeSocket.onState(readyState))
    await waitFor(() => expect(api.getWallet).toHaveBeenCalledTimes(2))
    act(() => activeSocket.onState(resolvedState))
    await waitFor(() => expect(api.getWallet).toHaveBeenCalledTimes(3))
    act(() => activeSocket.onState({ ...readyState, round: 2 }))
    await waitFor(() => expect(api.getWallet).toHaveBeenCalledTimes(4))
    expect(api.getRoundAnalytics).not.toHaveBeenCalled()
    expect(connectToRoom).toHaveBeenCalledOnce()
  })
})
