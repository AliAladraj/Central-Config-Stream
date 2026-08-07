// Drift detection: the database is the source of truth, the consumer's cache is
// what the fleet actually runs on. Everything between them — the KV publish,
// the JetStream stream, the watcher — can fail quietly, and the whole point of
// this control plane is that it does not. Nothing else in the console compares
// the two, so this is the check that catches the failure the system exists to
// prevent.
//
// The console watches ONE environment (the consumer's own), so the comparison
// is only meaningful there. Rows in other environments are counted as
// out-of-scope, never as drift — reporting them would be a false alarm.

const same = (a, b) => JSON.stringify(a) === JSON.stringify(b)

export function computeDrift({ watchedEnvId, snapshot, flagValues, configs, localization }) {
  const issues = []
  let compared = 0
  let outOfScope = 0

  // ── flags: KV key {env}.{flagKey}, cache keyed by flagKey
  const cacheFlags = snapshot.flags ?? {}
  const dbFlags = new Map()
  for (const row of flagValues) {
    if (row.environmentId !== watchedEnvId) { outOfScope++; continue }
    dbFlags.set(row.flagKey, row)
  }
  for (const [flagKey, row] of dbFlags) {
    compared++
    const cached = cacheFlags[flagKey]
    if (!cached) {
      issues.push(mk('flags', flagKey, 'missing-in-cache', row.id,
        `database has ${flagKey}=${row.value} (enabled ${row.enabled}) but the consumer has never received it`))
      continue
    }
    const dbEnabled = Boolean(Number(row.enabled))
    if (cached.enabled !== dbEnabled || cached.value !== row.value) {
      issues.push(mk('flags', flagKey, 'value-differs', row.id,
        `database ${row.value} / enabled ${dbEnabled} · consumer ${cached.value} / enabled ${cached.enabled}`))
    }
  }
  for (const flagKey of Object.keys(cacheFlags)) {
    if (!dbFlags.has(flagKey)) {
      issues.push(mk('flags', flagKey, 'missing-in-database', null,
        'the consumer is serving a flag with no row behind it — the KV key outlived its row'))
    }
  }

  // ── appsettings: KV key {env}.{microserviceId}
  const cachedSettings = snapshot.serviceSettings ?? {}
  const dbSettings = new Map()
  for (const row of configs) {
    if (row.environmentId !== watchedEnvId) { outOfScope++; continue }
    dbSettings.set(String(row.microserviceId), row)
  }
  for (const [msId, row] of dbSettings) {
    compared++
    const cached = cachedSettings[msId]
    if (cached === undefined) {
      issues.push(mk('servicesettings', msId, 'missing-in-cache', row.id,
        `microservice ${msId} has appsettings in the database that the consumer has not received`))
    } else if (!same(cached, row.settingsJson)) {
      issues.push(mk('servicesettings', msId, 'value-differs', row.id,
        'the cached appsettings tree differs from the stored one'))
    }
  }
  for (const msId of Object.keys(cachedSettings)) {
    if (!dbSettings.has(msId)) {
      issues.push(mk('servicesettings', msId, 'missing-in-database', null,
        'the consumer holds appsettings for a microservice with no row in this environment'))
    }
  }

  // ── localization: KV key {env}.{microserviceId}.{locale}
  const cacheLoc = snapshot.localization ?? {}
  const dbLoc = new Map()
  for (const row of localization) {
    if (row.environmentId !== watchedEnvId) { outOfScope++; continue }
    dbLoc.set(`${row.microserviceId}.${row.locale}`, row)
  }
  for (const [key, row] of dbLoc) {
    compared++
    const cached = cacheLoc[String(row.microserviceId)]?.[row.locale]
    if (cached === undefined) {
      issues.push(mk('localization', key, 'missing-in-cache', row.id,
        `the ${row.locale} bundle for microservice ${row.microserviceId} has not reached the consumer`))
    } else if (!same(cached, row.bundleJson)) {
      issues.push(mk('localization', key, 'value-differs', row.id,
        'the cached bundle differs from the stored one'))
    }
  }
  for (const [msId, bundles] of Object.entries(cacheLoc)) {
    for (const locale of Object.keys(bundles)) {
      if (!dbLoc.has(`${msId}.${locale}`)) {
        issues.push(mk('localization', `${msId}.${locale}`, 'missing-in-database', null,
          'the consumer holds a bundle with no row behind it'))
      }
    }
  }

  return { issues, compared, outOfScope, watchedEnvId }
}

function mk(domain, key, kind, id, detail) {
  return { domain, key, kind, id, detail }
}

export const driftLabel = {
  'missing-in-cache': 'not in consumer',
  'missing-in-database': 'not in database',
  'value-differs': 'values differ',
}
