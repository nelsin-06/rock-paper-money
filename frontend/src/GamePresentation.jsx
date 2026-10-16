import { useEffect, useId, useRef, useState } from 'react'
import blueCharacter from './assets/personaje-azul.png'
import redCharacter from './assets/personaje-rojo.png'
import { APP_VERSION } from './storage.js'
import { ROUND_STAKE, roundOutcome } from './gamePresentation.js'

export function GameIcon({ name, ...props }) {
  return (
    <svg className="game-icon" viewBox="0 0 64 64" aria-hidden="true" focusable="false" {...props}>
      {name === 'rock' && <>
        <path d="M10 44 8 28 20 12 40 8 56 22 58 42 46 56H22Z" fill="#f4ead2" stroke="#0b0812" strokeWidth="3" strokeLinejoin="round" />
        <path d="M20 12 27 27 40 8M27 27 47 33 56 22M27 27 22 56M47 33V56" stroke="#0b0812" strokeWidth="2" fill="none" opacity=".45" />
      </>}
      {name === 'paper' && <>
        <path d="M14 6h28l12 12v40H14Z" fill="#f4ead2" stroke="#0b0812" strokeWidth="3" strokeLinejoin="round" />
        <path d="M42 6v12h12" fill="#b9a0f2" stroke="#0b0812" strokeWidth="3" strokeLinejoin="round" />
        <path d="M22 32h24M22 40h24M22 48h15" stroke="#0b0812" strokeWidth="3" strokeLinecap="round" />
      </>}
      {name === 'scissors' && <>
        <path d="M24 40 44 6M40 40 20 6" stroke="#0b0812" strokeWidth="11" strokeLinecap="round" />
        <path d="M24 40 44 6M40 40 20 6" stroke="#f4ead2" strokeWidth="5" strokeLinecap="round" />
        <circle cx="21" cy="48" r="9" fill="#e5383b" stroke="#0b0812" strokeWidth="3" />
        <circle cx="43" cy="48" r="9" fill="#e5383b" stroke="#0b0812" strokeWidth="3" />
      </>}
      {name === 'coin' && <>
        <circle cx="32" cy="32" r="28" fill="#f6c33b" stroke="#0b0812" strokeWidth="3" />
        <circle cx="32" cy="32" r="21" fill="none" stroke="#c98f12" strokeWidth="3" />
        <text x="32" y="42" textAnchor="middle" fontFamily="Bungee, Impact, sans-serif" fontSize="28" fill="#0b0812">$</text>
      </>}
      {name === 'unknown' && <>
        <circle cx="32" cy="32" r="27" fill="#2b1450" stroke="#b9a0f2" strokeWidth="3" strokeDasharray="4 4" />
        <path d="M23 24a9 9 0 0 1 18 0c0 7-9 7-9 14" fill="none" stroke="#f4ead2" strokeWidth="4" strokeLinecap="round" />
        <circle cx="32" cy="46" r="2.5" fill="#f4ead2" />
      </>}
    </svg>
  )
}

export function GameLogo({ compact = false }) {
  return compact ? (
    <div className="brand" aria-label="Rock Paper Money">
      <span className="brand-icons"><GameIcon name="rock" /><GameIcon name="paper" /><GameIcon name="scissors" /></span>
      <span>RPM</span>
    </div>
  ) : (
    <svg className="game-logo" viewBox="0 0 400 400" role="img" aria-label="Rock Paper Money">
      <circle cx="200" cy="200" r="188" fill="#2b1450" stroke="#f4ead2" strokeWidth="8" />
      <circle cx="200" cy="200" r="168" fill="none" stroke="#f6c33b" strokeWidth="4" strokeDasharray="4 12" strokeLinecap="round" />
      <GameIcon className="" name="rock" x="52" y="72" width="120" height="120" />
      <GameIcon className="" name="paper" x="228" y="72" width="120" height="120" />
      <GameIcon className="" name="scissors" x="140" y="236" width="120" height="120" />
      <GameIcon className="" name="coin" x="146" y="146" width="108" height="108" />
    </svg>
  )
}

export function Character({ color = 'blue', className = '' }) {
  return <img className={`character character-${color} ${className}`} src={color === 'red' ? redCharacter : blueCharacter} alt="" draggable="false" />
}

