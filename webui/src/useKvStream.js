import { useEffect, useRef, useState } from 'react'
import { getState } from './api.js'

const MAX_EVENTS = 200

// useKvStream keeps the consumer's cache mirrored in React state.
//
// The initial value comes from GET /api/state; after that every change arrives
// on the SSE stream and is applied in place. There is no polling and no
// refetch per event: the pushed value IS the new value, which is the whole
// point of the KV watch.
export function useKvStream() {
  const [status, setStatus] = useState('connecting')
  const [snapshot, setSnapshot] = useState({ environmentId: null, flags: {}, micro: {}, localization: {} })
  const [events, setEvents] = useState([])

  // Set just before a write leaves the browser. The KV push routinely arrives
  // before the PUT response does, so latency is measured from this mark rather
  // than from the response.
  const pendingWrite = useRef(null)

  useEffect(() => {
    let cancelled = false
    getState()
      .then((s) => { if (!cancelled) setSnapshot(normalize(s)) })
      .catch(() => {})

    const es = new EventSource('/api/events')
    es.onopen = () => setStatus('live')
    es.onerror = () => setStatus('disconnected')
    es.onmessage = (m) => {
      const e = JSON.parse(m.data)
      const at = performance.now()

      let latencyMs = null
      if (pendingWrite.current) {
        latencyMs = Math.round(at - pendingWrite.current.startedAt)
        pendingWrite.current = null
      }

      setSnapshot((prev) => applyEvent(prev, e))
      setEvents((prev) => [{ ...e, latencyMs, id: `${e.key}-${at}` }, ...prev].slice(0, MAX_EVENTS))
    }

    return () => { cancelled = true; es.close() }
  }, [])

  return { status, snapshot, events, pendingWrite }
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
//   MICROCONFIG   {env}.{microserviceID}
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

  if (e.bucket === 'MICROCONFIG') {
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
