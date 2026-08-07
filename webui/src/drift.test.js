import { describe, expect, it } from 'vitest'
import { computeDrift } from './drift.js'

// computeDrift is the one place the console compares the database with what a
// consumer is actually serving, so these cases pin its semantics rather than
// its shape: what counts as drift, what counts as out of scope, and how a flag
// row's numeric `enabled` lines up with the cache's boolean.

const WATCHED = 1
const OTHER = 2

const base = {
  watchedEnvId: WATCHED,
  snapshot: { environmentId: WATCHED, flags: {}, serviceSettings: {}, localization: {} },
  flagValues: [],
  configs: [],
  localization: [],
}

const flagRow = (over = {}) => ({ id: 10, environmentId: WATCHED, flagKey: 'search_v2', value: 'on', enabled: 1, ...over })
const configRow = (over = {}) => ({ id: 20, environmentId: WATCHED, microserviceId: 7, settingsJson: { db: { pool: 4 } }, ...over })
const locRow = (over = {}) => ({ id: 30, environmentId: WATCHED, microserviceId: 7, locale: 'pt-BR', bundleJson: { hello: 'olá' }, ...over })

describe('computeDrift', () => {
  it('reports nothing when both sides agree', () => {
    const d = computeDrift({
      ...base,
      snapshot: {
        environmentId: WATCHED,
        flags: { search_v2: { value: 'on', enabled: true } },
        serviceSettings: { 7: { db: { pool: 4 } } },
        localization: { 7: { 'pt-BR': { hello: 'olá' } } },
      },
      flagValues: [flagRow()],
      configs: [configRow()],
      localization: [locRow()],
    })
    expect(d.issues).toEqual([])
    expect(d.compared).toBe(3)
    expect(d.outOfScope).toBe(0)
    expect(d.watchedEnvId).toBe(WATCHED)
  })

  it('treats a database row the consumer never received as drift', () => {
    const d = computeDrift({ ...base, flagValues: [flagRow()] })
    expect(d.issues).toHaveLength(1)
    expect(d.issues[0]).toMatchObject({ domain: 'flags', key: 'search_v2', kind: 'missing-in-cache', id: 10 })
    expect(d.compared).toBe(1)
  })

  it('treats a cached key with no row behind it as drift', () => {
    const d = computeDrift({
      ...base,
      snapshot: { ...base.snapshot, flags: { orphan: { value: 'x', enabled: true } } },
    })
    expect(d.issues).toHaveLength(1)
    expect(d.issues[0]).toMatchObject({ domain: 'flags', key: 'orphan', kind: 'missing-in-database', id: null })
    expect(d.compared).toBe(0)
  })

  it('compares the numeric enabled column against the cached boolean', () => {
    const agree = computeDrift({
      ...base,
      snapshot: { ...base.snapshot, flags: { search_v2: { value: 'on', enabled: false } } },
      flagValues: [flagRow({ enabled: 0 })],
    })
    expect(agree.issues).toEqual([])

    const differ = computeDrift({
      ...base,
      snapshot: { ...base.snapshot, flags: { search_v2: { value: 'on', enabled: true } } },
      flagValues: [flagRow({ enabled: 0 })],
    })
    expect(differ.issues).toHaveLength(1)
    expect(differ.issues[0]).toMatchObject({ kind: 'value-differs', key: 'search_v2' })
  })

  it('reports a differing flag value even when the enabled state matches', () => {
    const d = computeDrift({
      ...base,
      snapshot: { ...base.snapshot, flags: { search_v2: { value: 'off', enabled: true } } },
      flagValues: [flagRow()],
    })
    expect(d.issues[0]).toMatchObject({ kind: 'value-differs' })
  })

  it('compares appsettings trees by value, not by identity', () => {
    const same = computeDrift({
      ...base,
      snapshot: { ...base.snapshot, serviceSettings: { 7: { db: { pool: 4 } } } },
      configs: [configRow()],
    })
    expect(same.issues).toEqual([])

    const nested = computeDrift({
      ...base,
      snapshot: { ...base.snapshot, serviceSettings: { 7: { db: { pool: 8 } } } },
      configs: [configRow()],
    })
    expect(nested.issues).toHaveLength(1)
    expect(nested.issues[0]).toMatchObject({ domain: 'servicesettings', key: '7', kind: 'value-differs' })
  })

  it('does not mistake an empty cached settings object for a missing one', () => {
    const d = computeDrift({
      ...base,
      snapshot: { ...base.snapshot, serviceSettings: { 7: {} } },
      configs: [configRow({ settingsJson: {} })],
    })
    expect(d.issues).toEqual([])
    expect(d.compared).toBe(1)
  })

  it('compares localization per locale rather than per service', () => {
    const d = computeDrift({
      ...base,
      snapshot: {
        ...base.snapshot,
        localization: { 7: { 'pt-BR': { hello: 'olá' }, 'fr-FR': { hello: 'salut' } } },
      },
      localization: [locRow(), locRow({ id: 31, locale: 'de-DE', bundleJson: { hello: 'hallo' } })],
    })
    // pt-BR matches, de-DE never reached the consumer, fr-FR has no row behind it.
    expect(d.compared).toBe(2)
    expect(d.issues).toEqual([
      expect.objectContaining({ domain: 'localization', key: '7.de-DE', kind: 'missing-in-cache' }),
      expect.objectContaining({ domain: 'localization', key: '7.fr-FR', kind: 'missing-in-database' }),
    ])
  })

  it('reports a bundle whose strings differ', () => {
    const d = computeDrift({
      ...base,
      snapshot: { ...base.snapshot, localization: { 7: { 'pt-BR': { hello: 'oi' } } } },
      localization: [locRow()],
    })
    expect(d.issues[0]).toMatchObject({ key: '7.pt-BR', kind: 'value-differs' })
  })

  it('counts rows in other environments as out of scope, never as drift', () => {
    const d = computeDrift({
      ...base,
      flagValues: [flagRow({ id: 11, environmentId: OTHER, flagKey: 'only_in_staging' })],
      configs: [configRow({ id: 21, environmentId: OTHER })],
      localization: [locRow({ id: 31, environmentId: OTHER })],
    })
    expect(d.issues).toEqual([])
    expect(d.outOfScope).toBe(3)
    expect(d.compared).toBe(0)
  })

  it('does not let another environment shadow a watched row', () => {
    const d = computeDrift({
      ...base,
      snapshot: { ...base.snapshot, flags: { search_v2: { value: 'on', enabled: true } } },
      flagValues: [flagRow(), flagRow({ id: 11, environmentId: OTHER, value: 'off', enabled: 0 })],
    })
    expect(d.issues).toEqual([])
    expect(d.compared).toBe(1)
    expect(d.outOfScope).toBe(1)
  })

  it('survives a snapshot that has not been populated yet', () => {
    const d = computeDrift({ ...base, snapshot: { environmentId: WATCHED }, flagValues: [flagRow()] })
    expect(d.issues).toHaveLength(1)
    expect(d.issues[0].kind).toBe('missing-in-cache')
  })
})
