import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { ApiError } from './api.js'

// Shared primitives. Everything an operator reads about a failure — the
// server's own message, what the status means, and the request id to quote —
// is assembled in one place so no view can accidentally swallow part of it.

export function Banner({ problem, onDismiss }) {
  if (!problem) return null
  const err = problem.error
  const status = err instanceof ApiError ? err.status : null
  const explanation = err instanceof ApiError ? err.explanation : ''
  const tone = status === 409 ? 'warn' : 'err'

  return (
    <div className={`banner ${tone}`} role="alert">
      <div className="banner-head">
        <span className="status-chip">{status ? `HTTP ${status}` : 'failed'}</span>
        <strong>{problem.action}</strong>
        {onDismiss && <button className="ghost close" onClick={onDismiss} aria-label="Dismiss">×</button>}
      </div>
      <p className="banner-msg">{err?.message}</p>
      {explanation && <p className="banner-why">{explanation}</p>}
      {err?.requestId && <p className="banner-id">request id {err.requestId}</p>}
    </div>
  )
}

export function Notice({ text }) {
  if (!text) return null
  return <div className="banner ok" role="status"><p className="banner-msg">{text}</p></div>
}

export function Loading({ what = 'rows' }) {
  return <div className="state" role="status"><span className="spinner" aria-hidden="true" /> Loading {what}…</div>
}

export function Empty({ children }) {
  return <div className="state empty">{children}</div>
}

// KvKey renders the JetStream key a row maps to, with the environment segment
// dimmed so the shape of the key — env first, then identity — reads at a
// glance. It is the same string the push log prints.
export function KvKey({ envId, rest, bucket }) {
  return (
    <span className="kvkey" title={`${bucket} bucket, key ${envId}.${rest}`}>
      {bucket && <span className="kvkey-bucket">{bucket}</span>}
      <span className="kvkey-env">{envId}.</span>{rest}
    </span>
  )
}

export function Pager({ limit, offset, count, onOffset, onLimit }) {
  const page = Math.floor(offset / limit) + 1
  const full = count === limit
  return (
    <div className="pager">
      <span className="t">
        {count === 0 ? 'no rows' : `rows ${offset + 1}–${offset + count}`} · page {page}
      </span>
      <button className="ghost" disabled={offset === 0} onClick={() => onOffset(Math.max(0, offset - limit))}>
        Previous
      </button>
      <button className="ghost" disabled={!full} onClick={() => onOffset(offset + limit)}>
        Next
      </button>
      <select className="mini" value={limit} onChange={(e) => { onLimit(Number(e.target.value)); onOffset(0) }}>
        {[10, 25, 50, 100].map((n) => <option key={n} value={n}>{n} / page</option>)}
      </select>
    </div>
  )
}

const FOCUSABLE = 'button:not(:disabled), [href], input:not(:disabled), select:not(:disabled), textarea:not(:disabled), [tabindex]:not([tabindex="-1"])'

// Confirm is deliberately modal and deliberately verbose about consequences:
// deleting a flag takes its value rows in every environment with it, and that
// has to be stated before the click, not after. Cancel takes the focus for the
// same reason — a dialog that exists to catch an accidental delete must not put
// that delete under the Enter key.
export function Confirm({ request, onCancel, onConfirm }) {
  const dialog = useRef(null)
  const cancel = useRef(null)

  useEffect(() => {
    if (!request) return
    const opener = document.activeElement
    cancel.current?.focus()
    return () => { if (opener instanceof HTMLElement) opener.focus() }
  }, [request])

  useEffect(() => {
    if (!request) return
    const onKey = (e) => {
      if (e.key === 'Escape') { onCancel(); return }
      if (e.key !== 'Tab') return
      // Tab stays inside the dialog: the page behind it is not answerable yet.
      const stops = dialog.current?.querySelectorAll(FOCUSABLE)
      if (!stops?.length) return
      const first = stops[0]
      const last = stops[stops.length - 1]
      if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus() }
      else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus() }
      else if (!dialog.current.contains(document.activeElement)) { e.preventDefault(); first.focus() }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [request, onCancel])

  if (!request) return null
  return (
    <div className="scrim" onClick={(e) => { if (e.target === e.currentTarget) onCancel() }}>
      <div className="dialog" role="dialog" aria-modal="true" aria-label={request.title} ref={dialog}>
        <h3>{request.title}</h3>
        <p>{request.body}</p>
        {request.consequences?.length > 0 && (
          <ul className="consequences">
            {request.consequences.map((c, i) => <li key={i}>{c}</li>)}
          </ul>
        )}
        {request.target && <div className="dialog-target">{request.target}</div>}
        <div className="dialog-actions">
          <button ref={cancel} className="ghost" onClick={onCancel}>Cancel</button>
          <button className="danger" onClick={onConfirm}>{request.confirmLabel}</button>
        </div>
      </div>
    </div>
  )
}

// ── JSON editing ────────────────────────────────────────────────────────────

