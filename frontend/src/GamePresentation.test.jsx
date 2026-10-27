import { describe, expect, it, vi } from 'vitest'
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { CopyButton, ReportProblem, RoundResult } from './GamePresentation.jsx'
import * as api from './api.js'
vi.mock('./api.js', () => ({ submitProblemReport: vi.fn() }))

const completedRound = {
  round: 7,
  result: 'player_one_wins',
  moves: [{ role: 'host', move: 'paper' }, { role: 'guest', move: 'rock' }],
}

describe('role-relative round presentation', () => {
  it.each([
    ['host', 'player_one_wins', 'You won', '75 coins payout', '+25 coins net'],
    ['guest', 'player_one_wins', 'You lost', '0 coins payout', '-50 coins net'],
    ['host', 'player_two_wins', 'You lost', '0 coins payout', '-50 coins net'],
    ['guest', 'player_two_wins', 'You won', '75 coins payout', '+25 coins net'],
    ['host', 'draw', 'Draw round', '50 coins refunded', '0 coins net'],
    ['guest', 'draw', 'Draw round', '50 coins refunded', '0 coins net'],
  ])('shows %s / %s with the real payout and net', (role, result, title, payout, net) => {
    render(<RoundResult state={{ ...completedRound, result }} role={role} />)
    expect(screen.getByRole('heading', { name: title })).toBeInTheDocument()
    expect(screen.getByText(payout)).toBeInTheDocument()
    expect(screen.getByText(`${net} · 50-coin stake`)).toBeInTheDocument()
    expect(screen.getByText('Round 7 complete')).toBeInTheDocument()
    expect(within(screen.getByRole('group', { name: 'Your move' })).getByText(role === 'host' ? 'paper' : 'rock')).toBeInTheDocument()
  })

  it('shows a deadline draw without inventing either choice', () => {
    render(<RoundResult state={{ ...completedRound, result: 'draw', moves: [] }} role="guest" />)
    expect(screen.getAllByText('No move revealed')).toHaveLength(2)
    expect(screen.getByText('The round ended before both moves were submitted.')).toBeInTheDocument()
    expect(screen.getByText('50 coins refunded')).toBeInTheDocument()
  })

  it('shows an inactivity win with only the actual submitted choice', () => {
    render(<RoundResult state={{ ...completedRound, moves: [{ role: 'host', move: 'paper' }] }} role="host" />)
    expect(screen.getByText('paper')).toBeInTheDocument()
    expect(screen.getByText('No move revealed')).toBeInTheDocument()
    expect(screen.getByText('75 coins payout')).toBeInTheDocument()
  })

  it.each(['host', 'guest'])('explains a %s forfeit while retaining available moves', (role) => {
    render(<RoundResult state={{ ...completedRound, forfeit: true, moves: [{ role: 'host', move: 'paper' }] }} role={role} />)
    expect(screen.getByText('Round 7 complete · Forfeit')).toBeInTheDocument()
    expect(screen.getByText(role === 'host' ? 'Your opponent disconnected and did not return.' : 'You did not reconnect before the grace period ended.')).toBeInTheDocument()
    expect(screen.getByText('No move revealed')).toBeInTheDocument()
    expect(screen.getByText('paper')).toBeInTheDocument()
  })

  it('does not call an unknown result a loss or invent its payout', () => {
    render(<RoundResult state={{ ...completedRound, result: null }} role="host" />)
    expect(screen.getByRole('heading', { name: 'Round complete' })).toBeInTheDocument()
    expect(screen.queryByText(/coins payout/)).not.toBeInTheDocument()
  })
})

describe('local clipboard tools', () => {
  it('provides honest copy failure feedback', async () => {
    const user = userEvent.setup()
    vi.spyOn(navigator.clipboard, 'writeText').mockRejectedValue(new Error('Clipboard denied'))
    render(<CopyButton text="ABC234" />)
    await user.click(screen.getByRole('button', { name: 'Copy room code' }))
    expect(screen.getByRole('status')).toHaveTextContent('Could not copy. Select and copy the text manually.')
  })

  it('preserves the report after failure and disables duplicate sends while pending', async () => {
    const user = userEvent.setup()
    let reject
    api.submitProblemReport.mockImplementationOnce(() => new Promise((_, fail) => { reject = fail }))
    render(<ReportProblem />)
    await user.click(screen.getByRole('button', { name: 'Report problem' }))
    await user.selectOptions(screen.getByLabelText('Where did it happen?'), 'wallet')
    await user.type(screen.getByLabelText('What happened?'), 'Coins missing')
    await user.click(screen.getByRole('button', { name: 'Send report' }))
    expect(screen.getByRole('button', { name: 'Sending…' })).toBeDisabled()
    reject(new Error('offline'))
    expect(await screen.findByRole('alert')).toHaveTextContent('Could not confirm submission.')
    expect(screen.getByLabelText('What happened?')).toHaveValue('Coins missing')
    expect(screen.getByLabelText('Where did it happen?')).toHaveValue('wallet')
    expect(screen.getByRole('button', { name: 'Send report' })).toBeEnabled()
  })

  it('sends only description, selected Spanish section and app version', async () => {
    const user = userEvent.setup()
    api.submitProblemReport.mockResolvedValue({ status: 'created' })
    render(<ReportProblem />)
    await user.click(screen.getByRole('button', { name: 'Report problem' }))
    expect(screen.getByRole('button', { name: 'Send report' })).toBeDisabled()
    await user.selectOptions(screen.getByLabelText('Where did it happen?'), 'join_room')
    expect(screen.getByRole('option', { name: 'Unirse a una sala' }).selected).toBe(true)
    await user.type(screen.getByLabelText('What happened?'), 'The room did not load.')
    expect(screen.queryByLabelText('Report text')).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Send report' }))
    expect(api.submitProblemReport).toHaveBeenCalledWith({ description: 'The room did not load.', section: 'join_room', app_version: '1.0.0' })
    expect(screen.getByRole('status')).toHaveTextContent('Report sent. Thank you.')
    await user.click(screen.getByRole('button', { name: 'Close report a problem' }))
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })
})
