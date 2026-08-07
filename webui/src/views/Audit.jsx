import { Fragment, useState } from 'react'
import * as api from '../api.js'
import { Banner, Empty, Loading, Pager, useDebounced, useList } from '../ui.jsx'

// The audit log is the only record that survives the change: a KV bucket holds
// the current value and nothing else. It gets the full width of the view for
// that reason, and every recorded field is on screen rather than behind a
// tooltip — except the request body, which is long and opens per row.

const statusTone = (code) => {
  if (code >= 500) return 'bad'
  if (code === 429) return 'warn'
  if (code >= 400) return 'warn'
  if (code >= 200 && code < 300) return 'good'
  return ''
}

export default function Audit({ ctx }) {
  const { refs } = ctx
  const [actor, setActor] = useState('')
  const [from, setFrom] = useState('')
  const [to, setTo] = useState('')
  const [limit, setLimit] = useState(50)
  const [offset, setOffset] = useState(0)
  const [open, setOpen] = useState(null)

  // The typed filter settles before it is queried; the dates are picked, not
  // typed, so they go straight through.
  const actorQuery = useDebounced(actor)

  const { rows, loading, error, reload } = useList(
    () => api.listAudit({ actor: actorQuery, from, to, limit, offset }),
    [actorQuery, from, to, limit, offset],
  )

  const failures = rows.filter((r) => r.statusCode >= 400).length

  return (
    <>
      <div className="view-head">
        <h2>Audit log</h2>
        <span className="t">
          Every mutating request, recorded after it committed. Reads are not audited.
        </span>
      </div>

      <div className="panel">
        <div className="toolbar">
          <label className="field">
            <span>actor</span>
            <input
              value={actor}
              onChange={(e) => { setActor(e.target.value); setOffset(0) }}
              placeholder="any token name"
            />
          </label>
          <label className="field">
            <span>from</span>
            <input type="date" value={from} onChange={(e) => { setFrom(e.target.value); setOffset(0) }} />
          </label>
          <label className="field">
            <span>to</span>
            <input type="date" value={to} onChange={(e) => { setTo(e.target.value); setOffset(0) }} />
          </label>
          <button className="ghost" onClick={reload}>Refresh</button>
          {(actor || from || to) && (
            <button className="ghost" onClick={() => { setActor(''); setFrom(''); setTo(''); setOffset(0) }}>Clear filters</button>
          )}
          {rows.length > 0 && (
            <span className="t">{rows.length} entries · {failures} rejected</span>
          )}
        </div>

        {error ? <Banner problem={{ action: 'Load the audit log', error }} />
          : loading && rows.length === 0 ? <Loading what="audit entries" />
            : rows.length === 0 ? (
              <Empty>
                {actor || from || to
                  ? 'No entries match these filters. Widen the date range or clear the actor.'
                  : 'No changes have been recorded yet. Make a write and it will appear here.'}
              </Empty>
            ) : (
              <div className="scroll-x">
                <table className="grid audit">
                  <thead>
                    <tr>
                      <th>when</th><th>actor</th><th>method</th><th>path</th>
                      <th>domain</th><th>target</th><th>env</th><th>status</th><th>from</th><th />
                    </tr>
                  </thead>
                  <tbody>
                    {rows.map((row) => (
                      <Fragment key={row.id}>
                        <tr className={open === row.id ? 'sel' : undefined}>
                          {/* The widest fixed thing in the row: 19% of the table spent on a
                              datetime nothing else can compress. It keeps its space when the
                              table has room and folds at the date/time boundary when it does
                              not, which is what buys the last column back on a laptop. */}
                          <td>{row.occurredAt?.replace('T', ' ').replace('Z', '')}</td>
                          <td><span className="actor">{row.actor || '—'}</span></td>
                          <td><span className={`method ${row.method}`}>{row.method}</span></td>
                          <td className="mono">{row.path}</td>
                          <td className="t">{row.domain || '—'}</td>
                          <td className="t">{row.targetId || '—'}</td>
                          <td className="t">{row.environmentId ? refs.envName(row.environmentId) : '—'}</td>
                          <td><span className={`chip ${statusTone(row.statusCode)}`}>{row.statusCode}</span></td>
                          <td className="t">{row.remoteAddr || '—'}</td>
                          <td className="right">
                            {row.requestBody && (
                              <button className="ghost mini" onClick={() => setOpen(open === row.id ? null : row.id)}>
                                {open === row.id ? 'Hide body' : 'Body'}
                              </button>
                            )}
                          </td>
                        </tr>
                        {open === row.id && row.requestBody && (
                          <tr className="body-row">
                            <td colSpan={10}>
                              <pre>{pretty(row.requestBody)}</pre>
                              <p className="hint">
                                Password- and token-shaped fields are stored as [redacted]; the audit log is
                                read more widely than the write API is used.
                              </p>
                            </td>
                          </tr>
                        )}
                      </Fragment>
                    ))}
                  </tbody>
                </table>
              </div>
            )}

        <Pager limit={limit} offset={offset} count={rows.length} onOffset={setOffset} onLimit={setLimit} />
      </div>
    </>
  )
}

function pretty(body) {
  try { return JSON.stringify(JSON.parse(body), null, 2) } catch { return body }
}
