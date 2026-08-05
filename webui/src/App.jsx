import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import * as api from './api.js'
import { useKvStream } from './useKvStream.js'
import { Banner, Confirm, Notice } from './ui.jsx'
import { LivePanel } from './Panels.jsx'
import Overview from './views/Overview.jsx'
import Flags from './views/Flags.jsx'
import Configs from './views/Configs.jsx'
import Localization from './views/Localization.jsx'
import Reference from './views/Reference.jsx'
import Audit from './views/Audit.jsx'
import System from './views/System.jsx'

const NAV = [
  {
    group: 'Watch',
    items: [
      { id: 'overview', label: 'Overview', hint: 'health, drift, what changed' },
      { id: 'audit', label: 'Audit log', hint: 'who changed what' },
      { id: 'system', label: 'System', hint: 'health, metrics, inventory' },
    ],
  },
  {
    group: 'Configure',
    items: [
      { id: 'flags', label: 'Flags', hint: 'definitions and the env matrix' },
      { id: 'configs', label: 'Appsettings', hint: 'per service, per env' },
      { id: 'localization', label: 'Localization', hint: 'translation bundles' },
    ],
  },
  {
    group: 'Reference',
    items: [
      { id: 'reference', label: 'Environments & services', hint: 'the tables everything keys off' },
    ],
  },
]

const VIEWS = {
  overview: Overview,
  flags: Flags,
  configs: Configs,
  localization: Localization,
  reference: Reference,
  audit: Audit,
  system: System,
}

// useReference keeps the three lookup tables every other view needs to turn an
// id into a name. They change rarely, so they are loaded once and refreshed
// explicitly after a write rather than re-fetched per view.
function useReference() {
  const [data, setData] = useState({ environments: [], microservices: [], flags: [] })
  const [error, setError] = useState(null)
  const [nonce, setNonce] = useState(0)

  useEffect(() => {
    let live = true
    Promise.all([
      api.listEnvironments({ limit: 200 }),
      api.listMicroservices({ limit: 200 }),
      api.listFlags({ limit: 200 }),
    ])
      .then(([envs, svcs, flags]) => {
        if (!live) return
        setData({
          environments: envs.data ?? [],
          microservices: svcs.data ?? [],
          flags: flags.data ?? [],
        })
        setError(null)
      })
      .catch((err) => { if (live) setError(err) })
    return () => { live = false }
  }, [nonce])

  const reload = useCallback(() => setNonce((n) => n + 1), [])

  const envName = useCallback(
    (id) => data.environments.find((e) => e.id === Number(id))?.name ?? `env ${id}`,
    [data.environments],
  )
  const svcName = useCallback(
    (id) => data.microservices.find((m) => m.id === Number(id))?.name ?? `service ${id}`,
    [data.microservices],
  )

  return { ...data, reload, error, envName, svcName }
}

export default function App() {
  const { status, snapshot, events, pushes, stateError, resync, pendingWrite } = useKvStream()
  const [view, setView] = useState('overview')
  const [envId, setEnvId] = useState('')
  const [problem, setProblem] = useState(null)
  const [notice, setNotice] = useState(null)
  const [confirmReq, setConfirmReq] = useState(null)
  const [navOpen, setNavOpen] = useState(false)
  const resolver = useRef(null)
  const refs = useReference()

  // confirm shows the modal and resolves once the operator has answered, so a
  // caller reads as `if (!(await confirm(...))) return`.
  const confirm = useCallback((request) => new Promise((resolve) => {
    resolver.current = resolve
    setConfirmReq(request)
  }), [])

  const answer = useCallback((ok) => {
    setConfirmReq(null)
    resolver.current?.(ok)
    resolver.current = null
  }, [])

  const decline = useCallback(() => answer(false), [answer])
  const accept = useCallback(() => answer(true), [answer])

  // write marks the pending write so the KV push that follows can be timed,
  // then reports either the elapsed round trip or the server's own error.
  const write = useCallback(async (action, fn) => {
    pendingWrite.current = { startedAt: performance.now(), path: action }
    setProblem(null)
    setNotice(null)
    try {
      const res = await fn()
      setNotice(`${action} · HTTP ${res.status} in ${res.elapsedMs}ms (database write + KV publish)`)
      return { ok: true, ...res }
    } catch (error) {
      pendingWrite.current = null
      setProblem({ action, error })
      return { ok: false, error }
    }
  }, [pendingWrite])

  // Only the newest event can carry a live latency. Reaching further back
  // would leave an old write's number on screen as if it were this one's.
  const lastLatency = events[0]?.latencyMs
  const View = VIEWS[view]

  const ctx = useMemo(() => ({
    envId, setEnvId, refs, write, confirm, snapshot, setView,
    consumerError: stateError, pushes, resync,
  }), [envId, refs, write, confirm, snapshot, stateError, pushes, resync])

  return (
    <div className="shell">
      <header>
        <button className="ghost nav-toggle" onClick={() => setNavOpen((o) => !o)} aria-label="Toggle navigation">≡</button>
        <h1>central&#8209;config <span className="t">console</span></h1>

        <label className="env-switch">
          <span>Environment</span>
          <select value={envId} onChange={(e) => setEnvId(e.target.value)}>
            <option value="">All environments</option>
            {refs.environments.map((e) => (
              <option key={e.id} value={e.id}>{e.name} (id {e.id})</option>
            ))}
          </select>
        </label>

        <div className="header-pills">
          <span className={`pill ${status === 'live' ? 'live' : status === 'disconnected' ? 'down' : ''}`}>
            {status === 'live' ? 'watching KV' : status}
          </span>
          <span className={`pill ${stateError ? 'down' : ''}`}>
            {stateError ? 'consumer cache unavailable' : `consumer env ${snapshot.environmentId ?? '…'}`}
          </span>
          <span className="pill">
            {lastLatency != null ? `last push +${lastLatency}ms` : 'no push yet'}
          </span>
        </div>
      </header>

      <div className="layout">
        <nav className={navOpen ? 'open' : undefined}>
          {NAV.map((section) => (
            <div key={section.group} className="nav-group">
              <div className="nav-title">{section.group}</div>
              {section.items.map((item) => (
                <button
                  key={item.id}
                  className={`nav-item ${view === item.id ? 'active' : ''}`}
                  onClick={() => { setView(item.id); setNavOpen(false) }}
                >
                  <span className="nav-label">{item.label}</span>
                  <span className="nav-hint">{item.hint}</span>
                </button>
              ))}
            </div>
          ))}
        </nav>

        <main>
          <Banner problem={problem} onDismiss={() => setProblem(null)} />
          {refs.error && <Banner problem={{ action: 'Load reference tables', error: refs.error }} />}
          <Notice text={notice} />
          <View ctx={ctx} />
        </main>

        <LivePanel snapshot={snapshot} events={events} status={status} error={stateError} />
      </div>

      <Confirm request={confirmReq} onCancel={decline} onConfirm={accept} />
    </div>
  )
}
