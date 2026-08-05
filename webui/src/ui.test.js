import { describe, expect, it } from 'vitest'
import { diffPaths } from './ui.jsx'

// diffPaths is what an operator reads before sending an appsettings write, so
// what it does at the edges — arrays, nulls, a replaced subtree — is part of
// the contract, not an implementation detail.

describe('diffPaths', () => {
  it('reports nothing for identical trees', () => {
    expect(diffPaths({ a: 1, b: { c: 2 } }, { a: 1, b: { c: 2 } })).toEqual([])
  })

  it('ignores key order', () => {
    expect(diffPaths({ a: 1, b: 2 }, { b: 2, a: 1 })).toEqual([])
  })

  it('names an added key and carries its new value', () => {
    expect(diffPaths({ a: 1 }, { a: 1, b: 2 })).toEqual([{ kind: 'added', path: 'b', to: 2 }])
  })

  it('names a removed key and carries its old value', () => {
    expect(diffPaths({ a: 1, b: 2 }, { a: 1 })).toEqual([{ kind: 'removed', path: 'b', from: 2 }])
  })

  it('reports a changed leaf with both sides', () => {
    expect(diffPaths({ a: 1 }, { a: 2 })).toEqual([{ kind: 'changed', path: 'a', from: 1, to: 2 }])
  })

  it('walks into nested objects and reports the dotted path', () => {
    const changes = diffPaths(
      { db: { pool: 4, host: 'a' } },
      { db: { pool: 8, host: 'a' } },
    )
    expect(changes).toEqual([{ kind: 'changed', path: 'db.pool', from: 4, to: 8 }])
  })

  it('reports paths in sorted order', () => {
    const changes = diffPaths({ z: 1, a: 1 }, { z: 2, a: 2 })
    expect(changes.map((c) => c.path)).toEqual(['a', 'z'])
  })

  it('treats an array as one value rather than diffing per index', () => {
    expect(diffPaths({ hosts: ['a', 'b'] }, { hosts: ['a', 'c'] })).toEqual([
      { kind: 'changed', path: 'hosts', from: ['a', 'b'], to: ['a', 'c'] },
    ])
    expect(diffPaths({ hosts: ['a'] }, { hosts: ['a'] })).toEqual([])
  })

  it('treats a subtree replaced by a scalar as one change', () => {
    expect(diffPaths({ db: { pool: 4 } }, { db: 'off' })).toEqual([
      { kind: 'changed', path: 'db', from: { pool: 4 }, to: 'off' },
    ])
  })

  it('does not walk into null as if it were an object', () => {
    expect(diffPaths({ db: null }, { db: { pool: 4 } })).toEqual([
      { kind: 'changed', path: 'db', from: null, to: { pool: 4 } },
    ])
    expect(diffPaths({ db: null }, { db: null })).toEqual([])
  })

  it('distinguishes a key set to undefined from a key that is absent', () => {
    expect(diffPaths({}, { a: undefined })).toEqual([{ kind: 'added', path: 'a', to: undefined }])
  })

  it('labels a change at the top level as the root', () => {
    expect(diffPaths(1, 2)).toEqual([{ kind: 'changed', path: '(root)', from: 1, to: 2 }])
  })

  it('reports every changed key of a multi-key edit', () => {
    const changes = diffPaths(
      { a: 1, keep: true, gone: 'x', nested: { deep: { v: 1 } } },
      { a: 2, keep: true, added: 'y', nested: { deep: { v: 1, extra: 2 } } },
    )
    expect(changes).toEqual([
      { kind: 'changed', path: 'a', from: 1, to: 2 },
      { kind: 'added', path: 'added', to: 'y' },
      { kind: 'removed', path: 'gone', from: 'x' },
      { kind: 'added', path: 'nested.deep.extra', to: 2 },
    ])
  })
})
