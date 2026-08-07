import { describe, expect, it } from 'vitest'
import { applyEvent, normalize } from './useKvStream.js'

// These two functions are the seam between the server's JSON and what the
// panels render, and nothing rendered them in a test — so renaming the field
// on one side of that seam and not the other produced a snapshot whose
// serviceSettings key was undefined, and the console crashed on first paint
// with "Cannot convert undefined or null to object". The names on both sides
// are asserted here rather than left to a manual look at the running app.

const EMPTY = { environmentId: null, flags: {}, serviceSettings: {}, localization: {} }

describe('normalize', () => {
  it('names every bucket the panels read, even from an empty body', () => {
    expect(normalize({})).toEqual(EMPTY)
  })

  it('carries service settings through under the name the server sends', () => {
    const got = normalize({ environmentId: 1, serviceSettings: { 7: { db: { pool: 4 } } } })
    expect(got.serviceSettings).toEqual({ 7: { db: { pool: 4 } } })
    expect(got.environmentId).toBe(1)
  })

  it('does not resurrect the pre-rename field name', () => {
    // A server still sending `micro` is a version mismatch, not a value to
    // silently accept — accepting it would hide exactly that mismatch.
    const got = normalize({ micro: { 7: { db: { pool: 4 } } } })
    expect(got.serviceSettings).toEqual({})
    expect(got.micro).toBeUndefined()
  })
})

describe('applyEvent', () => {
  const base = { ...EMPTY, environmentId: 1 }

  it('applies a SERVICESETTINGS push to the service settings map', () => {
    const got = applyEvent(base, { bucket: 'SERVICESETTINGS', key: '1.7', value: { db: { pool: 8 } } })
    expect(got.serviceSettings).toEqual({ 7: { db: { pool: 8 } } })
  })

  it('removes the key when the push is a delete', () => {
    const seeded = { ...base, serviceSettings: { 7: { db: { pool: 8 } } } }
    const got = applyEvent(seeded, { bucket: 'SERVICESETTINGS', key: '1.7', value: null })
    expect(got.serviceSettings).toEqual({})
  })

  it('leaves the snapshot alone for a bucket it does not know', () => {
    expect(applyEvent(base, { bucket: 'MICROCONFIG', key: '1.7', value: {} })).toBe(base)
  })
})
