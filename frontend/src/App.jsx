import { useEffect, useRef, useState } from 'react'
import * as api from './api.js'
import { clearSession, loadSession, saveSession } from './storage.js'
import { supabase } from './supabase.js'

const MOVES = [
  { value: 'rock', label: 'Rock', symbol: '●' },
  { value: 'paper', label: 'Paper', symbol: '▰' },
  { value: 'scissors', label: 'Scissors', symbol: '✦' },
]

function displayError(error) {
  return error instanceof Error ? error.message : 'Something went wrong. Please try again.'
}

function Home({ onEnterRoom }) {
  const [code, setCode] = useState('')
  const [busy, setBusy] = useState(null)
  const [error, setError] = useState('')
  const entryLockRef = useRef(false)

  async function enter(action, role) {
    if (entryLockRef.current) return
    entryLockRef.current = true
    setBusy(role)
    setError('')
    try {
      const session = await action()
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

  return (
    <main className="page home-page">
      <section className="hero" aria-labelledby="home-title">
        <p className="eyebrow">Two players · One room</p>
        <h1 id="home-title">Rock. Paper. <span>Money.</span></h1>
        <p className="hero-copy">Open a room, share the code, and settle it in three moves.</p>
      </section>

      <section className="entry-card" aria-label="Enter a game">
        <button className="button button-primary" disabled={busy !== null} onClick={() => enter(api.createRoom, 'host')}>
          {busy === 'host' ? 'Creating room…' : 'Create room'}
        </button>
        <div className="divider"><span>or join</span></div>
        <form onSubmit={handleJoin}>
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
          <button className="button button-secondary" disabled={busy !== null || code.length !== 6}>
            {busy === 'guest' ? 'Joining room…' : 'Join room'}
          </button>
        </form>
        {error && <div className="notice notice-error" role="alert">{error}</div>}
      </section>
    </main>
  )
}

function SessionRecovery({ error, onRetry, onForget }) {
  return (
    <main className="page recovery-page">
      <section className="entry-card centered" aria-live="polite">
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
            <h1>Restoring your room…</h1>
            <p>Checking your saved player credentials.</p>
          </>
        )}
      </section>
    </main>
  )
}

function Scoreboard({ players = [], role, resolved = false }) {
  return (
    <section className="scoreboard" aria-label="Score and readiness">
      {['host', 'guest'].map((playerRole) => {
        const player = players.find((candidate) => candidate.role === playerRole)
        return (
          <div className={`player-card ${playerRole === role ? 'player-card-you' : ''}`} key={playerRole}>
            <div>
              <p>{playerRole === 'host' ? 'Host' : 'Guest'} {playerRole === role && <span className="you">You</span>}</p>
              <span className={`status-dot ${(resolved ? player?.wantsNextRound : player?.submitted) ? 'status-ready' : ''}`}>
                {resolved
                  ? player?.wantsNextRound ? 'Wants another round' : 'Deciding'
                  : player?.submitted ? 'Move locked' : player ? 'Choosing' : 'Not connected'}
              </span>
            </div>
            <strong aria-label={`${playerRole} score`}>{player?.wins ?? 0}</strong>
          </div>
        )
      })}
    </section>
  )
}

function RoundResult({ state, role }) {
  const localWon = (state.result === 'player_one_wins' && role === 'host') ||
    (state.result === 'player_two_wins' && role === 'guest')
  const title = state.result === 'draw' ? 'Draw round' : localWon ? 'You won' : 'You lost'
  const moveFor = (playerRole) => state.moves.find((item) => item.role === playerRole)?.move ?? 'unknown'

  return (
    <section className="result-panel" aria-live="polite">
      <p className="eyebrow">Round complete</p>
      <h2>{title}</h2>
      {state.forfeit ? (
        <p>{localWon ? 'Your opponent disconnected and did not return.' : 'You did not reconnect before the grace period ended.'}</p>
      ) : (
        <div className="revealed-moves">
          <p><span>Your move</span><strong>{moveFor(role)}</strong></p>
          <p><span>Opponent</span><strong>{moveFor(role === 'host' ? 'guest' : 'host')}</strong></p>
        </div>
      )}
    </section>
  )
}

