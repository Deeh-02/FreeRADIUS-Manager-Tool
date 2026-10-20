// A one-time ID for one "record payment" attempt. The server remembers it: if the request is
// sent again (timeout, double click, flaky network) the original receipt comes back instead of
// a second payment being recorded.
//
// crypto.randomUUID() exists only on HTTPS and localhost; the admin panel is often opened over
// plain HTTP on a LAN, so fall back to getRandomValues, which works everywhere.
export function newIdempotencyKey() {
  const c = globalThis.crypto
  if (c && typeof c.randomUUID === 'function') return c.randomUUID()
  const bytes = new Uint8Array(16)
  if (c && typeof c.getRandomValues === 'function') c.getRandomValues(bytes)
  else for (let i = 0; i < bytes.length; i++) bytes[i] = Math.floor(Math.random() * 256)
  return Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('')
}
