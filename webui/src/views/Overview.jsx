import { useCallback, useEffect, useState } from 'react'
import * as api from '../api.js'
import { computeDrift, driftLabel } from '../drift.js'
import { Empty, KvKey, Loading, fmtTime } from '../ui.jsx'

// The landing view answers two questions: is the control plane healthy, and is
// what the fleet is running the same as what the database says it should be.
// The second one is the drift check — nothing else in this console compares the
// two sides, and a silent divergence there is the failure this system exists to
// prevent.

export default function Overview({ ctx }) {
  const { snapshot, refs, setView } = ctx
  const [state, setState] = useState({ loading: true })
  const [health, setHealth] = useState(null)
  const [recent, setRecent] = useState([])
  const [nonce, setNonce] = useState(0)

  const watched = snapshot.environmentId

  const reload = useCallback(() => setNonce((n) => n + 1), [])

  useEffect(() => {
    if (watched == null) return
    let live = true
    setState((s) => ({ ...s, loading: true }))
    Promise.all([
      api.listFlagValues({ limit: 200 }),
      api.listConfigs({ limit: 200 }),
      api.listLocalization({ limit: 200 }),
      api.getHealth().catch(() => ({ data: null })),
      api.listAudit({ limit: 8 }).catch(() => ({ data: [] })),
    ])
      .then(([fv, cfg, loc, h, aud]) => {
        if (!live) return
        setHealth(h.data)
        setRecent(aud.data ?? [])
        setState({
          loading: false,
          drift: computeDrift({
            watchedEnvId: watched,
            snapshot,
            flagValues: fv.data ?? [],
            configs: cfg.data ?? [],
            localization: loc.data ?? [],
          }),
        })
      })
      .catch((error) => { if (live) setState({ loading: false, error }) })
    return () => { live = false }
    // Re-running on every KV push would hammer the API; the operator refreshes.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [watched, nonce])

  return (
    <>
      <div className="view-head">
        <h2>Overview</h2>
        <button className="ghost" onClick={reload}>Re-check</button>
      </div>

      <div className="stat-row">
        {Object.entries(health ?? {}).map(([k, v]) => (
          <div key={k} className={`stat ${v === 'ok' || v === 'connected' ? 'good' : 'bad'}`}>
            <div className="stat-label">{k}</div>
            <div className="stat-value">{String(v)}</div>
          </div>
        ))}
        <div className="stat">
          <div className="stat-label">environments</div>
          <div className="stat-value">{refs.environments.length}</div>
        </div>
        <div className="stat">
          <div className="stat-label">microservices</div>
          <div className="stat-value">{refs.microservices.length}</div>
        </div>
        <div className="stat">
          <div className="stat-label">flags</div>
          <div className="stat-value">{refs.flags.length}</div>
        </div>
      </div>

      <div className="panel">
        <h3 className="panel-title">Database vs consumer cache</h3>
        {state.loading ? <Loading what="rows to compare" />
          : state.error ? <Empty>Could not compare: {state.error.message}</Empty>
            : <DriftReport drift={state.drift} refs={refs} />}
      </div>

      <div className="panel">
        <h3 className="panel-title">
          Recent changes
          <button className="ghost mini" onClick={() => setView('audit')}>Open the audit log</button>
        </h3>
        {recent.length === 0 ? <Empty>Nothing recorded yet.</Empty> : (
          <table className="grid">
            <thead><tr><th>when</th><th>actor</th><th>method</th><th>path</th><th>status</th></tr></thead>
            <tbody>
              {recent.map((e) => (
                <tr key={e.id}>
                  <td className="nowrap t">{fmtTime(e.occurredAt)}</td>
                  <td><span className="actor">{e.actor || '—'}</span></td>
                  <td><span className={`method ${e.method}`}>{e.method}</span></td>
                  <td className="mono">{e.path}</td>
                  <td><span className={`chip ${e.statusCode >= 400 ? 'warn' : 'good'}`}>{e.statusCode}</span></td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </>
  )
}

function DriftReport({ drift, refs }) {
  const { issues, compared, outOfScope, watchedEnvId } = drift
  const scope = (
    <p className="hint">
      This console holds one consumer, watching <strong>{refs.envName(watchedEnvId)}</strong> (id {watchedEnvId}).
      Only that environment can be compared — {compared} row{compared === 1 ? '' : 's'} checked,
      {' '}{outOfScope} row{outOfScope === 1 ? '' : 's'} in other environments skipped rather than
      reported as drift.
    </p>
  )

  if (issues.length === 0) {
    return (
      <>
        <div className="verdict good">
          In sync. Every row in {refs.envName(watchedEnvId)} matches what the consumer is serving.
        </div>
        {scope}
      </>
    )
  }

  return (
    <>
      <div className="verdict bad">
        {issues.length} difference{issues.length === 1 ? '' : 's'} between the database and what the
        consumer is serving.
      </div>
      <table className="grid">
        <thead><tr><th>bucket</th><th>kv key</th><th>problem</th><th>detail</th></tr></thead>
        <tbody>
          {issues.map((i, n) => (
            <tr key={n}>
              <td className="t">{i.domain}</td>
              <td><KvKey envId={watchedEnvId} rest={i.key} /></td>
              <td><span className="chip warn">{driftLabel[i.kind]}</span></td>
              <td className="t">{i.detail}</td>
            </tr>
          ))}
        </tbody>
      </table>
      <p className="hint">
        A row that is in the database but not in the cache usually means a KV publish failed and the
        reconciler has not swept yet. The opposite means a KV key outlived its row.
      </p>
      {scope}
    </>
  )
}
