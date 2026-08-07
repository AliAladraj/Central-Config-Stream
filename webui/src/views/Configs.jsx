import { useCallback, useEffect, useState } from 'react'
import * as api from '../api.js'
import {
  Banner, Empty, JsonEditor, KvKey, Loading, PAGE_SIZE, PageHeader, Pager,
  Refresh, ScopeNote, fmtTime, jsonValue, useList, useResetOnChange,
} from '../ui.jsx'

// Appsettings are the highest-consequence rows in the system: one bad tree
// breaks every instance of that service at once. So the editor validates as you
// type and shows the diff against the row as loaded before anything is sent.

export default function Configs({ ctx }) {
  const { envId, refs, write, confirm, snapshot } = ctx
  const [msId, setMsId] = useState('')
  const [limit, setLimit] = useState(PAGE_SIZE)
  const [offset, setOffset] = useState(0)
  const [selected, setSelected] = useState(null)
  const [creating, setCreating] = useState(false)

  useResetOnChange(envId, () => setOffset(0))

  const { rows, loading, error, reload } = useList(
    () => api.listConfigs({ microserviceId: msId, environmentId: envId, limit, offset }),
    [msId, envId, limit, offset],
  )

  const remove = async (row) => {
    const ok = await confirm({
      title: `Delete appsettings for ${refs.svcName(row.microserviceId)} in ${refs.envName(row.environmentId)}?`,
      body: 'This removes the whole settings tree for that service in that environment.',
      consequences: [
        `The KV key ${row.environmentId}.${row.microserviceId} is purged, so instances of that service stop receiving configuration.`,
        'Running instances keep the copy already in memory; instances that restart come up with no configuration from the control plane.',
      ],
      target: `appsettings id ${row.id}`,
      confirmLabel: 'Delete appsettings',
    })
    if (!ok) return
    const r = await write(`DELETE /configs/values/${row.id}`, () => api.deleteConfig(row.id))
    if (r.ok) { setSelected(null); reload() }
  }

  return (
    <>
      <PageHeader
        title="Appsettings"
        subtitle="The per-service JSON tree, one row per service per environment. One bad tree breaks every instance of that service at once, so the editor validates as you type and diffs before it sends."
        action={<button className="ghost" onClick={() => { setCreating(true); setSelected(null) }}>New appsettings row</button>}
      />

      <div className="panel">
        <div className="toolbar">
          <label className="field">
            <span>microservice</span>
            <select value={msId} onChange={(e) => { setMsId(e.target.value); setOffset(0) }}>
              <option value="">All services</option>
              {refs.microservices.map((m) => <option key={m.id} value={m.id}>{m.name} (id {m.id})</option>)}
            </select>
          </label>
          <Refresh onClick={reload} />
          <ScopeNote envId={envId} envName={refs.envName} />
        </div>

        {error ? <Banner problem={{ action: 'Load appsettings rows', error }} />
          : loading && rows.length === 0 ? <Loading what="appsettings" />
            : rows.length === 0 ? <Empty>No appsettings rows match these filters.</Empty>
              : (
                <div className="scroll-x">
                  <table className="grid">
                    <thead>
                      <tr><th>id</th><th>service</th><th>env</th><th>kv key</th><th>keys</th><th>updated</th><th /></tr>
                    </thead>
                    <tbody>
                      {rows.map((row) => (
                        <tr key={row.id} className={selected?.id === row.id ? 'sel' : undefined}>
                          <td>{row.id}</td>
                          <td>{refs.svcName(row.microserviceId)}</td>
                          <td>{refs.envName(row.environmentId)}</td>
                          <td><KvKey bucket="MICROCONFIG" envId={row.environmentId} rest={row.microserviceId} /></td>
                          <td className="t">{Object.keys(row.settingsJson ?? {}).length} top-level</td>
                          <td className="t">{fmtTime(row.updatedAt)}</td>
                          <td className="right">
                            <button className="ghost" onClick={() => { setCreating(false); setSelected(row) }}>Edit</button>
                            <button className="ghost danger-text" onClick={() => remove(row)}>Delete</button>
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              )}

        <Pager limit={limit} offset={offset} count={rows.length} onOffset={setOffset} onLimit={setLimit} />
      </div>

      {creating && (
        <CreateConfig ctx={ctx} onClose={() => setCreating(false)} onCreated={() => { setCreating(false); reload() }} />
      )}
      {selected && (
        <EditConfig
          ctx={ctx} row={selected} snapshot={snapshot}
          onClose={() => setSelected(null)}
          onSaved={() => { setSelected(null); reload() }}
        />
      )}
    </>
  )
}

function CreateConfig({ ctx, onClose, onCreated }) {
  const { refs, write, envId } = ctx
  const [microserviceId, setMicroserviceId] = useState('')
  const [environmentId, setEnvironmentId] = useState(envId || '')
  const [text, setText] = useState('{\n  \n}')

  const ready = microserviceId && environmentId && isJsonObject(text)

  const create = async () => {
    const r = await write('POST /configs/values', () => api.createConfig({
      microserviceId: Number(microserviceId),
      environmentId: Number(environmentId),
      settingsJson: jsonValue(text),
    }))
    if (r.ok) onCreated()
  }

  return (
    <div className="editor">
      <div className="editor-head">
        <strong>New appsettings row</strong>
        <button className="ghost close" onClick={onClose} aria-label="Close">×</button>
      </div>
      <div className="row">
        <label className="field">
          <span>microservice</span>
          <select value={microserviceId} onChange={(e) => setMicroserviceId(e.target.value)}>
            <option value="">choose…</option>
            {refs.microservices.map((m) => <option key={m.id} value={m.id}>{m.name} (id {m.id})</option>)}
          </select>
        </label>
        <label className="field">
          <span>environment</span>
          <select value={environmentId} onChange={(e) => setEnvironmentId(e.target.value)}>
            <option value="">choose…</option>
            {refs.environments.map((e) => <option key={e.id} value={e.id}>{e.name} (id {e.id})</option>)}
          </select>
        </label>
      </div>
      {microserviceId && environmentId && (
        <p className="hint">Publishes to <KvKey bucket="MICROCONFIG" envId={environmentId} rest={microserviceId} /></p>
      )}
      <JsonEditor label="settingsJson" text={text} onText={setText} />
      <div className="actions">
        <button disabled={!ready} onClick={create}>Create appsettings</button>
      </div>
    </div>
  )
}

function EditConfig({ ctx, row: initial, snapshot, onClose, onSaved }) {
  const { refs, write } = ctx
  const [row, setRow] = useState(initial)
  const [text, setText] = useState(JSON.stringify(initial.settingsJson, null, 2))
  const [conflict, setConflict] = useState(false)

  useEffect(() => {
    setRow(initial)
    setText(JSON.stringify(initial.settingsJson, null, 2))
    setConflict(false)
  }, [initial])

  const refresh = useCallback(async () => {
    try {
      const { data } = await api.getConfig(row.id)
      setRow(data)
      setText(JSON.stringify(data.settingsJson, null, 2))
      setConflict(false)
    } catch { /* the banner already carries the failure */ }
  }, [row.id])

  const save = async () => {
    const r = await write(`PUT /configs/values (id ${row.id})`, () => api.updateConfig({
      id: row.id,
      microserviceId: row.microserviceId,
      environmentId: row.environmentId,
      settingsJson: jsonValue(text),
      expectedUpdatedAt: row.updatedAt,
    }))
    if (r.ok) onSaved()
    else if (r.error?.status === 409) setConflict(true)
  }

  return (
    <div className="editor">
      <div className="editor-head">
        <div>
          <strong>{refs.svcName(row.microserviceId)}</strong> in <strong>{refs.envName(row.environmentId)}</strong>
          <div className="t">appsettings id {row.id} · loaded at {fmtTime(row.updatedAt)}</div>
        </div>
        <KvKey bucket="MICROCONFIG" envId={row.environmentId} rest={row.microserviceId} />
        <button className="ghost close" onClick={onClose} aria-label="Close">×</button>
      </div>

      {row.environmentId !== snapshot.environmentId && (
        <p className="hint">
          This console watches environment {snapshot.environmentId}, so this write will not appear in the live panel.
        </p>
      )}

      <JsonEditor label="settingsJson" text={text} onText={setText} original={row.settingsJson} />

      {conflict && (
        <div className="conflict">
          Someone else saved this row after you loaded it. Your write was refused rather than
          overwriting their change — reload, re-apply your edit, and save again.
          <button className="ghost" onClick={refresh}>Reload this row</button>
        </div>
      )}

      <div className="actions">
        <button disabled={!isJsonObject(text)} onClick={save}>Save appsettings</button>
      </div>
    </div>
  )
}

function isJsonObject(text) {
  try { jsonValue(text); return true } catch { return false }
}
