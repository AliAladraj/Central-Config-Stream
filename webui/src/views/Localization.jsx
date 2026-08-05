import { useCallback, useEffect, useState } from 'react'
import * as api from '../api.js'
import { Empty, JsonEditor, KvKey, Loading, Pager, fmtTime, jsonValue, useList } from '../ui.jsx'

// One row is a whole locale bundle for a (service, environment, locale) tuple,
// which is also exactly one KV key — apps load a locale atomically.

export default function Localization({ ctx }) {
  const { envId, refs, write, confirm, snapshot } = ctx
  const [msId, setMsId] = useState('')
  const [locale, setLocale] = useState('')
  const [limit, setLimit] = useState(25)
  const [offset, setOffset] = useState(0)
  const [selected, setSelected] = useState(null)
  const [creating, setCreating] = useState(false)

  const { rows, loading, error, reload } = useList(
    () => api.listLocalization({ microserviceId: msId, environmentId: envId, locale, limit, offset }),
    [msId, envId, locale, limit, offset],
  )
  useEffect(() => { setOffset(0) }, [msId, envId, locale])

  const remove = async (row) => {
    const ok = await confirm({
      title: `Delete the ${row.locale} bundle for ${refs.svcName(row.microserviceId)}?`,
      body: `Removes the whole ${row.locale} bundle in ${refs.envName(row.environmentId)}. Other locales are untouched.`,
      consequences: [
        `The KV key ${row.environmentId}.${row.microserviceId}.${row.locale} is purged.`,
        `Users on ${row.locale} fall back to whatever the service does when a bundle is missing — usually raw keys.`,
      ],
      target: `localization id ${row.id}`,
      confirmLabel: 'Delete bundle',
    })
    if (!ok) return
    const r = await write(`DELETE /localization/${row.id}`, () => api.deleteLocalization(row.id))
    if (r.ok) { setSelected(null); reload() }
  }

  return (
    <>
      <div className="view-head">
        <h2>Localization</h2>
        <button className="ghost" onClick={() => { setCreating(true); setSelected(null) }}>New bundle</button>
      </div>

      <div className="panel">
        <div className="toolbar">
          <label className="field">
            <span>microservice</span>
            <select value={msId} onChange={(e) => setMsId(e.target.value)}>
              <option value="">All services</option>
              {refs.microservices.map((m) => <option key={m.id} value={m.id}>{m.name} (id {m.id})</option>)}
            </select>
          </label>
          <label className="field">
            <span>locale</span>
            <input value={locale} onChange={(e) => setLocale(e.target.value)} placeholder="all" />
          </label>
          <span className="t">environment comes from the header switcher</span>
          <button className="ghost" onClick={reload}>Refresh</button>
        </div>

        {error ? <Empty>Could not load: {error.message}</Empty>
          : loading && rows.length === 0 ? <Loading what="bundles" />
            : rows.length === 0 ? <Empty>No bundles match these filters.</Empty>
              : (
                <div className="scroll-x">
                  <table className="grid">
                    <thead>
                      <tr><th>id</th><th>service</th><th>env</th><th>locale</th><th>kv key</th><th>strings</th><th>updated</th><th /></tr>
                    </thead>
                    <tbody>
                      {rows.map((row) => (
                        <tr key={row.id} className={selected?.id === row.id ? 'sel' : undefined}>
                          <td>{row.id}</td>
                          <td>{refs.svcName(row.microserviceId)}</td>
                          <td>{refs.envName(row.environmentId)}</td>
                          <td>{row.locale}</td>
                          <td><KvKey bucket="LOCALIZATION" envId={row.environmentId} rest={`${row.microserviceId}.${row.locale}`} /></td>
                          <td className="t">{Object.keys(row.bundleJson ?? {}).length}</td>
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

      {creating && <CreateBundle ctx={ctx} onClose={() => setCreating(false)} onCreated={() => { setCreating(false); reload() }} />}
      {selected && (
        <EditBundle
          ctx={ctx} row={selected} snapshot={snapshot}
          onClose={() => setSelected(null)}
          onSaved={() => { setSelected(null); reload() }}
        />
      )}
    </>
  )
}

function CreateBundle({ ctx, onClose, onCreated }) {
  const { refs, write, envId } = ctx
  const [microserviceId, setMicroserviceId] = useState('')
  const [environmentId, setEnvironmentId] = useState(envId || '')
  const [locale, setLocale] = useState('')
  const [text, setText] = useState('{\n  \n}')

  // A dot in the locale would split the KV key, so the server rejects it. Say
  // so here rather than spending a round trip to find out.
  const localeProblem = locale === '' ? null
    : locale.includes('.') ? 'A locale may not contain a dot — the KV key uses dots as separators.'
      : /\s/.test(locale) ? 'A locale may not contain whitespace.'
        : null

  const ready = microserviceId && environmentId && locale && !localeProblem && isJsonObject(text)

  const create = async () => {
    const r = await write('POST /localization', () => api.createLocalization({
      microserviceId: Number(microserviceId),
      environmentId: Number(environmentId),
      locale,
      bundleJson: jsonValue(text),
    }))
    if (r.ok) onCreated()
  }

  return (
    <div className="editor">
      <div className="editor-head">
        <strong>New localization bundle</strong>
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
        <label className="field">
          <span>locale</span>
          <input value={locale} onChange={(e) => setLocale(e.target.value)} placeholder="pt-BR" />
        </label>
      </div>
      {localeProblem && <p className="field-err">{localeProblem}</p>}
      {microserviceId && environmentId && locale && !localeProblem && (
        <p className="hint">Publishes to <KvKey bucket="LOCALIZATION" envId={environmentId} rest={`${microserviceId}.${locale}`} /></p>
      )}
      <JsonEditor label="bundleJson" text={text} onText={setText} />
      <div className="actions">
        <button disabled={!ready} onClick={create}>Create bundle</button>
      </div>
    </div>
  )
}

function EditBundle({ ctx, row: initial, snapshot, onClose, onSaved }) {
  const { refs, write } = ctx
  const [row, setRow] = useState(initial)
  const [text, setText] = useState(JSON.stringify(initial.bundleJson, null, 2))
  const [conflict, setConflict] = useState(false)

  useEffect(() => {
    setRow(initial)
    setText(JSON.stringify(initial.bundleJson, null, 2))
    setConflict(false)
  }, [initial])

  const refresh = useCallback(async () => {
    try {
      const { data } = await api.getLocalization(row.id)
      setRow(data)
      setText(JSON.stringify(data.bundleJson, null, 2))
      setConflict(false)
    } catch { /* the banner already carries the failure */ }
  }, [row.id])

  const save = async () => {
    const r = await write(`PUT /localization/values (id ${row.id})`, () => api.updateLocalization({
      id: row.id,
      microserviceId: row.microserviceId,
      environmentId: row.environmentId,
      locale: row.locale,
      bundleJson: jsonValue(text),
      expectedUpdatedAt: row.updatedAt,
    }))
    if (r.ok) onSaved()
    else if (r.error?.status === 409) setConflict(true)
  }

  return (
    <div className="editor">
      <div className="editor-head">
        <div>
          <strong>{row.locale}</strong> · {refs.svcName(row.microserviceId)} in {refs.envName(row.environmentId)}
          <div className="t">localization id {row.id} · loaded at {fmtTime(row.updatedAt)}</div>
        </div>
        <KvKey bucket="LOCALIZATION" envId={row.environmentId} rest={`${row.microserviceId}.${row.locale}`} />
        <button className="ghost close" onClick={onClose} aria-label="Close">×</button>
      </div>

      {row.environmentId !== snapshot.environmentId && (
        <p className="hint">
          This console watches environment {snapshot.environmentId}, so this write will not appear in the live panel.
        </p>
      )}

      <JsonEditor label="bundleJson" text={text} onText={setText} original={row.bundleJson} />

      {conflict && (
        <div className="conflict">
          Someone else saved this bundle after you loaded it. Your write was refused rather than
          overwriting their change — reload, re-apply your edit, and save again.
          <button className="ghost" onClick={refresh}>Reload this row</button>
        </div>
      )}

      <div className="actions">
        <button disabled={!isJsonObject(text)} onClick={save}>Save bundle</button>
      </div>
    </div>
  )
}

function isJsonObject(text) {
  try { jsonValue(text); return true } catch { return false }
}
