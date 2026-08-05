import { useEffect, useRef, useState } from 'react'

// The live rail. It stays on screen in every view because the question it
// answers — did the change I just made actually reach a consumer — is the one
// worth asking after every write.

// useFlash briefly highlights a row whose value just changed, so a push is
// visible even when the value is similar to the old one.
function useFlash(value) {
  const [flashing, setFlashing] = useState(false)
  const first = useRef(true)
  useEffect(() => {
    if (first.current) { first.current = false; return }
    setFlashing(true)
    const t = setTimeout(() => setFlashing(false), 1200)
    return () => clearTimeout(t)
  }, [value])
  return flashing
}

function FlagRow({ flagKey, payload, envId }) {
  const flashing = useFlash(JSON.stringify(payload))
  return (
    <tr className={flashing ? 'flash' : undefined}>
      <td>
        {flagKey}
        <div className="kvkey tiny"><span className="kvkey-env">{envId}.</span>{flagKey}</div>
      </td>
      <td className={payload.enabled ? 'on' : 'off'}>{String(payload.enabled)}</td>
      <td>{payload.value}</td>
    </tr>
  )
}

function JsonRow({ label, kvRest, value, envId }) {
  const json = JSON.stringify(value)
  const flashing = useFlash(json)
  return (
    <tr className={flashing ? 'flash' : undefined}>
      <td>
        {label}
        <div className="kvkey tiny"><span className="kvkey-env">{envId}.</span>{kvRest}</div>
      </td>
      <td className="clip">{json}</td>
    </tr>
  )
}

export function CachePanel({ snapshot }) {
  const envId = snapshot.environmentId ?? '?'
  const flags = Object.entries(snapshot.flags).sort(([a], [b]) => a.localeCompare(b))
  const micro = Object.entries(snapshot.micro).sort(([a], [b]) => a.localeCompare(b))
  const loc = Object.entries(snapshot.localization).flatMap(([msID, bundles]) =>
    Object.entries(bundles).map(([locale, bundle]) => [`${msID}.${locale}`, bundle]))

  return (
    <div className="live-body">
      <div>
        <div className="hint">FLAGS · {flags.length}</div>
        <table className="kv">
          <thead><tr><th>flag / kv key</th><th>on</th><th>value</th></tr></thead>
          <tbody>
            {flags.length === 0
              ? <tr><td colSpan={3} className="t">empty</td></tr>
              : flags.map(([k, v]) => <FlagRow key={k} flagKey={k} payload={v} envId={envId} />)}
          </tbody>
        </table>
      </div>

      <div>
        <div className="hint">MICROCONFIG · {micro.length}</div>
        <table className="kv">
          <thead><tr><th>service / kv key</th><th>settings</th></tr></thead>
          <tbody>
            {micro.length === 0
              ? <tr><td colSpan={2} className="t">empty</td></tr>
              : micro.map(([k, v]) => <JsonRow key={k} label={`service ${k}`} kvRest={k} value={v} envId={envId} />)}
          </tbody>
        </table>
      </div>

      <div>
        <div className="hint">LOCALIZATION · {loc.length}</div>
        <table className="kv">
          <thead><tr><th>service.locale / kv key</th><th>bundle</th></tr></thead>
          <tbody>
            {loc.length === 0
              ? <tr><td colSpan={2} className="t">empty</td></tr>
              : loc.map(([k, v]) => <JsonRow key={k} label={k} kvRest={k} value={v} envId={envId} />)}
          </tbody>
        </table>
      </div>
    </div>
  )
}

export function EventLog({ events }) {
  if (events.length === 0) {
    return <div className="log"><div className="t">No pushes yet. The cache above was warmed from KV on connect.</div></div>
  }
  return (
    <div className="log">
      {events.map((e) => (
        <div key={e.id}>
          <span className="t">{e.receivedAt?.substring(11, 23)}</span>{' '}
          <span className="b">{e.bucket}</span>{' '}
          <span className="kvkey">{e.key}</span>{' '}
          {e.value == null ? <span className="err">DELETE</span> : <span className="clip-inline">{JSON.stringify(e.value)}</span>}
          {e.latencyMs != null && <span className="lat"> +{e.latencyMs}ms after write</span>}
        </div>
      ))}
    </div>
  )
}

export function LivePanel({ snapshot, events, status, error }) {
  const [tab, setTab] = useState('cache')
  return (
    <aside className="live">
      <div className="live-head">
        <div className="live-title">
          Consumer · env {snapshot.environmentId ?? '…'}
          <span className={`dot ${status === 'live' && !error ? 'ok' : 'bad'}`} />
        </div>
        {error && (
          <div className="live-error" role="alert">
            The consumer cache could not be read, so nothing below is known to be current.
            <div className="t">{error.message}</div>
          </div>
        )}
        <div className="tabs">
          <button className={tab === 'cache' ? 'active' : ''} onClick={() => setTab('cache')}>Cache</button>
          <button className={tab === 'log' ? 'active' : ''} onClick={() => setTab('log')}>
            Push log{events.length ? ` (${events.length})` : ''}
          </button>
        </div>
      </div>
      {tab === 'cache'
        ? <CachePanel snapshot={snapshot} />
        : <EventLog events={events} />}
    </aside>
  )
}
