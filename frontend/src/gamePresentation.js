// Display rules mirror the backend economy; snapshots remain authoritative for outcomes.
export const ROUND_STAKE = 50
export const WINNER_PAYOUT = 75

export function roundOutcome(state, role) {
  if (state.result === 'draw') return { kind: 'draw', title: 'Draw round', payout: ROUND_STAKE, net: 0 }
  if (!['host', 'guest'].includes(role) || !['player_one_wins', 'player_two_wins'].includes(state.result)) {
    return { kind: 'unknown', title: 'Round complete', payout: null, net: null }
  }
  const won = state.result === (role === 'host' ? 'player_one_wins' : 'player_two_wins')
  return {
    kind: won ? 'win' : 'loss',
    title: won ? 'You won' : 'You lost',
    payout: won ? WINNER_PAYOUT : 0,
    net: won ? WINNER_PAYOUT - ROUND_STAKE : -ROUND_STAKE,
  }
}
