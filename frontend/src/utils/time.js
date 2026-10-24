import { parseISO } from 'date-fns'

// The database keeps most times as wall-clock time in the ISP's zone (TIMESTAMP, no zone), and
// the backend sends them labelled "Z". Reading that label as UTC shifted every time by the
// browser's offset (3 hours in Kenya). Drop the "Z" and the time is read as the wall-clock
// time it is. Times with an explicit offset ("+03:00", real instants such as a backup file's
// modified time) keep their offset.
export function serverDate(s) {
  if (s instanceof Date) return s
  const str = String(s)
  return parseISO(/T.*Z$/.test(str) ? str.slice(0, -1) : str)
}
