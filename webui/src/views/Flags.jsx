import { useCallback, useEffect, useMemo, useState } from 'react'
import * as api from '../api.js'
import { Empty, KvKey, Loading, Pager, fmtTime, useList } from '../ui.jsx'

// Flags are the one domain where a value exists once per environment, so the
// natural view is a matrix rather than a list: "what is search_v2 in prod"
// is the question, and a flat list of value rows answers it only by eye.

export default function Flags({ ctx }) {
  const [tab, setTab] = useState('matrix')
  return (
    <>
      <div className="view-head">
        <h2>Flags</h2>
        <div className="tabs">
          <button className={tab === 'matrix' ? 'active' : ''} onClick={() => setTab('matrix')}>Matrix</button>
          <button className={tab === 'values' ? 'active' : ''} onClick={() => setTab('values')}>Value rows</button>
          <button className={tab === 'defs' ? 'active' : ''} onClick={() => setTab('defs')}>Definitions</button>
        </div>
      </div>
      {tab === 'matrix' && <Matrix ctx={ctx} />}
      {tab === 'values' && <ValueRows ctx={ctx} />}
      {tab === 'defs' && <Definitions ctx={ctx} />}
    </>
  )
}

// ── matrix ──────────────────────────────────────────────────────────────────

function Matrix({ ctx }) {
  const { refs, snapshot, envId } = ctx
  const [flagKey, setFlagKey] = useState('')
  const [cell, setCell] = useState(null) // { flagId, flagKey, environmentId, row|null }

  const { rows, loading, error, reload } = useList(
    () => api.listFlagValues({ flagKey, limit: 200 }),
    [flagKey],
  )

  const byFlagEnv = useMemo(() => {
    const m = new Map()
    for (const r of rows) m.set(`${r.flagId}:${r.environmentId}`, r)
    return m
  }, [rows])

  const flags = useMemo(
    () => refs.flags.filter((f) => !flagKey || f.flagKey.includes(flagKey)),
    [refs.flags, flagKey],
  )
  const envs = refs.environments

  const watched = snapshot.environmentId

  if (error) return <Empty>Could not load flag values: {error.message}</Empty>

  return (
    <div className="panel">
      <div className="toolbar">
        <label className="field">
          <span>Flag key contains</span>
          <input value={flagKey} onChange={(e) => setFlagKey(e.target.value)} placeholder="all flags" />
        </label>
        <button className="ghost" onClick={reload}>Refresh</button>
        <span className="t spacer-note">
          A filled cell is a flag value row. Empty means the flag has no value in that environment.
        </span>
      </div>

      {loading && rows.length === 0 ? <Loading what="the matrix" /> : (
        flags.length === 0 ? <Empty>No flag definitions yet. Create one under Definitions.</Empty> : (
          <div className="scroll-x">
            <table className="matrix">
              <thead>
                <tr>
                  <th className="sticky-col">flag</th>
                  {envs.map((e) => (
                    <th key={e.id} className={String(e.id) === String(envId) ? 'sel' : undefined}>
                      {e.name}
                      <div className="t">
                        id {e.id}{e.id === watched ? ' · watched' : ''}
                      </div>
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {flags.map((f) => (
                  <tr key={f.id}>
                    <th className="sticky-col">
                      {f.flagKey}
                      <div className="t">id {f.id}{f.isActive ? '' : ' · inactive'}</div>
                    </th>
                    {envs.map((e) => {
                      const row = byFlagEnv.get(`${f.id}:${e.id}`)
                      const drifted = row && e.id === watched && driftsFromCache(row, snapshot)
                      return (
                        <td key={e.id} className={String(e.id) === String(envId) ? 'sel' : undefined}>
                          <button
                            className={`cell ${row ? (row.enabled ? 'on' : 'off') : 'none'} ${drifted ? 'drift' : ''}`}
                            onClick={() => setCell({ flag: f, env: e, row: row ?? null })}
                            title={drifted ? 'The consumer cache does not match this row' : undefined}
                          >
                            {row
                              ? <><span className="cell-state">{row.enabled ? 'on' : 'off'}</span><span className="cell-val">{row.value || '∅'}</span></>
                              : <span className="cell-add">add</span>}
                            {drifted && <span className="cell-drift">drift</span>}
                          </button>
                        </td>
                      )
                    })}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )
      )}

      {cell && (
        <CellEditor
          ctx={ctx}
          cell={cell}
          onClose={() => setCell(null)}
          onSaved={() => { setCell(null); reload() }}
        />
      )}
    </div>
  )
}

function driftsFromCache(row, snapshot) {
  const cached = snapshot.flags?.[row.flagKey]
  if (!cached) return true
  return cached.value !== row.value || cached.enabled !== Boolean(Number(row.enabled))
}

// CellEditor is create and edit in one place: the same cell either has a row or
// does not, and an operator thinks of both as "set this flag here".
function CellEditor({ ctx, cell, onClose, onSaved }) {
  const { write, confirm, snapshot } = ctx
  const { flag, env } = cell
  const [row, setRow] = useState(cell.row)
  const [value, setValue] = useState(cell.row?.value ?? '')
  const [enabled, setEnabled] = useState(String(cell.row?.enabled ?? 1))
  const [conflict, setConflict] = useState(false)
  const [busy, setBusy] = useState(false)

  const refresh = useCallback(async () => {
    if (!row) return
    try {
      const { data } = await api.getFlagValue(row.id)
      setRow({ ...data, flagKey: flag.flagKey })
      setValue(data.value)
      setEnabled(String(data.enabled))
      setConflict(false)
    } catch { /* the banner already carries the failure */ }
  }, [row, flag.flagKey])

  const save = async () => {
    setBusy(true)
    const r = row
      ? await write(`PUT /flags/values (${flag.flagKey} in ${env.name})`, () => api.updateFlagValue({
        id: row.id,
        environmentId: row.environmentId,
        flagId: row.flagId,
        value,
        enabled: Number(enabled),
        // The row as loaded. A lost race is a 409 rather than a silent
        // overwrite of whatever the other admin just wrote.
        expectedUpdatedAt: row.updatedAt,
      }))
      : await write(`POST /flags/values (${flag.flagKey} in ${env.name})`, () => api.createFlagValue({
        environmentId: env.id,
        flagId: flag.id,
        value,
        enabled: Number(enabled),
      }))
    setBusy(false)
    if (r.ok) onSaved()
    else if (r.error?.status === 409 && row) setConflict(true)
  }

  const remove = async () => {
    const ok = await confirm({
      title: `Delete the ${flag.flagKey} value in ${env.name}?`,
      body: 'This removes the flag value row for this environment only. The flag definition and its values in other environments are untouched.',
      consequences: [
        `The KV key ${env.id}.${flag.flagKey} is purged, so consumers in ${env.name} stop receiving this flag.`,
        'Code reading this flag falls back to whatever default it hardcodes.',
      ],
      target: `flag value id ${row.id}`,
      confirmLabel: 'Delete value',
    })
    if (!ok) return
    setBusy(true)
    const r = await write(`DELETE /flags/values/${row.id}`, () => api.deleteFlagValue(row.id))
    setBusy(false)
    if (r.ok) onSaved()
  }

  return (
    <div className="editor">
      <div className="editor-head">
        <div>
          <strong>{flag.flagKey}</strong> in <strong>{env.name}</strong>
          <div className="t">{row ? `flag value id ${row.id}` : 'no row yet — saving creates one'}</div>
        </div>
        <KvKey bucket="FLAGS" envId={env.id} rest={flag.flagKey} />
        <button className="ghost close" onClick={onClose} aria-label="Close">×</button>
      </div>

      {env.id !== snapshot.environmentId && (
        <p className="hint">
          This console watches environment {snapshot.environmentId}, so a write here will succeed
          but no push will appear in the live panel.
        </p>
      )}

      <div className="row">
        <label className="field">
          <span>value</span>
          <input value={value} onChange={(e) => setValue(e.target.value)} />
        </label>
        <label className="field">
          <span>enabled</span>
          <select value={enabled} onChange={(e) => setEnabled(e.target.value)}>
            <option value="1">1 · on</option>
            <option value="0">0 · off</option>
          </select>
        </label>
        {row && (
          <label className="field">
            <span>loaded at (concurrency check)</span>
            <input value={fmtTime(row.updatedAt)} readOnly />
          </label>
        )}
      </div>

      {conflict && (
        <div className="conflict">
          Someone else changed this row after you loaded it, so the write was refused rather than
          overwriting their change.
          <button className="ghost" onClick={refresh}>Reload this row</button>
        </div>
      )}

      <div className="actions">
        <button disabled={busy} onClick={save}>{row ? 'Save value' : 'Create value'}</button>
        {row && <button className="danger" disabled={busy} onClick={remove}>Delete value</button>}
      </div>
    </div>
  )
}

// ── flag value rows ─────────────────────────────────────────────────────────

function ValueRows({ ctx }) {
  const { envId, refs, write, confirm } = ctx
  const [flagKey, setFlagKey] = useState('')
  const [limit, setLimit] = useState(25)
  const [offset, setOffset] = useState(0)

  const { rows, loading, error, reload } = useList(
    () => api.listFlagValues({ environmentId: envId, flagKey, limit, offset }),
    [envId, flagKey, limit, offset],
  )
  useEffect(() => { setOffset(0) }, [envId, flagKey])

  const remove = async (row) => {
    const ok = await confirm({
      title: `Delete flag value ${row.id}?`,
      body: `Removes ${row.flagKey} from ${refs.envName(row.environmentId)} only.`,
      consequences: [`The KV key ${row.environmentId}.${row.flagKey} is purged.`],
      target: `${row.flagKey} = ${row.value} (enabled ${row.enabled})`,
      confirmLabel: 'Delete value',
    })
    if (!ok) return
    const r = await write(`DELETE /flags/values/${row.id}`, () => api.deleteFlagValue(row.id))
    if (r.ok) reload()
  }

  return (
    <div className="panel">
      <div className="toolbar">
        <label className="field">
          <span>flagKey</span>
          <input value={flagKey} onChange={(e) => setFlagKey(e.target.value)} placeholder="all" />
        </label>
        <span className="t">environmentId comes from the header switcher</span>
        <button className="ghost" onClick={reload}>Refresh</button>
      </div>

      {error ? <Empty>Could not load: {error.message}</Empty>
        : loading && rows.length === 0 ? <Loading what="flag values" />
          : rows.length === 0 ? <Empty>No flag values match. Set one from the matrix.</Empty>
            : (
              <div className="scroll-x">
                <table className="grid">
                  <thead>
                    <tr>
                      <th>id</th><th>flag</th><th>env</th><th>value</th><th>on</th>
                      <th>kv key</th><th>updated</th><th />
                    </tr>
                  </thead>
                  <tbody>
                    {rows.map((row) => (
                      <tr key={row.id}>
                        <td>{row.id}</td>
                        <td>{row.flagKey}</td>
                        <td>{refs.envName(row.environmentId)}</td>
                        <td>{row.value}</td>
                        <td className={row.enabled ? 'on' : 'off'}>{row.enabled}</td>
                        <td><KvKey bucket="FLAGS" envId={row.environmentId} rest={row.flagKey} /></td>
                        <td className="t">{fmtTime(row.updatedAt)}</td>
                        <td><button className="ghost danger-text" onClick={() => remove(row)}>Delete</button></td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}

      <Pager limit={limit} offset={offset} count={rows.length} onOffset={setOffset} onLimit={setLimit} />
    </div>
  )
}

// ── flag definitions ────────────────────────────────────────────────────────

function Definitions({ ctx }) {
  const { refs, write, confirm } = ctx
  const [flagKey, setFlagKey] = useState('')
  const [limit, setLimit] = useState(25)
  const [offset, setOffset] = useState(0)
  const [newKey, setNewKey] = useState('')
  const [newActive, setNewActive] = useState('1')

  const { rows, loading, error, reload } = useList(
    () => api.listFlags({ flagKey, limit, offset }),
    [flagKey, limit, offset],
  )

  const keyProblem = newKey === '' ? null
    : newKey.includes('.') ? 'A flag key may not contain a dot — it would split the KV key.'
      : /\s/.test(newKey) ? 'A flag key may not contain whitespace.'
        : null

  const create = async () => {
    const r = await write('POST /flags', () => api.createFlag({ flagKey: newKey, isActive: Number(newActive) }))
    if (r.ok) { setNewKey(''); reload(); refs.reload() }
  }

  const remove = async (row) => {
    // The cascade is the whole reason this confirmation exists.
    const affected = await countValues(row.flagKey)
    const ok = await confirm({
      title: `Delete the flag ${row.flagKey}?`,
      body: 'Deleting a flag definition cascades. This is not limited to one environment.',
      consequences: [
        affected === null
          ? 'Its value rows in every environment are deleted with it.'
          : `Its ${affected} value row${affected === 1 ? '' : 's'} — across every environment — are deleted with it.`,
        'The matching KV key is purged in each of those environments, so every consumer stops receiving this flag within milliseconds.',
        'Code still reading this flag falls back to its hardcoded default.',
      ],
      target: `flag id ${row.id} · ${row.flagKey}`,
      confirmLabel: 'Delete flag and all its values',
    })
    if (!ok) return
    const r = await write(`DELETE /flags/${row.id}`, () => api.deleteFlag(row.id))
    if (r.ok) { reload(); refs.reload() }
  }

  return (
    <div className="panel">
      <div className="create-card">
        <h3>New flag</h3>
        <div className="row">
          <label className="field">
            <span>flagKey</span>
            <input value={newKey} onChange={(e) => setNewKey(e.target.value)} placeholder="search_v3" />
          </label>
          <label className="field">
            <span>isActive</span>
            <select value={newActive} onChange={(e) => setNewActive(e.target.value)}>
              <option value="1">1 · active</option>
              <option value="0">0 · inactive</option>
            </select>
          </label>
        </div>
        {keyProblem && <p className="field-err">{keyProblem}</p>}
        <p className="hint">A definition carries no environment. Give it a value per environment from the matrix.</p>
        <button disabled={!newKey || !!keyProblem} onClick={create}>Create flag</button>
      </div>

      <div className="toolbar">
        <label className="field">
          <span>flagKey contains</span>
          <input value={flagKey} onChange={(e) => { setFlagKey(e.target.value); setOffset(0) }} placeholder="all" />
        </label>
        <button className="ghost" onClick={reload}>Refresh</button>
      </div>

      {error ? <Empty>Could not load: {error.message}</Empty>
        : loading && rows.length === 0 ? <Loading what="flags" />
          : rows.length === 0 ? <Empty>No flags yet. Create the first one above.</Empty>
            : (
              <table className="grid">
                <thead><tr><th>id</th><th>flagKey</th><th>isActive</th><th>updated</th><th /></tr></thead>
                <tbody>
                  {rows.map((row) => (
                    <tr key={row.id}>
                      <td>{row.id}</td>
                      <td>{row.flagKey}</td>
                      <td className={row.isActive ? 'on' : 'off'}>{row.isActive}</td>
                      <td className="t">{fmtTime(row.updatedAt)}</td>
                      <td><button className="ghost danger-text" onClick={() => remove(row)}>Delete</button></td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}

      <Pager limit={limit} offset={offset} count={rows.length} onOffset={setOffset} onLimit={setLimit} />
    </div>
  )
}

// countValues asks how many rows the cascade would take, so the confirmation
// states a number instead of a vague warning. A failure here is not fatal — the
// dialog falls back to the wording without a count.
async function countValues(flagKey) {
  try {
    const { data } = await api.listFlagValues({ flagKey, limit: 200 })
    return (data ?? []).filter((r) => r.flagKey === flagKey).length
  } catch {
    return null
  }
}
