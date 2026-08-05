import { useState } from 'react'
import * as api from '../api.js'
import { Empty, Loading, Pager, fmtTime, useList } from '../ui.jsx'

// Environments and microservices are the tables every other domain keys off.
// Neither can be renamed through the API — they are create and delete only —
// and a delete is refused with 409 while anything still references the row.

export default function Reference({ ctx }) {
  return (
    <>
      <div className="view-head"><h2>Environments &amp; services</h2></div>
      <div className="two-up">
        <Environments ctx={ctx} />
        <Microservices ctx={ctx} />
      </div>
    </>
  )
}

function Environments({ ctx }) {
  const { refs, write, confirm } = ctx
  const [name, setName] = useState('')
  const [limit, setLimit] = useState(25)
  const [offset, setOffset] = useState(0)
  const { rows, loading, error, reload } = useList(
    () => api.listEnvironments({ limit, offset }), [limit, offset],
  )

  const create = async () => {
    const r = await write('POST /environments', () => api.createEnvironment({ name }))
    if (r.ok) { setName(''); reload(); refs.reload() }
  }

  const remove = async (row) => {
    const ok = await confirm({
      title: `Delete the environment ${row.name}?`,
      body: 'An environment can only be deleted once nothing points at it.',
      consequences: [
        'If any flag value, appsettings row or localization bundle still names this environment, the server refuses with 409 and nothing is deleted.',
        'Delete or move those rows first, then retry.',
      ],
      target: `environment id ${row.id} · ${row.name}`,
      confirmLabel: 'Delete environment',
    })
    if (!ok) return
    const r = await write(`DELETE /environments/${row.id}`, () => api.deleteEnvironment(row.id))
    if (r.ok) { reload(); refs.reload() }
  }

  return (
    <div className="panel">
      <h3 className="panel-title">Environments</h3>
      <div className="toolbar">
        <label className="field">
          <span>name</span>
          <input value={name} onChange={(e) => setName(e.target.value)} placeholder="canary" />
        </label>
        <button disabled={!name} onClick={create}>Create environment</button>
      </div>
      <p className="hint">A new environment changes the set every token scope is expressed in, so this needs a full-scope token.</p>

      {error ? <Empty>Could not load: {error.message}</Empty>
        : loading && rows.length === 0 ? <Loading what="environments" />
          : rows.length === 0 ? <Empty>No environments. Nothing else can be configured until one exists.</Empty>
            : (
              <table className="grid">
                <thead><tr><th>id</th><th>name</th><th>updated</th><th /></tr></thead>
                <tbody>
                  {rows.map((row) => (
                    <tr key={row.id}>
                      <td>{row.id}</td>
                      <td>{row.name}</td>
                      <td className="t">{fmtTime(row.updatedAt)}</td>
                      <td className="right"><button className="ghost danger-text" onClick={() => remove(row)}>Delete</button></td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
      <Pager limit={limit} offset={offset} count={rows.length} onOffset={setOffset} onLimit={setLimit} />
    </div>
  )
}

function Microservices({ ctx }) {
  const { refs, write, confirm } = ctx
  const [name, setName] = useState('')
  const [limit, setLimit] = useState(25)
  const [offset, setOffset] = useState(0)
  const { rows, loading, error, reload } = useList(
    () => api.listMicroservices({ limit, offset }), [limit, offset],
  )

  const create = async () => {
    const r = await write('POST /microservices', () => api.createMicroservice({ name }))
    if (r.ok) { setName(''); reload(); refs.reload() }
  }

  const remove = async (row) => {
    const ok = await confirm({
      title: `Delete the microservice ${row.name}?`,
      body: 'A microservice can only be deleted once nothing points at it.',
      consequences: [
        'If it still has appsettings or localization rows in any environment, the server refuses with 409 and nothing is deleted.',
        'Delete those rows first, then retry.',
      ],
      target: `microservice id ${row.id} · ${row.name}`,
      confirmLabel: 'Delete microservice',
    })
    if (!ok) return
    const r = await write(`DELETE /microservices/${row.id}`, () => api.deleteMicroservice(row.id))
    if (r.ok) { reload(); refs.reload() }
  }

  return (
    <div className="panel">
      <h3 className="panel-title">Microservices</h3>
      <div className="toolbar">
        <label className="field">
          <span>name</span>
          <input value={name} onChange={(e) => setName(e.target.value)} placeholder="catalog-api" />
        </label>
        <button disabled={!name} onClick={create}>Create microservice</button>
      </div>
      <p className="hint">A microservice belongs to no environment; its appsettings and bundles are per environment.</p>

      {error ? <Empty>Could not load: {error.message}</Empty>
        : loading && rows.length === 0 ? <Loading what="microservices" />
          : rows.length === 0 ? <Empty>No microservices yet.</Empty>
            : (
              <table className="grid">
                <thead><tr><th>id</th><th>name</th><th>updated</th><th /></tr></thead>
                <tbody>
                  {rows.map((row) => (
                    <tr key={row.id}>
                      <td>{row.id}</td>
                      <td>{row.name}</td>
                      <td className="t">{fmtTime(row.updatedAt)}</td>
                      <td className="right"><button className="ghost danger-text" onClick={() => remove(row)}>Delete</button></td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
      <Pager limit={limit} offset={offset} count={rows.length} onOffset={setOffset} onLimit={setLimit} />
    </div>
  )
}