function Room({ session, onLeave }) {
  const [state, setState] = useState(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [action, setAction] = useState(null)
  const [movePending, setMovePending] = useState(false)
  const [connectionAttempt, setConnectionAttempt] = useState(0)
  const [connectionError, setConnectionError] = useState('')
  const actionLockRef = useRef(false)
  const mountedRef = useRef(true)

  useEffect(() => {
    mountedRef.current = true
    return () => {
      mountedRef.current = false
    }
  }, [])

  useEffect(() => {
    let active = true

    const close = api.subscribeToRoom(session.roomCode, {
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
        const serverPlayer = nextState.players.find((player) => player.role === session.role)
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
  }, [session.roomCode, session.role, connectionAttempt, onLeave])

  useEffect(() => {
    let active = true
    let presenceController = null
    const refresh = async () => {
      if (presenceController) return
      const requestController = new AbortController()
      presenceController = requestController
      try {
        await api.refreshPresence(session.roomCode, session.playerToken, { signal: requestController.signal })
      } catch (presenceError) {
        if (active && !requestController.signal.aborted) setConnectionError(displayError(presenceError))
      } finally {
        if (presenceController === requestController) presenceController = null
      }
    }
    refresh()
    const heartbeat = window.setInterval(refresh, api.PRESENCE_HEARTBEAT_MS)
    return () => {
      active = false
      window.clearInterval(heartbeat)
      presenceController?.abort()
    }
  }, [session.roomCode, session.playerToken])

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

  const currentPlayer = state?.players.find((player) => player.role === session.role)
  const hasSubmitted = (currentPlayer?.submitted ?? false) || movePending
  const opponent = state?.players.find((player) => player.role !== session.role)
  const wantsNextRound = currentPlayer?.wantsNextRound ?? false

  function retryConnection() {
    setLoading(true)
    setConnectionError('')
    setConnectionAttempt((attempt) => attempt + 1)
  }

  return (
    <main className="page room-page">
      <header className="room-header">
        <div>
          <p className="eyebrow">Room code</p>
          <h1 aria-label={`Room code ${session.roomCode}`}>{session.roomCode}</h1>
          <p className="share-hint">Share this code with your opponent.</p>
        </div>
        <button
          className="button button-quiet"
          disabled={action !== null || !state?.resolved}
          onClick={() => performAction('leave', async () => {
            await api.leaveRoom(session.roomCode, session.playerToken)
            if (mountedRef.current) onLeave()
          })}
        >
          {action === 'leave' ? 'Leaving room…' : 'Leave room'}
        </button>
      </header>

      <Scoreboard players={state?.players} role={session.role} resolved={state?.resolved} />

      {(error || connectionError) && (
        <div className="notice notice-error" role="alert">
          <span>{error || connectionError}</span>
          {connectionError && <button onClick={retryConnection}>Retry</button>}
        </div>
      )}

      {loading && !state && <section className="game-panel centered" aria-live="polite"><div className="loader" /><h2>Loading room…</h2></section>}

      {state && !state.ready && (
        <section className="game-panel centered" aria-live="polite">
          <div className="waiting-mark"><span /><span /><span /></div>
          <p className="eyebrow">Room is open</p>
          <h2>Waiting for a guest</h2>
          <p>Keep this page open while your opponent joins with the code above.</p>
        </section>
      )}

      {state?.ready && !state.resolved && (
        <section className="game-panel" aria-labelledby="round-title">
          <p className="eyebrow">Current round</p>
          <h2 id="round-title">{hasSubmitted ? 'Move submitted' : 'Choose your move'}</h2>
          <p>{hasSubmitted ? 'Your choice is hidden. Waiting for your opponent.' : 'Your move stays secret until both players submit.'}</p>
          <div className="move-grid" aria-label="Choose a move">
            {MOVES.map((move) => (
              <button
                className="move-button"
                key={move.value}
                disabled={hasSubmitted || action !== null}
                onClick={() => performAction('move', () => api.submitMove(session.roomCode, session.playerToken, move.value))}
              >
                <span aria-hidden="true">{move.symbol}</span>
                {move.label}
              </button>
            ))}
          </div>
          {action === 'move' && <p className="action-status" role="status">Locking your move…</p>}
        </section>
      )}

      {state?.resolved && (
        <section className="game-panel">
          <RoundResult state={state} role={session.role} />
          <p aria-live="polite">
            {wantsNextRound
              ? 'You want another round. Waiting for your opponent to decide.'
              : opponent?.wantsNextRound
                ? 'Your opponent wants another round. Accept or leave the room.'
                : 'Request another round, or leave the room.'}
          </p>
          <button
            className="button button-primary"
            disabled={action !== null || wantsNextRound}
            onClick={() => performAction('next', () => api.startNextRound(session.roomCode, session.playerToken, state.round))}
          >
            {action === 'next'
              ? 'Sending request…'
              : wantsNextRound
                ? 'Waiting for opponent…'
                : opponent?.wantsNextRound ? 'Accept next round' : 'Request next round'}
          </button>
        </section>
      )}

      <p className="leave-note">
        {state?.resolved ? 'Leaving closes this room for both players.' : 'You can leave after the round is finished.'}
      </p>
    </main>
  )
}

export function GameApp() {
  const [session, setSession] = useState(() => loadSession())
  const [sessionReady, setSessionReady] = useState(() => !session)
  const [validationError, setValidationError] = useState('')
  const [validationAttempt, setValidationAttempt] = useState(0)

  useEffect(() => {
    if (!session || sessionReady) return undefined

    const controller = new AbortController()
    let active = true

    api.validateSession(session.roomCode, session.playerToken, session.role, { signal: controller.signal })
      .then(() => {
        if (active) setSessionReady(true)
      })
      .catch((requestError) => {
        if (!active || controller.signal.aborted) return
        if (requestError?.status === 401 || requestError?.status === 404) {
          clearSession()
          setSession(null)
          return
        }
        setValidationError(displayError(requestError))
      })

    return () => {
      active = false
      controller.abort()
    }
  }, [session, sessionReady, validationAttempt])

  function leaveRoom() {
    clearSession()
    setSession(null)
    setSessionReady(false)
    setValidationError('')
  }

  function enterRoom(nextSession) {
    setSession(nextSession)
    setSessionReady(true)
    setValidationError('')
  }

  function retryValidation() {
    setValidationError('')
    setValidationAttempt((attempt) => attempt + 1)
  }

  if (session && !sessionReady) {
    return <SessionRecovery error={validationError} onRetry={retryValidation} onForget={leaveRoom} />
  }

  return session ? (
    <Room key={`${session.roomCode}:${session.playerToken}`} session={session} onLeave={leaveRoom} />
  ) : (
    <Home onEnterRoom={enterRoom} />
  )
}

function AuthForm({ initialError, onAuthenticated, onPending }) {
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
        {(error || initialError) && <div className="notice notice-error" role="alert">{error || initialError}</div>}
      </section>
    </main>
  )
}

export default function App() {
  const [authSession, setAuthSession] = useState(null)
  const [loading, setLoading] = useState(Boolean(supabase))
  const [pendingEmail, setPendingEmail] = useState('')
  const [error, setError] = useState(supabase ? '' : 'Supabase authentication is not configured.')

  useEffect(() => {
    if (!supabase) {
      return undefined
    }
    let active = true
    let authStateObserved = false
    supabase.auth.getSession().then(({ data, error: sessionError }) => {
      if (!active || authStateObserved) return
      if (sessionError) setError(displayError(sessionError))
      setAuthSession(data.session ?? null)
      if (!data.session) clearSession()
      setLoading(false)
    })
    const { data: { subscription } } = supabase.auth.onAuthStateChange((_event, nextSession) => {
      if (!active) return
      authStateObserved = true
      setAuthSession(nextSession)
      setPendingEmail('')
      setError('')
      if (!nextSession) clearSession()
      setLoading(false)
    })
    return () => {
      active = false
      subscription.unsubscribe()
    }
  }, [])

  async function signOut() {
    setError('')
    try {
      const { error: signOutError } = await supabase.auth.signOut()
      if (signOutError) {
        setError(displayError(signOutError))
        return
      }
      clearSession()
      setAuthSession(null)
    } catch (signOutError) {
      setError(displayError(signOutError))
    }
  }

  if (loading) return <SessionRecovery error="" onRetry={() => {}} onForget={() => {}} />
  if (error && !supabase) {
    return <main className="page recovery-page"><div className="notice notice-error" role="alert">{error}</div></main>
  }
  if (pendingEmail) {
    return (
      <main className="page recovery-page">
        <section className="entry-card centered" aria-live="polite">
          <p className="eyebrow">Confirm your email</p>
          <h1>Check your inbox</h1>
          <p>We sent a confirmation link to {pendingEmail}. Confirm your email, then return here to sign in.</p>
          <button className="button button-quiet" onClick={() => setPendingEmail('')}>Back to sign in</button>
        </section>
      </main>
    )
  }
  if (!authSession) return <AuthForm initialError={error} onAuthenticated={setAuthSession} onPending={setPendingEmail} />

  return (
    <div className="authenticated-app">
      <div className="account-bar">
        <span>Signed in</span>
        <button className="button button-quiet" onClick={signOut}>Sign out</button>
      </div>
      {error && <div className="page notice notice-error" role="alert">{error}</div>}
      <GameApp />
    </div>
  )
}
