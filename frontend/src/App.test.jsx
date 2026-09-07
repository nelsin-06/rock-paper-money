import { StrictMode } from 'react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import App from './App.jsx'
import * as api from './api.js'
import { STORAGE_KEY } from './storage.js'

vi.mock('./api.js')

const hostSession = { roomCode: 'ABC234', playerToken: 'host-secret', role: 'host' }
const waitingState = {
  roomCode: 'ABC234', ready: false, resolved: false,
  players: [{ role: 'host', wins: 0, submitted: false }], result: null, moves: [],
}
const readyState = {
  roomCode: 'ABC234', ready: true, resolved: false,
  players: [
    { role: 'host', wins: 0, submitted: false },
    { role: 'guest', wins: 0, submitted: false },
  ], result: null, moves: [],
}

describe('core room flow', () => {
  beforeEach(() => {
    localStorage.clear()
    api.createRoom.mockReset()
    api.joinRoom.mockReset()
    api.getRoomState.mockReset()
    api.submitMove.mockReset()
    api.startNextRound.mockReset()
  })

  it('shows home and creates a room while persisting private credentials', async () => {
    api.createRoom.mockResolvedValue(hostSession)
    api.getRoomState.mockResolvedValue(waitingState)
    render(<App />)

    expect(screen.getByRole('heading', { name: /rock.*paper.*money/i })).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Create room' }))

    expect(await screen.findByRole('heading', { name: /waiting for a guest/i })).toBeInTheDocument()
    expect(screen.getByLabelText('Room code ABC234')).toBeInTheDocument()
    expect(screen.queryByText('host-secret')).not.toBeInTheDocument()
    expect(JSON.parse(localStorage.getItem(STORAGE_KEY))).toEqual(hostSession)
  })

  it('joins using a normalized six-character code', async () => {
    api.joinRoom.mockResolvedValue({ ...hostSession, role: 'guest' })
    api.getRoomState.mockResolvedValue(readyState)
    render(<App />)

    await userEvent.type(screen.getByLabelText(/six-character room code/i), 'abc234')
    await userEvent.click(screen.getByRole('button', { name: 'Join room' }))

    await screen.findByRole('heading', { name: 'Choose your move' })
    expect(api.joinRoom).toHaveBeenCalledWith('ABC234')
  })

  it('locks same-tick duplicate create attempts', async () => {
    api.createRoom.mockResolvedValue(hostSession)
    api.getRoomState.mockResolvedValue(waitingState)
    render(<App />)

    const create = screen.getByRole('button', { name: 'Create room' })
    act(() => {
      create.click()
      create.click()
    })

    expect(api.createRoom).toHaveBeenCalledTimes(1)
    expect(await screen.findByRole('heading', { name: /waiting for a guest/i })).toBeInTheDocument()
  })

  it('locks same-tick duplicate join attempts', async () => {
    api.joinRoom.mockResolvedValue({ ...hostSession, role: 'guest' })
    api.getRoomState.mockResolvedValue(readyState)
    render(<App />)
    fireEvent.change(screen.getByLabelText(/six-character room code/i), { target: { value: 'ABC234' } })

    const join = screen.getByRole('button', { name: 'Join room' })
    act(() => {
      join.click()
      join.click()
    })

    expect(api.joinRoom).toHaveBeenCalledTimes(1)
    expect(await screen.findByRole('heading', { name: 'Choose your move' })).toBeInTheDocument()
  })

  it('releases the home entry lock after failure so create can be retried', async () => {
    api.createRoom.mockRejectedValueOnce(new Error('Create failed')).mockResolvedValue(hostSession)
    api.getRoomState.mockResolvedValue(waitingState)
    render(<App />)

    await userEvent.click(screen.getByRole('button', { name: 'Create room' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('Create failed')
    await userEvent.click(screen.getByRole('button', { name: 'Create room' }))

    expect(api.createRoom).toHaveBeenCalledTimes(2)
    expect(await screen.findByRole('heading', { name: /waiting for a guest/i })).toBeInTheDocument()
  })

  it('restores a room, submits only one move, and hides choices before resolution', async () => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(hostSession))
    api.getRoomState.mockResolvedValue(readyState)
    api.submitMove.mockResolvedValue()
    render(<App />)

    const choices = await screen.findByLabelText('Choose a move')
    expect(screen.getByText(/stays secret until both players/i)).toBeInTheDocument()
    expect(screen.queryByText(/opponent.*rock/i)).not.toBeInTheDocument()

    const rock = within(choices).getByRole('button', { name: 'Rock' })
    await userEvent.dblClick(rock)
    await waitFor(() => expect(api.submitMove).toHaveBeenCalledTimes(1))
    expect(api.submitMove).toHaveBeenCalledWith('ABC234', 'host-secret', 'rock')
  })

  it('recovers visible action state after a StrictMode move failure', async () => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(hostSession))
    api.getRoomState.mockResolvedValue(readyState)
    api.submitMove.mockRejectedValue(new Error('Move failed'))
    render(<StrictMode><App /></StrictMode>)

    const rock = await screen.findByRole('button', { name: 'Rock' })
    await userEvent.click(rock)

    expect(await screen.findByRole('alert')).toHaveTextContent('Move failed')
    expect(rock).toBeEnabled()
    expect(screen.getByRole('heading', { name: 'Choose your move' })).toBeInTheDocument()
  })

  it('shows submitted readiness from server truth without revealing either move', async () => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(hostSession))
    const submitted = {
      ...readyState,
      players: [{ role: 'host', wins: 0, submitted: true }, { role: 'guest', wins: 0, submitted: false }],
    }
    api.getRoomState.mockResolvedValue(submitted)
    render(<App />)

    expect(await screen.findByRole('heading', { name: 'Move submitted' })).toBeInTheDocument()
    expect(screen.queryByText('paper')).not.toBeInTheDocument()
    expect(screen.getByText('Move locked')).toBeInTheDocument()
  })

  it('shows a resolved result, supports next round, and leaves without deleting server room', async () => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(hostSession))
    const resolved = {
      roomCode: 'ABC234', ready: true, resolved: true, result: 'player_one_wins',
      players: [{ role: 'host', wins: 1, submitted: true }, { role: 'guest', wins: 0, submitted: true }],
      moves: [{ role: 'host', move: 'paper' }, { role: 'guest', move: 'rock' }],
    }
    const nextRound = {
      ...readyState,
      players: [{ role: 'host', wins: 1, submitted: false }, { role: 'guest', wins: 0, submitted: false }],
    }
    api.getRoomState.mockResolvedValueOnce(resolved).mockResolvedValue(nextRound)
    api.startNextRound.mockResolvedValue()
    render(<App />)

    expect(await screen.findByRole('heading', { name: 'You won' })).toBeInTheDocument()
    expect(screen.getByText('paper')).toBeInTheDocument()
    expect(screen.getByText('rock')).toBeInTheDocument()
    expect(screen.getByLabelText('host score')).toHaveTextContent('1')

    await userEvent.click(screen.getByRole('button', { name: 'Next round' }))
    expect(api.startNextRound).toHaveBeenCalledWith('ABC234', 'host-secret')
    expect(await screen.findByRole('heading', { name: 'Choose your move' })).toBeInTheDocument()
    expect(screen.getByLabelText('host score')).toHaveTextContent('1')

    expect(screen.getByText(/server room remains open/i)).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Leave room' }))
    expect(screen.getByRole('button', { name: 'Create room' })).toBeInTheDocument()
    expect(localStorage.getItem(STORAGE_KEY)).toBeNull()
  })

  it('keeps credentials and offers retry after a polling failure', async () => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(hostSession))
    api.getRoomState.mockRejectedValueOnce(new Error('Network unavailable')).mockResolvedValue(waitingState)
    render(<App />)

    expect(await screen.findByRole('alert')).toHaveTextContent('Network unavailable')
    expect(localStorage.getItem(STORAGE_KEY)).not.toBeNull()
    await userEvent.click(screen.getByRole('button', { name: 'Retry' }))
    expect(await screen.findByRole('heading', { name: /waiting for a guest/i })).toBeInTheDocument()
  })
})
