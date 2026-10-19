// Money display helpers. The API never adds amounts in different currencies together; totals
// arrive as a list of { currency, amount, count }.

export function money(amount, currency = '') {
  const n = Number(amount || 0).toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 })
  return currency ? `${currency} ${n}` : n
}

// formatTotals turns [{currency, amount}] into "KES 1,200.00 · USD 30.00" ("—" when empty).
export function formatTotals(list) {
  if (!Array.isArray(list) || !list.length) return '—'
  return list.map((t) => money(t.amount, t.currency)).join(' · ')
}

export function apiError(err, fallback = 'Request failed') {
  return err?.response?.data?.error || err?.message || fallback
}
