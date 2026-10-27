import { useCallback, useEffect, useRef, useState } from 'react'
import * as api from './api.js'
import { connectToRoom } from './realtime.js'
import { Character, CopyButton, GameIcon, GameLogo, Modal, ReportProblem, RoundResult } from './GamePresentation.jsx'
import { ROUND_STAKE, WINNER_PAYOUT, roundOutcome } from './gamePresentation.js'
import { clearSession, loadSession, saveSession } from './storage.js'
import { supabase } from './supabase.js'

const MOVES = [
  { value: 'rock', label: 'Rock' },
  { value: 'paper', label: 'Paper' },
  { value: 'scissors', label: 'Scissors' },
]

function displayError(error) {
  return error instanceof Error ? error.message : 'Something went wrong. Please try again.'
}

const ECONOMY_CHANGED_EVENT = 'rock-paper-money:economy-changed'
export const ERROR_NOTICE_DURATION_MS = 5000

function signalEconomyChanged() {
  window.dispatchEvent(new Event(ECONOMY_CHANGED_EVENT))
}

export function ErrorNotice({ message, onDismiss, children }) {
  const dismissRef = useRef(onDismiss)

  useEffect(() => {
    dismissRef.current = onDismiss
  }, [onDismiss])

  useEffect(() => {
    if (!message) return undefined
    const timeout = window.setTimeout(() => dismissRef.current(), ERROR_NOTICE_DURATION_MS)
    return () => window.clearTimeout(timeout)
  }, [message])

  if (!message) return null
  return (
    <div className="notice notice-error" role="alert">
      <span>{message}</span>
      <div className="notice-actions">
        {children}
        <button className="notice-dismiss" type="button" aria-label="Dismiss error" onClick={onDismiss}>×</button>
      </div>
    </div>
  )
}

