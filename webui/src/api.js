// Thin wrappers over the test console's HTTP surface. The console proxies
// admin calls to central-config, so the browser never holds the admin token.
//
// Every call funnels through request(), so the server's own error message
// reaches the screen exactly once and is never replaced by a generic one.

export class ApiError extends Error {
  constructor(status, message, { retryAfter = null, requestId = null } = {}) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.retryAfter = retryAfter
    this.requestId = requestId
  }

  // explain turns a status into the sentence an operator needs. The server
  // message always comes with it — this adds the context the message assumes
  // you already have.
  get explanation() {
    switch (this.status) {
      case 400: return 'The server rejected the request body. Fix the highlighted field and try again.'
      case 401: return 'The console sent no admin token, or one the server does not know. Check ADMIN_TOKEN on the console process.'
      case 403: return 'The console’s token is not scoped to this environment. It can read every environment but may only write to the ones its scope lists — ask for a wider token or make this change from an environment it covers.'
      case 404: return 'That row no longer exists. Someone may have deleted it since this list was loaded.'
      case 409: return 'The write collided with another one. Either the row was changed since you loaded it, or a row with these keys already exists.'
      case 429: return this.retryAfter
        ? `The write rate limit is in effect. Retry in ${this.retryAfter}s.`
        : 'The write rate limit is in effect. Wait a moment and retry.'
      default: return this.status >= 500
        ? 'The server failed to handle the request. Its log has the detail; the response deliberately does not.'
        : ''
    }
  }
}

async function request(method, path, body) {
  const started = performance.now()
  let r
  try {
    r = await fetch('/api/admin' + path, {
      method,
      headers: body === undefined ? undefined : { 'Content-Type': 'application/json' },
      body: body === undefined ? undefined : JSON.stringify(body),
    })
  } catch (err) {
    throw new ApiError(0, `Could not reach the console: ${err.message}`)
  }

  const elapsedMs = Math.round(performance.now() - started)
  const requestId = r.headers.get('X-Request-Id')
  const text = await r.text()

  if (!r.ok) {
    let message = text.trim()
    try {
      const parsed = JSON.parse(text)
      if (parsed && typeof parsed.error === 'string') message = parsed.error
    } catch { /* not JSON — the raw body is the best message available */ }
    const retryAfterRaw = r.headers.get('Retry-After')
    throw new ApiError(r.status, message || `HTTP ${r.status}`, {
      retryAfter: retryAfterRaw ? Number(retryAfterRaw) : null,
      requestId,
    })
  }

  const data = text ? JSON.parse(text) : null
  // elapsedMs is the full round trip, which already includes the JetStream KV
  // publish — the handler publishes before it responds.
  return { data, status: r.status, elapsedMs, requestId }
}

const qs = (params) => {
  const q = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) {
    if (v !== '' && v !== null && v !== undefined) q.set(k, String(v))
  }
  const s = q.toString()
  return s ? `?${s}` : ''
}

// ── reads ───────────────────────────────────────────────────────────────────

export const listEnvironments = (p = {}) => request('GET', `/environments${qs(p)}`)
export const listMicroservices = (p = {}) => request('GET', `/microservices${qs(p)}`)
export const listFlags = (p = {}) => request('GET', `/flags${qs(p)}`)
export const getFlag = (id) => request('GET', `/flags/${id}`)
export const listFlagValues = (p = {}) => request('GET', `/flags/values${qs(p)}`)
export const getFlagValue = (id) => request('GET', `/flags/values/${id}`)
export const listConfigs = (p = {}) => request('GET', `/configs/values${qs(p)}`)
export const getConfig = (id) => request('GET', `/configs/values/${id}`)
export const getMicroservice = (id) => request('GET', `/configs/${id}`)
export const listLocalization = (p = {}) => request('GET', `/localization${qs(p)}`)
export const getLocalization = (id) => request('GET', `/localization/${id}`)
export const lookupLocalization = (msId, envId, locale) =>
  request('GET', `/localization/lookup/${msId}/${envId}/${encodeURIComponent(locale)}`)
export const listAudit = (p = {}) => request('GET', `/audit${qs(p)}`)
export const getInventory = () => request('GET', '/inventory')
export const getHealth = () => request('GET', '/health')

// /metrics is Prometheus text, not JSON, so it skips request()'s JSON parse.
export async function getMetrics() {
  const r = await fetch('/api/admin/metrics')
  const text = await r.text()
  if (!r.ok) throw new ApiError(r.status, text || `HTTP ${r.status}`)
  return text
}

// ── writes ──────────────────────────────────────────────────────────────────

export const createEnvironment = (body) => request('POST', '/environments', body)
export const deleteEnvironment = (id) => request('DELETE', `/environments/${id}`)

export const createMicroservice = (body) => request('POST', '/microservices', body)
export const deleteMicroservice = (id) => request('DELETE', `/microservices/${id}`)

export const createFlag = (body) => request('POST', '/flags', body)
export const deleteFlag = (id) => request('DELETE', `/flags/${id}`)

export const createFlagValue = (body) => request('POST', '/flags/values', body)
export const updateFlagValue = (body) => request('PUT', '/flags/values', body)
export const deleteFlagValue = (id) => request('DELETE', `/flags/values/${id}`)

export const createConfig = (body) => request('POST', '/configs/values', body)
export const updateConfig = (body) => request('PUT', '/configs/values', body)
export const deleteConfig = (id) => request('DELETE', `/configs/values/${id}`)

export const createLocalization = (body) => request('POST', '/localization', body)
export const updateLocalization = (body) => request('PUT', '/localization/values', body)
export const deleteLocalization = (id) => request('DELETE', `/localization/${id}`)

// ── console-local ───────────────────────────────────────────────────────────

export async function getState() {
  const r = await fetch('/api/state')
  if (!r.ok) throw new Error(`state: HTTP ${r.status}`)
  return r.json()
}

// ── KV key layout, mirrored from the consumer ───────────────────────────────
// Showing the key a row maps to is the fastest way to tell whether a push you
// just saw belongs to the row you just edited.

export const flagKvKey = (envId, flagKey) => `${envId}.${flagKey}`
export const microKvKey = (envId, msId) => `${envId}.${msId}`
export const localeKvKey = (envId, msId, locale) => `${envId}.${msId}.${locale}`
