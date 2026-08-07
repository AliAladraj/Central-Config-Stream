import { useState } from 'react'
import * as api from '../api.js'
import {
  Banner, Empty, Loading, PAGE_SIZE, PageHeader, Pager, Refresh, ScopeNote,
  fmtTime, useList,
} from '../ui.jsx'

// Environments and microservices are the tables every other domain keys off.
// Neither can be renamed through the API — they are create and delete only —
// and a delete is refused with 409 while anything still references the row.
//
// This is the one view the header environment switcher does not reach, and it
// cannot: an environment row is what the switcher is choosing between, and a
// microservice belongs to no environment at all. That is stated in each panel
// rather than left to be inferred from the switcher having no effect.

export default function Reference({ ctx }) {
  return (
    <>
      <PageHeader
        title="Environments & services"
        subtitle="The two tables every flag, appsettings row and bundle keys off. Create and delete only — neither can be renamed through the API."
      />
      <div className="two-up">
        <Environments ctx={ctx} />
        <Microservices ctx={ctx} />
      </div>
    </>
  )
}

// NewRow is the create form every table in this console reveals from its own
// "New …" button. Keeping it in one place is what stops the three views that
// each invented their own from drifting apart again.
function NewRow({ open, onClose, label, placeholder, value, onValue, onCreate, note }) {
  if (!open) return null
  return (
    <div className="new-row">
      <div className="toolbar">
        <label className="field">
          <span>{label}</span>
          <input
            autoFocus
            value={value}
            onChange={(e) => onValue(e.target.value)}
            placeholder={placeholder}
            onKeyDown={(e) => { if (e.key === 'Enter' && value) onCreate() }}
          />
        </label>
        <button disabled={!value} onClick={onCreate}>Create</button>
        <button className="ghost" onClick={onClose}>Cancel</button>
      </div>
      <p className="hint">{note}</p>
    </div>
  )
}

function Environments({ ctx }) {
  const { refs, write, confirm } = ctx
  const [name, setName] = useState('')
  const [creating, setCreating] = useState(false)
  const [limit, setLimit] = useState(PAGE_SIZE)
  const [offset, setOffset] = useState(0)
  const { rows, loading, error, reload } = useList(
    () => api.listEnvironments({ limit, offset }), [limit, offset],
  )

  const create = async () => {
    const r = await write('POST /environments', () => api.createEnvironment({ name }))
    if (r.ok) { setName(''); setCreating(false); reload(); refs.reload() }
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
      <h3 className="panel-title">
        Environments
        <button className="ghost mini" onClick={() => setCreating((c) => !c)}>New environment</button>
      </h3>
      <div className="toolbar">
        <Refresh onClick={reload} />
        <ScopeNote global reason="an environment row is what the header switcher chooses between" />
      </div>
      <NewRow
        open={creating}
        onClose={() => { setCreating(false); setName('') }}
        label="name"
        placeholder="canary"
        value={name}
        onValue={setName}
        onCreate={create}
        note="A new environment changes the set every token scope is expressed in, so this needs a full-scope token."
      />

      {error ? <Banner problem={{ action: 'Load environments', error }} />
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
  const [creating, setCreating] = useState(false)
  const [limit, setLimit] = useState(PAGE_SIZE)
  const [offset, setOffset] = useState(0)
  const { rows, loading, error, reload } = useList(
    () => api.listMicroservices({ limit, offset }), [limit, offset],
  )

  const create = async () => {
    const r = await write('POST /microservices', () => api.createMicroservice({ name }))
    if (r.ok) { setName(''); setCreating(false); reload(); refs.reload() }
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
      <h3 className="panel-title">
        Microservices
        <button className="ghost mini" onClick={() => setCreating((c) => !c)}>New microservice</button>
      </h3>
      <div className="toolbar">
        <Refresh onClick={reload} />
        <ScopeNote global reason="a microservice belongs to no environment" />
      </div>
      <NewRow
        open={creating}
        onClose={() => { setCreating(false); setName('') }}
        label="name"
        placeholder="catalog-api"
        value={name}
        onValue={setName}
        onCreate={create}
        note="A microservice belongs to no environment; its appsettings and bundles are per environment."
      />

      {error ? <Banner problem={{ action: 'Load microservices', error }} />
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