export function AccountPanel({ onBalance }) {
  const [balance, setBalance] = useState(null)
  const [amount, setAmount] = useState('')
  const [open, setOpen] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const pendingRechargeRef = useRef(null)

  const refresh = useCallback(async () => {
    try {
      const wallet = await api.getWallet()
      setBalance(wallet.balance)
      onBalance(wallet.balance)
      setError('')
    } catch (requestError) {
      setError(displayError(requestError))
    }
  }, [onBalance])

  useEffect(() => {
    const initialRefresh = window.setTimeout(refresh, 0)
    window.addEventListener(ECONOMY_CHANGED_EVENT, refresh)
    return () => {
      window.clearTimeout(initialRefresh)
      window.removeEventListener(ECONOMY_CHANGED_EVENT, refresh)
    }
  }, [refresh])

  async function recharge(event) {
    event.preventDefault()
    if (!/^[1-9]\d*$/.test(amount)) {
      setError('Enter a positive whole coin amount.')
      return
    }
    setBusy(true)
    setError('')
    if (pendingRechargeRef.current?.amount !== amount) {
      pendingRechargeRef.current = { amount, key: crypto.randomUUID() }
    }
    try {
      const wallet = await api.rechargeWallet(amount, pendingRechargeRef.current.key)
      setBalance(wallet.balance)
      onBalance(wallet.balance)
      setAmount('')
      pendingRechargeRef.current = null
    } catch (requestError) {
      setError(displayError(requestError))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="wallet-control">
      <button className="wallet-button" aria-label={`Coin wallet: ${balance ?? 'loading'} coins. Recharge`} onClick={() => setOpen(true)}>
        <GameIcon name="coin" /><span>{balance ?? '…'} coins</span><span aria-hidden="true">+</span>
      </button>
      {!open && error && <ErrorNotice message={error} onDismiss={() => setError('')}>
        <button type="button" onClick={refresh}>Retry wallet</button>
      </ErrorNotice>}
      {open && <Modal title="Coin wallet" onClose={() => setOpen(false)}>
        <div className="wallet-card">
          <p className="wallet-balance" aria-live="polite"><GameIcon name="coin" /><strong>{balance ?? '…'} coins</strong></p>
          <p>Each round costs {ROUND_STAKE} coins. Creating a room does not charge coins; both players fund it when an opponent joins.</p>
          <form className="recharge-form" onSubmit={recharge}>
            <label htmlFor="recharge-amount">Recharge amount</label>
            <input id="recharge-amount" inputMode="numeric" pattern="[1-9][0-9]*" value={amount} onChange={(event) => {
              setAmount(event.target.value.replace(/\D/g, ''))
              pendingRechargeRef.current = null
            }} placeholder="100" />
            <button className="button button-secondary" disabled={busy || !amount}>{busy ? 'Recharging…' : 'Add coins'}</button>
          </form>
          <p className="wallet-update" role="status">{busy ? 'Adding coins…' : 'Balance updates after funding and completed rounds.'}</p>
          <ErrorNotice message={error} onDismiss={() => setError('')} />
        </div>
      </Modal>}
    </div>
  )
}

function Home({ onEnterRoom, walletBalance }) {
  const [code, setCode] = useState('')
  const [busy, setBusy] = useState(null)
  const [error, setError] = useState('')
  const [joinOpen, setJoinOpen] = useState(false)
  const entryLockRef = useRef(false)

  async function enter(action, role) {
    if (entryLockRef.current) return
    entryLockRef.current = true
    setBusy(role)
    setError('')
    try {
      const session = await action()
      signalEconomyChanged()
      saveSession(session)
      onEnterRoom(session)
    } catch (requestError) {
      entryLockRef.current = false
      setError(displayError(requestError))
      setBusy(null)
    }
  }

  function handleJoin(event) {
    event.preventDefault()
    if (code.length !== 6) {
      setError('Enter the six-character room code.')
      return
    }
    enter(() => api.joinRoom(code), 'guest')
  }

  const insufficientForRound = walletBalance !== null && BigInt(walletBalance) < BigInt(ROUND_STAKE)

  return (
    <main className="page home-page">
      <Character className="menu-character menu-character-left" />
      <Character color="red" className="menu-character menu-character-right" />
      <section className="hero" aria-labelledby="home-title">
        <h1 id="home-title"><span>Rock,</span><span>Paper,</span><em>Money</em></h1>
        <p className="hero-copy">Two players. One choice. Make it count.</p>
      </section>

      <section className="menu-actions" aria-label="Enter a game">
        <button className="button button-primary" disabled={busy !== null || insufficientForRound} onClick={() => enter(api.createRoom, 'host')}>
          <GameIcon name="rock" />
          {busy === 'host' ? 'Creating room…' : 'Create room'}
        </button>
        <button className="button button-secondary" disabled={busy !== null || insufficientForRound} onClick={() => setJoinOpen(true)}><GameIcon name="paper" />Join room</button>
        <ReportProblem />
        <p className="entry-rules">{ROUND_STAKE} coins per player · Winner receives {WINNER_PAYOUT} coins</p>
        {insufficientForRound && <div className="notice notice-error" role="status">You need at least 50 coins to create or join a room. Use your wallet to recharge.</div>}
        {!joinOpen && <ErrorNotice message={error} onDismiss={() => setError('')} />}
      </section>
      {joinOpen && <Modal title="Join a room" onClose={() => setJoinOpen(false)}>
        <p>Get the room code from your opponent. Both players contribute {ROUND_STAKE} coins when you join.</p>
        <form className="join-form" onSubmit={handleJoin}>
          <label htmlFor="room-code">Six-character room code</label>
          <input
            id="room-code"
            value={code}
            onChange={(event) => setCode(event.target.value.replace(/[^a-z0-9]/gi, '').toUpperCase().slice(0, 6))}
            placeholder="ABC234"
            autoComplete="off"
            autoCapitalize="characters"
            spellCheck="false"
            inputMode="text"
          />
          <button className="button button-secondary" disabled={busy !== null || code.length !== 6 || insufficientForRound}>
            {busy === 'guest' ? 'Joining room…' : 'Join room'}
          </button>
        </form>
        <ErrorNotice message={error} onDismiss={() => setError('')} />
      </Modal>}
    </main>
  )
}

function SessionRecovery({ error, onRetry, onForget }) {
  return (
    <main className="page recovery-page">
      <section className="entry-card centered" aria-live="polite">
        <GameLogo />
        {error ? (
          <>
            <p className="eyebrow">Session unavailable</p>
            <h1>We could not restore your room</h1>
            <p>{error}</p>
            <div className="recovery-actions">
              <button className="button button-primary" onClick={onRetry}>Retry</button>
              <button className="button button-quiet" onClick={onForget}>Forget this session</button>
            </div>
          </>
        ) : (
          <>
            <div className="loader" />
            <h1>Getting your game ready…</h1>
            <p>Checking your account session.</p>
          </>
        )}
      </section>
    </main>
  )
}

function Scoreboard({ players = [], role, resolved = false, presence = {} }) {
  return (
    <section className="scoreboard" aria-label="Score and readiness">
      {['host', 'guest'].map((playerRole) => {
        const player = players.find((candidate) => candidate.role === playerRole)
        return (
          <div className={`player-card ${playerRole === role ? 'player-card-you' : ''}`} key={playerRole}>
            <div>
              <p>{playerRole === role ? 'You' : 'Opponent'}</p>
              <span className={`status-dot ${(resolved ? player?.wantsNextRound : player?.submitted) ? 'status-ready' : ''}`}>
                {resolved
                  ? player?.wantsNextRound ? 'Wants another round' : 'Deciding'
                  : player?.submitted ? 'Move locked' : player ? 'Choosing' : 'Not connected'}
              </span>
              {player && presence[playerRole] === false && <span className="presence-status">Reconnecting</span>}
            </div>
            <strong aria-label={`${playerRole} score`}>{player?.wins ?? 0}</strong>
          </div>
        )
      })}
    </section>
  )
}

function Room({ session, onLeave, walletBalance }) {
  const [state, setState] = useState(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [action, setAction] = useState(null)
  const [movePending, setMovePending] = useState(false)
  const [connectionAttempt, setConnectionAttempt] = useState(0)
  const [connectionError, setConnectionError] = useState('')
  const actionLockRef = useRef(false)
  const mountedRef = useRef(true)
  const previousEconomyStateRef = useRef(null)

  useEffect(() => {
    mountedRef.current = true
    return () => {
      mountedRef.current = false
    }
  }, [])

  useEffect(() => {
    let active = true

    const close = connectToRoom(session.roomCode, {
      onOpen: () => {
        if (active) setConnectionError('')
      },
      onState: (nextState) => {
        if (!active) return
        if (nextState.closed) {
          onLeave()
          return
        }
        setState(nextState)
        const previous = previousEconomyStateRef.current
        if (previous && ((!previous.ready && nextState.ready) || (!previous.resolved && nextState.resolved) || previous.round !== nextState.round)) {
          signalEconomyChanged()
        }
        previousEconomyStateRef.current = nextState
        const serverPlayer = nextState.players.find((player) => player.role === nextState.playerRole)
        if (serverPlayer?.submitted) setMovePending(false)
        setConnectionError('')
        setLoading(false)
      },
      onError: (streamError) => {
        if (!active) return
        setConnectionError(displayError(streamError))
        setLoading(false)
      },
    })

    return () => {
      active = false
      close()
    }
  }, [session.roomCode, connectionAttempt, onLeave])

  async function performAction(name, request) {
    if (actionLockRef.current) return
    actionLockRef.current = true
    if (name === 'move') setMovePending(true)
    setAction(name)
    setError('')
    try {
      await request()
    } catch (requestError) {
      if (mountedRef.current) {
        if (name === 'move') setMovePending(false)
        setError(displayError(requestError))
      }
    } finally {
      actionLockRef.current = false
      if (mountedRef.current) setAction(null)
    }
  }

  const role = state?.playerRole
  const currentPlayer = state?.players.find((player) => player.role === role)
  const hasSubmitted = (currentPlayer?.submitted ?? false) || movePending
  const opponent = state?.players.find((player) => player.role !== role)
  const wantsNextRound = currentPlayer?.wantsNextRound ?? false
  const canLeave = state?.resolved || (state?.ready === false && state.players.length === 1)
  const insufficientForRound = walletBalance !== null && BigInt(walletBalance) < BigInt(ROUND_STAKE)
  const outcome = state?.resolved ? roundOutcome(state, role) : null

  function leave() {
    performAction('leave', async () => {
      await api.leaveRoom(session.roomCode)
      if (mountedRef.current) onLeave()
    })
  }

  function retryConnection() {
    setLoading(true)
    setConnectionError('')
    setConnectionAttempt((attempt) => attempt + 1)
  }

  return (
    <main className={`page room-page${outcome ? ` result-page result-${outcome.kind}` : ''}`}>
      <header className="room-header">
        <div>
          <p className="eyebrow">Room code</p>
          <h1 aria-label={`Room code ${session.roomCode}`}>{session.roomCode}</h1>
          <p className="share-hint">{state?.resolved ? `Round ${state.round} settled` : 'Keep this room open while you play.'}</p>
        </div>
        {!state?.resolved && <button
          className="button button-quiet"
          disabled={action !== null || !canLeave}
          onClick={leave}
        >
          {action === 'leave' ? 'Leaving room…' : 'Leave room'}
        </button>}
      </header>

      {state && <Scoreboard players={state.players} role={role} resolved={state.resolved} presence={state.presence} />}

      <ErrorNotice
        message={error || connectionError}
        onDismiss={() => error ? setError('') : setConnectionError('')}
      >
        {connectionError && <button type="button" onClick={retryConnection}>Retry</button>}
      </ErrorNotice>

      {loading && <p className="connection-status" role="status">{state ? 'Reconnecting to your room…' : 'Connecting to your room…'}</p>}

      {!state?.resolved && <div className="arena">
        <div className="arena-panes">
          <section className="fighter-pane opponent-pane" aria-label="Opponent arena">
            <div className="fighter-copy">
              <span className="fighter-tag opponent-tag">Opponent</span>
              <p>{!state?.ready ? 'A challenger awaits' : opponent?.submitted ? 'Their move is locked' : 'Choosing in secret'}</p>
            </div>
            <Character color="red" />
          </section>
          <div className="arena-hud" aria-label="Round details">
            <span>{state ? `Round ${state.round}` : 'Connecting'}</span>
            <span className="pot"><GameIcon name="coin" />{state?.ready ? `${ROUND_STAKE * 2} coins · Pot` : 'Awaiting funding'}</span>
            <span>{hasSubmitted ? 'Locked' : state?.ready ? 'Choose' : 'Waiting'}</span>
          </div>
          <section className="fighter-pane local-pane" aria-label="Your arena">
            <Character />
            <div className="local-controls">
              <span className="fighter-tag local-tag">Your side</span>
              {state?.ready && <>
                <h2 id="round-title">{hasSubmitted ? 'Move submitted' : 'Choose your move'}</h2>
                <p aria-live="polite">{hasSubmitted ? 'Your choice is hidden. Waiting for your opponent.' : 'Your move stays secret until both players submit.'}</p>
                <div className="move-grid" aria-label="Choose a move">
                  {MOVES.map((move) => (
                    <button
                      className={`move-button move-${move.value}`}
                      key={move.value}
                      disabled={hasSubmitted || action !== null}
                      onClick={() => performAction('move', () => api.submitMove(session.roomCode, move.value))}
                    >
                      <GameIcon name={move.value} />
                      <span>{move.label}</span>
                    </button>
                  ))}
                </div>
                {action === 'move' && <p className="action-status" role="status">Locking your move…</p>}
              </>}
            </div>
          </section>
        </div>
        {(!state || !state.ready) && <div className="waiting-overlay">
          <section className="waiting-card" aria-live="polite">
            <div className="loader" />
            <p className="eyebrow">{state ? 'Room is open' : 'Joining the arena'}</p>
            <h2>{state ? 'Waiting for a guest' : loading ? 'Loading room…' : 'Room connection unavailable'}</h2>
            {state ? <>
              <p>Share your room code with an opponent.</p>
              <p className="room-code">{session.roomCode}</p>
              <CopyButton text={session.roomCode} />
              <p>Keep this page open. Each player contributes {ROUND_STAKE} coins when the round starts.</p>
              {insufficientForRound && <div className="notice notice-error">Recharge to at least 50 coins before a guest can fund this round.</div>}
            </> : <p>Waiting for the server to confirm your room.</p>}
          </section>
        </div>}
      </div>}

      {state?.resolved && (
        <section className="game-panel result-stage">
          <Character className="result-character" />
          <div className="result-content">
            <RoundResult state={state} role={role} />
            <p aria-live="polite">
              {wantsNextRound
                ? 'You want another round. Waiting for your opponent to decide.'
                : opponent?.wantsNextRound
                  ? 'Your opponent wants another round. Accept or leave the room.'
                  : 'Request another round, or leave the room.'}
            </p>
            <div className="result-actions">
              <button
                className="button button-primary"
                disabled={action !== null || wantsNextRound || insufficientForRound}
                onClick={() => performAction('next', () => api.startNextRound(session.roomCode, state.round))}
              >
                {action === 'next'
                  ? 'Sending request…'
                  : wantsNextRound
                    ? 'Waiting for opponent…'
                    : opponent?.wantsNextRound ? 'Accept next round' : 'Play again'}
              </button>
              <button className="button button-cream" disabled={action !== null || !canLeave} onClick={leave}>
                {action === 'leave' ? 'Leaving room…' : 'Menu'}
              </button>
            </div>
            <p className="rematch-hint">A new round starts only when both players agree and have {ROUND_STAKE} coins each.</p>
            {insufficientForRound && <div className="notice notice-error">Recharge to at least 50 coins before accepting another paid round.</div>}
          </div>
        </section>
      )}

      <p className="leave-note">
        {state?.resolved
          ? 'Leaving closes this room for both players.'
          : state?.ready === false && state.players.length === 1
            ? 'You can leave while waiting for an opponent.'
            : 'You can leave after the round is finished.'}
      </p>
    </main>
  )
}

export function GameApp({ walletBalance = null }) {
  const [session, setSession] = useState(() => loadSession())

  const leaveRoom = useCallback(() => {
    clearSession()
    setSession(null)
  }, [])

  function enterRoom(nextSession) {
    setSession(nextSession)
  }

  return session ? (
    <Room key={session.roomCode} session={session} onLeave={leaveRoom} walletBalance={walletBalance} />
  ) : (
    <Home onEnterRoom={enterRoom} walletBalance={walletBalance} />
  )
}

function AuthForm({ initialError, onAuthenticated, onPending, onDismissInitialError }) {
  const [mode, setMode] = useState('login')
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  async function submit(event) {
    event.preventDefault()
    setBusy(true)
    setError('')
    try {
      const result = mode === 'register'
        ? await supabase.auth.signUp({ email, password })
        : await supabase.auth.signInWithPassword({ email, password })
      if (result.error) throw result.error
      if (mode === 'register' && result.data.user && !result.data.session) {
        onPending(email)
      } else if (result.data.session) {
        onAuthenticated(result.data.session)
      }
    } catch (authError) {
      setError(displayError(authError))
    } finally {
      setBusy(false)
    }
  }

  return (
    <main className="page recovery-page">
      <section className="entry-card auth-card" aria-labelledby="auth-title">
        <GameLogo />
        <p className="eyebrow">Account required</p>
        <h1 id="auth-title">{mode === 'login' ? 'Sign in to play' : 'Create your account'}</h1>
        <form className="auth-form" onSubmit={submit}>
          <label htmlFor="auth-email">Email address</label>
          <input id="auth-email" type="email" autoComplete="email" required value={email} onChange={(event) => setEmail(event.target.value)} />
          <label htmlFor="auth-password">Password</label>
          <input id="auth-password" type="password" autoComplete={mode === 'login' ? 'current-password' : 'new-password'} minLength="6" required value={password} onChange={(event) => setPassword(event.target.value)} />
          <button className="button button-primary" disabled={busy}>
            {busy ? 'Please wait…' : mode === 'login' ? 'Sign in' : 'Register'}
          </button>
        </form>
        <button className="button button-quiet auth-switch" type="button" disabled={busy} onClick={() => {
          setMode((current) => current === 'login' ? 'register' : 'login')
          setError('')
        }}>
          {mode === 'login' ? 'Need an account? Register' : 'Already have an account? Sign in'}
        </button>
        <ErrorNotice
          message={error || initialError}
          onDismiss={() => error ? setError('') : onDismissInitialError()}
        />
      </section>
    </main>
  )
}

export default function App() {
  const [authSession, setAuthSession] = useState(null)
  const [loading, setLoading] = useState(Boolean(supabase))
  const [pendingEmail, setPendingEmail] = useState('')
  const [error, setError] = useState(supabase ? '' : 'Supabase authentication is not configured.')
  const [walletBalance, setWalletBalance] = useState(null)
  const backendTokenRef = useRef('')
  const backendSessionTaskRef = useRef(Promise.resolve())
  const authSequenceRef = useRef(0)

  const establishSession = useCallback((nextSession) => {
    const sequence = ++authSequenceRef.current
    if (!nextSession) {
      backendTokenRef.current = ''
      api.clearSessionSecurity()
    }
    if (backendTokenRef.current !== nextSession?.access_token) setAuthSession(null)
    const task = backendSessionTaskRef.current.then(async () => {
      if (sequence !== authSequenceRef.current) return
      if (!nextSession) {
        api.clearSessionSecurity()
        backendTokenRef.current = ''
        if (sequence === authSequenceRef.current) {
          clearSession()
          setAuthSession(null)
        }
        return
      }
      if (backendTokenRef.current !== nextSession.access_token) {
        backendTokenRef.current = ''
        await api.bootstrapSession(nextSession.access_token)
        // A superseded bootstrap cannot certify the latest security context.
        if (sequence !== authSequenceRef.current) return
        backendTokenRef.current = nextSession.access_token
      }
      if (sequence === authSequenceRef.current) setAuthSession(nextSession)
    })
    backendSessionTaskRef.current = task.catch(() => {})
    return task
  }, [])

  useEffect(() => {
    if (!supabase) {
      return undefined
    }
    let active = true
    let authStateObserved = false
    supabase.auth.getSession().then(async ({ data, error: sessionError }) => {
      if (!active || authStateObserved) return
      try {
        if (sessionError) throw sessionError
        await establishSession(data.session ?? null)
      } catch (sessionFailure) {
        if (active) setError(displayError(sessionFailure))
      }
      setLoading(false)
    })
    const { data: { subscription } } = supabase.auth.onAuthStateChange((_event, nextSession) => {
      if (!active) return
      authStateObserved = true
      setPendingEmail('')
      setError('')
      establishSession(nextSession).catch((sessionFailure) => {
        if (active) setError(displayError(sessionFailure))
      }).finally(() => {
        if (active) setLoading(false)
      })
    })
    return () => {
      active = false
      subscription.unsubscribe()
    }
  }, [establishSession])

  async function signOut() {
    setError('')
    try {
      await backendSessionTaskRef.current
      await api.logoutSession()
      const { error: signOutError } = await supabase.auth.signOut()
      if (signOutError) {
        setError(displayError(signOutError))
        return
      }
      clearSession()
      backendTokenRef.current = ''
      setAuthSession(null)
    } catch (signOutError) {
      setError(displayError(signOutError))
    }
  }

  if (loading) return <SessionRecovery error="" onRetry={() => {}} onForget={() => {}} />
  if (error && !supabase) {
    return <main className="page recovery-page"><section className="entry-card centered"><GameLogo /><h1>Game unavailable</h1><div className="notice notice-error" role="alert">{error}</div><p>Authentication must be configured before you can play.</p></section></main>
  }
  if (pendingEmail) {
    return (
      <main className="page recovery-page">
        <section className="entry-card centered" aria-live="polite">
          <GameLogo />
          <p className="eyebrow">Confirm your email</p>
          <h1>Check your inbox</h1>
          <p>We sent a confirmation link to {pendingEmail}. Confirm your email, then return here to sign in.</p>
          <button className="button button-quiet" onClick={() => setPendingEmail('')}>Back to sign in</button>
        </section>
      </main>
    )
  }
  if (!authSession) return <AuthForm initialError={error} onAuthenticated={(session) => establishSession(session).catch((sessionFailure) => setError(displayError(sessionFailure)))} onPending={setPendingEmail} onDismissInitialError={() => setError('')} />

  return (
    <div className="authenticated-app">
      <header className="account-bar">
        <GameLogo compact />
        <div className="player-account">
          <span className="player-identity" title={authSession.user?.email}>
            {authSession.user?.user_metadata?.username || authSession.user?.user_metadata?.display_name || authSession.user?.email || 'Player'}
          </span>
          <AccountPanel onBalance={setWalletBalance} />
          <button className="button button-quiet" onClick={signOut}>Sign out</button>
        </div>
      </header>
      <div className="page account-error">
        <ErrorNotice message={error} onDismiss={() => setError('')} />
      </div>
      <GameApp walletBalance={walletBalance} />
    </div>
  )
}
