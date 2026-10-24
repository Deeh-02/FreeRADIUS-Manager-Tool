import test from 'node:test'
import assert from 'node:assert/strict'
import { serverDate } from './time.js'

// Run with: TZ=Africa/Nairobi node --test src/utils/time.test.js
const wall = (d) => [d.getFullYear(), d.getMonth() + 1, d.getDate(), d.getHours(), d.getMinutes()].join(' ')

test('a time the database stored without a zone is shown as the wall-clock time it is', () => {
  // The backend sends it as ...Z; reading that as UTC showed 04:08 for a payment made at 01:08.
  assert.equal(wall(serverDate('2026-10-24T01:08:53Z')), '2026 10 24 1 8')
  assert.equal(wall(serverDate('2026-10-24T01:08:53.123456Z')), '2026 10 24 1 8')
})

test('a time with an explicit offset keeps it', () => {
  const d = serverDate('2026-10-24T09:00:00+03:00')
  assert.equal(d.toISOString(), '2026-10-24T06:00:00.000Z')
})

test('a plain date stays that date', () => {
  assert.equal(wall(serverDate('2026-10-24')), '2026 10 24 0 0')
})

test('a Date passes through', () => {
  const d = new Date()
  assert.equal(serverDate(d), d)
})