// diffPaths walks two decoded JSON trees and reports what actually changed, by
// dotted path. A bad appsettings write breaks every consumer of that service at
// once, so the change gets shown before it is sent, not summarised after.
export function diffPaths(before, after, prefix = '') {
  const out = []
  const isObj = (v) => v !== null && typeof v === 'object' && !Array.isArray(v)

  if (isObj(before) && isObj(after)) {
    const keys = new Set([...Object.keys(before), ...Object.keys(after)])
    for (const k of [...keys].sort()) {
      const path = prefix ? `${prefix}.${k}` : k
      if (!(k in before)) out.push({ kind: 'added', path, to: after[k] })
      else if (!(k in after)) out.push({ kind: 'removed', path, from: before[k] })
      else out.push(...diffPaths(before[k], after[k], path))
    }
    return out
  }

  if (JSON.stringify(before) !== JSON.stringify(after)) {
    out.push({ kind: 'changed', path: prefix || '(root)', from: before, to: after })
  }
  return out
}

const short = (v) => {
  const s = JSON.stringify(v)
  return s === undefined ? 'undefined' : s.length > 60 ? `${s.slice(0, 60)}…` : s
}

// JsonEditor validates on every keystroke, can prettify, and shows the diff
// against the row as loaded. The API rejects anything that is not a JSON
// object, so that check happens here too rather than costing a round trip.
export function JsonEditor({ label, text, onText, original, requireObject = true }) {
  const parsed = useMemo(() => {
    if (text.trim() === '') return { error: 'Enter a JSON object.' }
    try {
      const value = JSON.parse(text)
      if (requireObject && (value === null || typeof value !== 'object' || Array.isArray(value))) {
        return { error: 'Must be a JSON object — the server rejects arrays, strings and numbers here.' }
      }
      return { value }
    } catch (err) {
      return { error: err.message }
    }
  }, [text, requireObject])

  const changes = useMemo(() => {
    if (parsed.error || original === undefined) return []
    return diffPaths(original, parsed.value)
  }, [parsed, original])

  return (
    <div className="json-editor">
      <div className="json-head">
        <label htmlFor={`json-${label}`}>{label}</label>
        <div className="json-tools">
          <span className={parsed.error ? 'chip bad' : 'chip good'}>
            {parsed.error ? 'invalid' : 'valid JSON'}
          </span>
          <button
            className="ghost mini" type="button" disabled={!!parsed.error}
            onClick={() => onText(JSON.stringify(parsed.value, null, 2))}
          >
            Format
          </button>
          {original !== undefined && (
            <button
              className="ghost mini" type="button"
              onClick={() => onText(JSON.stringify(original, null, 2))}
            >
              Revert
            </button>
          )}
        </div>
      </div>
      <textarea
        id={`json-${label}`}
        className={parsed.error ? 'bad' : undefined}
        value={text}
        spellCheck={false}
        onChange={(e) => onText(e.target.value)}
      />
      {parsed.error && <p className="field-err">{parsed.error}</p>}
      {!parsed.error && original !== undefined && (
        changes.length === 0
          ? <p className="hint">No changes yet.</p>
          : (
            <div className="diff">
              <div className="hint">{changes.length} change{changes.length === 1 ? '' : 's'} to send</div>
              {changes.slice(0, 12).map((c, i) => (
                <div key={i} className={`diff-row ${c.kind}`}>
                  <span className="diff-kind">{c.kind}</span>
                  <span className="diff-path">{c.path}</span>
                  {c.kind !== 'added' && <span className="diff-from">{short(c.from)}</span>}
                  {c.kind !== 'removed' && <span className="diff-to">{short(c.to)}</span>}
                </div>
              ))}
              {changes.length > 12 && <div className="hint">…and {changes.length - 12} more</div>}
            </div>
          )
      )}
    </div>
  )
}

export const jsonValue = (text, requireObject = true) => {
  const value = JSON.parse(text)
  if (requireObject && (value === null || typeof value !== 'object' || Array.isArray(value))) {
    throw new Error('must be a JSON object')
  }
  return value
}

// ── data loading ────────────────────────────────────────────────────────────

// useList wraps a list endpoint with the three states a table needs: loading,
// failed and loaded. Reloading keeps the previous rows on screen so the table
// does not flash empty between pages.
export function useList(loader, deps) {
  const [rows, setRows] = useState([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState(null)
  const [nonce, setNonce] = useState(0)
  const seq = useRef(0)

  useEffect(() => {
    const mine = ++seq.current
    setLoading(true)
    loader()
      .then(({ data }) => {
        if (seq.current !== mine) return
        setRows(data ?? [])
        setError(null)
      })
      .catch((err) => { if (seq.current === mine) setError(err) })
      .finally(() => { if (seq.current === mine) setLoading(false) })
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, nonce])

  const reload = useCallback(() => setNonce((n) => n + 1), [])
  return { rows, loading, error, reload }
}

// useDebounced holds a typed filter still for a beat before it reaches useList.
// Typing search_v2 is nine keystrokes and was nine requests; the seq guard kept
// the result correct but the table flickered through eight throwaway renders.
export function useDebounced(value, ms = 250) {
  const [settled, setSettled] = useState(value)
  useEffect(() => {
    const t = setTimeout(() => setSettled(value), ms)
    return () => clearTimeout(t)
  }, [value, ms])
  return settled
}

// useResetOnChange zeroes the pager when a filter owned elsewhere — the header
// environment switcher — changes. It runs during render rather than in an
// effect because an effect fires after useList has already fetched at the old
// offset, which costs a second request for every switch.
export function useResetOnChange(value, reset) {
  const [seen, setSeen] = useState(value)
  if (seen !== value) {
    setSeen(value)
    reset()
  }
}

export const fmtTime = (iso) => (iso ? iso.replace('T', ' ').replace('Z', '') : '—')