export function Modal({ title, onClose, children }) {
  const dialogRef = useRef(null)
  const titleId = useId()
  useEffect(() => {
    const previousFocus = document.activeElement
    const dialog = dialogRef.current
    // Native dialogs provide focus containment and Escape handling in browsers.
    if (dialog.showModal) dialog.showModal()
    else dialog.setAttribute('open', '')
    return () => {
      dialog.close?.()
      previousFocus?.focus()
    }
  }, [])

  return (
    <dialog ref={dialogRef} className="game-dialog" aria-labelledby={titleId} onCancel={(event) => {
      event.preventDefault()
      onClose()
    }}>
      <div className="dialog-heading">
        <h2 id={titleId}>{title}</h2>
        <button className="icon-button" type="button" aria-label={`Close ${title.toLowerCase()}`} onClick={onClose}>×</button>
      </div>
      {children}
    </dialog>
  )
}

export function CopyButton({ text, label = 'Copy room code' }) {
  const [feedback, setFeedback] = useState('')
  async function copy() {
    try {
      await navigator.clipboard.writeText(text)
      setFeedback('Copied to clipboard.')
    } catch {
      setFeedback('Could not copy. Select and copy the text manually.')
    }
  }
  return <div className="copy-control">
    <button className="button button-secondary" type="button" onClick={copy}>{label}</button>
    {feedback && <p role="status" className="copy-feedback">{feedback}</p>}
  </div>
}

export function ReportProblem() {
  const [open, setOpen] = useState(false)
  const [issue, setIssue] = useState('')
  const report = `Rock Paper Money — local problem report\nApp version: ${APP_VERSION}\nScreen: Main menu\n\nIssue:\n${issue}`
  return <>
    <button className="button button-report" onClick={() => setOpen(true)}><GameIcon name="scissors" />Report problem</button>
    {open && <Modal title="Report a problem" onClose={() => setOpen(false)}>
      <p>This report stays in your browser. Nothing is sent or saved. Copy it to share through a support channel you already use.</p>
      <label htmlFor="report-issue">What happened?</label>
      <textarea id="report-issue" rows="4" value={issue} onChange={(event) => setIssue(event.target.value)} placeholder="Describe the issue. Do not include passwords or access tokens." />
      <label htmlFor="report-preview">Report text</label>
      <textarea id="report-preview" rows="6" value={report} readOnly />
      <CopyButton key={report} text={report} label="Copy report" />
    </Modal>}
  </>
}

export function RoundResult({ state, role }) {
  const outcome = roundOutcome(state, role)
  const moves = state.moves ?? []
  const moveFor = (playerRole) => moves.find((item) => item.role === playerRole)?.move
  const missingMoves = !moveFor('host') || !moveFor('guest')

  return (
    <section className="result-panel" aria-label="Round result" aria-live="polite">
      <p className="eyebrow">Round {state.round} complete{state.forfeit ? ' · Forfeit' : ''}</p>
      <h2>{outcome.title}</h2>
      {state.forfeit ? (
        <p>{outcome.kind === 'win' ? 'Your opponent disconnected and did not return.' : 'You did not reconnect before the grace period ended.'}</p>
      ) : missingMoves && <p>The round ended before both moves were submitted.</p>}
      <div className="revealed-moves">
        {[{ playerRole: role, label: 'Your move' }, { playerRole: role === 'host' ? 'guest' : 'host', label: 'Opponent' }].map(({ playerRole, label }) => {
          const move = moveFor(playerRole)
          const knownMove = ['rock', 'paper', 'scissors'].includes(move)
          return <div className="revealed-choice" role="group" aria-label={label} key={label}>
            <span>{label}</span>
            <GameIcon name={knownMove ? move : 'unknown'} />
            <strong>{knownMove ? move : 'No move revealed'}</strong>
          </div>
        })}
        <span className="versus" aria-hidden="true">VS</span>
      </div>
      {outcome.payout !== null && <>
        <div className="prize"><GameIcon name="coin" /><strong>{outcome.payout} coins {outcome.kind === 'draw' ? 'refunded' : 'payout'}</strong></div>
        <p className="net-result">{outcome.net > 0 ? '+' : ''}{outcome.net} coins net · {ROUND_STAKE}-coin stake</p>
      </>}
    </section>
  )
}
