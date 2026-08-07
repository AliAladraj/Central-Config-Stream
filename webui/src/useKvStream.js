import { useCallback, useEffect, useRef, useState } from 'react'
import { getState } from './api.js'

const MAX_EVENTS = 200

// How long a write mark stays eligible to time the next push. A write whose KV
// publish failed never produces one, and a mark left behind would hand that
// write's latency to whatever unrelated change arrives next.
const WRITE_MARK_TTL_MS = 5000

// useKvStream keeps the consumer's cache mirrored in React state.
//
// The value comes from GET /api/state; after that every change arrives on the
// SSE stream and is applied in place. There is no polling and no refetch per
// event: the pushed value IS the new value, which is the whole point of the KV
// watch. The one exception is a reconnect — a stream that dropped may have
// missed pushes, so the cache is read again rather than assumed intact.
export function useKvStream() {
  const [status, setStatus] = useState('connecting')
  const [snapshot, setSnapshot] = useState({ environmentId: null, flags: {}, micro: {}, localization: {} })
  const [events, setEvents] = useState([])
  const [stateError, setStateError] = useState(null)
  // Monotonic, unlike events.length, which stops growing at MAX_EVENTS. Views
  // that computed something from a snapshot use it to say how far behind they
  // have fallen since.
  const [pushes, setPushes] = useState(0)

  // Set just before a write leaves the browser. The KV push routinely arrives
  // before the PUT response does, so latency is measured from this mark rather
  // than from the response.
  const pendingWrite = useRef(null)

  // Held so a view can ask for the cache again after a failed read; it is the
  // same call the stream makes on reconnect.
  const syncRef = useRef(null)
  const resync = useCallback(() => syncRef.current?.(), [])

  useEffect(() => {
    let cancelled = false
    const sync = () => getState()
      .then((s) => { if (!cancelled) { setSnapshot(normalize(s)); setStateError(null) } })
      .catch((err) => { if (!cancelled) setStateError(err) })

    syncRef.current = sync
    sync()

    const es = new EventSource('/api/events')
    // Only a reconnect re-reads state; the first open already has it in flight.
    let dropped = false
    es.onopen = () => {
      setStatus('live')
      if (dropped) { dropped = false; sync() }
    }
    es.onerror = () => { dropped = true; setStatus('disconnected') }
    es.onmessage = (m) => {
      const e = JSON.parse(m.data)
      const at = performance.now()

      let latencyMs = null
      if (pendingWrite.current) {
        const since = at - pendingWrite.current.startedAt
        pendingWrite.current = null
        if (since <= WRITE_MARK_TTL_MS) latencyMs = Math.round(since)
      }

      setSnapshot((prev) => applyEvent(prev, e))
      setEvents((prev) => [{ ...e, latencyMs, id: `${e.key}-${at}` }, ...prev].slice(0, MAX_EVENTS))
      setPushes((n) => n + 1)
    }

    return () => { cancelled = true; syncRef.current = null; es.close() }
  }, [])

  return { status, snapshot, events, pushes, stateError, resync, pendingWrite }
}

function normalize(s) {
  return {
    environmentId: s.environmentId ?? null,
    flags: s.flags ?? {},
    micro: s.micro ?? {},
    localization: s.localization ?? {},
  }
}

// applyEvent mirrors the consumer-side key layout:
//   FLAGS         {env}.{flagKey}
//   SERVICESETTINGS   {env}.{microserviceID}
//   LOCALIZATION  {env}.{microserviceID}.{locale}
function applyEvent(prev, e) {
  const rest = stripEnv(e.key, prev.environmentId)
  const deleted = e.value === null || e.value === undefined

  if (e.bucket === 'FLAGS') {
    const flags = { ...prev.flags }
    if (deleted) delete flags[rest]
    else flags[rest] = e.value
    return { ...prev, flags }
  }

  if (e.bucket === 'SERVICESETTINGS') {
    const micro = { ...prev.micro }
    if (deleted) delete micro[rest]
    else micro[rest] = e.value
    return { ...prev, micro }
  }

  if (e.bucket === 'LOCALIZATION') {
    const [msID, ...localeParts] = rest.split('.')
    const locale = localeParts.join('.')
    const localization = { ...prev.localization }
    const bundles = { ...(localization[msID] ?? {}) }
    if (deleted) delete bundles[locale]
    else bundles[locale] = e.value
    localization[msID] = bundles
    return { ...prev, localization }
  }

  return prev
}

function stripEnv(key, envID) {
  const prefix = `${envID}.`
  return key.startsWith(prefix) ? key.slice(prefix.length) : key
}
