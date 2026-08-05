import { useEffect, useState } from 'react'
import * as api from '../api.js'
import { Banner, Loading } from '../ui.jsx'

// /health, /metrics and /inventory as first-class reads. Metrics is Prometheus
// text; the counters that say whether KV publishing is actually working are
// pulled to the top rather than left for the operator to find by scrolling.

const HEADLINE = [
  ['centralconfig_kv_publish_success_total', 'KV publishes that reached JetStream'],
  ['centralconfig_kv_publish_failures_total', 'KV publishes that failed (left to the reconciler)'],
  ['centralconfig_kv_delete_failures_total', 'KV key deletions that failed'],
  ['centralconfig_reconcile_keys_republished_total', 'Keys republished from the database'],
  ['centralconfig_reconcile_keys_pruned_total', 'KV keys pruned because the row is gone'],
  ['centralconfig_reconcile_source_failures_total', 'Reconcile sources that failed'],
  ['centralconfig_http_panics_total', 'Handler panics'],
]

export default function System() {
  const [tab, setTab] = useState('health')
  return (
    <>
      <div className="view-head">
        <h2>System</h2>
        <div className="tabs">
          <button className={tab === 'health' ? 'active' : ''} onClick={() => setTab('health')}>Health &amp; metrics</button>
          <button className={tab === 'inventory' ? 'active' : ''} onClick={() => setTab('inventory')}>Inventory</button>
        </div>
      </div>
      {tab === 'health' ? <HealthMetrics /> : <InventoryView />}
    </>
  )
}

function HealthMetrics() {
  const [health, setHealth] = useState(null)
  const [metrics, setMetrics] = useState(null)
  const [error, setError] = useState(null)
  const [loading, setLoading] = useState(true)
  const [raw, setRaw] = useState(false)
  const [nonce, setNonce] = useState(0)

  useEffect(() => {
    let live = true
    setLoading(true)
    Promise.all([api.getHealth(), api.getMetrics()])
      .then(([h, m]) => { if (live) { setHealth(h.data); setMetrics(m); setError(null) } })
      .catch((err) => { if (live) setError(err) })
      .finally(() => { if (live) setLoading(false) })
    return () => { live = false }
  }, [nonce])

  if (loading && !health) return <Loading what="health and metrics" />
  if (error) return <Banner problem={{ action: 'Read health and metrics', error }} />

  const parsed = parseMetrics(metrics ?? '')

  return (
    <div className="panel">
      <div className="toolbar">
        <button className="ghost" onClick={() => setNonce((n) => n + 1)}>Refresh</button>
        <button className="ghost" onClick={() => setRaw((r) => !r)}>{raw ? 'Show summary' : 'Show raw /metrics'}</button>
      </div>

      <div className="stat-row">
        {Object.entries(health ?? {}).map(([k, v]) => (
          <div key={k} className={`stat ${v === 'ok' || v === 'connected' ? 'good' : 'bad'}`}>
            <div className="stat-label">{k}</div>
            <div className="stat-value">{String(v)}</div>
          </div>
        ))}
      </div>

      {raw ? <pre className="raw">{metrics}</pre> : (
        <table className="grid">
          <thead><tr><th>counter</th><th>labels</th><th>value</th></tr></thead>
          <tbody>
            {HEADLINE.flatMap(([name, label]) => {
              const series = parsed.filter((s) => s.name === name)
              if (series.length === 0) {
                return [(
                  <tr key={name}>
                    <td>{label}<div className="t mono">{name}</div></td>
                    <td className="t">—</td>
                    <td className="t">0</td>
                  </tr>
                )]
              }
              return series.map((s, i) => (
                <tr key={`${name}-${i}`}>
                  <td>{i === 0 ? label : ''}<div className="t mono">{i === 0 ? name : ''}</div></td>
                  <td className="t mono">{s.labels || '—'}</td>
                  <td className={name.includes('failure') || name.includes('panic') ? (Number(s.value) > 0 ? 'bad' : 'good') : ''}>
                    {s.value}
                  </td>
                </tr>
              ))
            })}
          </tbody>
        </table>
      )}
    </div>
  )
}

// parseMetrics reads the sample lines of the Prometheus exposition format —
// enough for counters and gauges, which is all this view shows.
function parseMetrics(text) {
  const out = []
  for (const line of text.split('\n')) {
    if (!line || line.startsWith('#')) continue
    const m = /^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{[^}]*\})?\s+(.+)$/.exec(line.trim())
    if (!m) continue
    out.push({ name: m[1], labels: m[2] ? m[2].slice(1, -1) : '', value: m[3] })
  }
  return out
}

function InventoryView() {
  const [inv, setInv] = useState(null)
  const [error, setError] = useState(null)
  const [nonce, setNonce] = useState(0)

  useEffect(() => {
    let live = true
    api.getInventory()
      .then(({ data }) => { if (live) { setInv(data); setError(null) } })
      .catch((err) => { if (live) setError(err) })
    return () => { live = false }
  }, [nonce])

  if (error) return <Banner problem={{ action: 'Read the inventory', error }} />
  if (!inv) return <Loading what="the inventory" />

  const counts = [
    ['flag values', inv.flags?.length ?? 0],
    ['appsettings rows', inv.microConfigs?.length ?? 0],
    ['localization bundles', inv.localization?.length ?? 0],
  ]

  return (
    <div className="panel">
      <div className="toolbar">
        <button className="ghost" onClick={() => setNonce((n) => n + 1)}>Refresh</button>
        <span className="t">GET /inventory — every editable row with the id an update targets.</span>
      </div>
      <div className="stat-row">
        {counts.map(([label, n]) => (
          <div key={label} className="stat">
            <div className="stat-label">{label}</div>
            <div className="stat-value">{n}</div>
          </div>
        ))}
      </div>
      <pre className="raw">{JSON.stringify(inv, null, 2)}</pre>
    </div>
  )
}
