import type { FleetStatusView } from '../types'

// The status file the fleet daemon keeps in this instance's control directory
// (bind-mounted at /fleet-mounts/control) while the "Fleet status mod" setting
// is on. Absent — the setting off, or a backend with no control mount — the
// mod draws nothing.
export const STATUS_FILE = '/fleet-mounts/control/claude-status.json'

// How often the file is read.
export const TICK_MS = 2_000
// How long a "stopped" alert stays up.
export const ALERT_MS = 15_000
// How long after the daemon last rewrote the file its counts are trusted. The
// daemon refreshes it every few seconds while a fleet TUI is connected (agent
// activity is only polled then).
export const STALE_MS = 30_000

// The file as the daemon writes it (agentstrategy.StatusModFile).
export type Status = {
  updated_at: number
  fleet: string
  instance: string
  working: number
  idle: number
  stops: StatusStop[]
}

// An agent elsewhere that went from working to idle, at `at` (daemon clock).
export type StatusStop = { fleet: string; instance: string; at: number }

const isString = (v: unknown): v is string => typeof v === 'string'
const isCount = (v: unknown): v is number =>
  typeof v === 'number' && Number.isFinite(v) && v >= 0

// parseStatus reads the file's text, or null when it is not a status.
export function parseStatus(text: string): Status | null {
  let raw: unknown
  try {
    raw = JSON.parse(text)
  } catch {
    return null
  }
  if (typeof raw !== 'object' || raw === null) return null
  const s = raw as Record<string, unknown>
  if (!isString(s.fleet) || !isString(s.instance) || !isCount(s.updated_at)) return null
  if (!isCount(s.working) || !isCount(s.idle)) return null
  const stops: StatusStop[] = []
  for (const stop of Array.isArray(s.stops) ? s.stops : []) {
    const t = stop as Record<string, unknown>
    if (isString(t.fleet) && isString(t.instance) && isCount(t.at)) {
      stops.push({ fleet: t.fleet, instance: t.instance, at: t.at })
    }
  }
  return {
    updated_at: s.updated_at,
    fleet: s.fleet,
    instance: s.instance,
    working: s.working,
    idle: s.idle,
    stops,
  }
}

// Clocks is what toView remembers between reads. Every time it keeps is on the
// mod's own clock: the daemon's (updated_at, at) is only ever compared with
// itself, so a container clock that drifts from the host's changes nothing.
export type Clocks = {
  // updated_at of the last read, and when (mod clock) it last changed.
  updatedAt: number
  freshAt: number
  // When each stop (by key) started, translated onto the mod clock.
  stopSeen: Map<string, number>
}

export const newClocks = (): Clocks => ({ updatedAt: -1, freshAt: 0, stopSeen: new Map() })

// toView turns a read of the file at `now` (mod clock) into what the band shows.
export function toView(s: Status, now: number, admiral: boolean, clocks: Clocks): FleetStatusView {
  if (s.updated_at !== clocks.updatedAt) {
    clocks.updatedAt = s.updated_at
    clocks.freshAt = now
  }
  const isStale = now - clocks.freshAt > STALE_MS

  const stopped: string[] = []
  const live = new Set<string>()
  for (const stop of s.stops) {
    const name = `${stop.fleet}/${stop.instance}`
    const key = `${name}@${stop.at}`
    live.add(key)
    let seen = clocks.stopSeen.get(key)
    if (seen === undefined) {
      // Its age by the daemon's clock, carried over to ours.
      seen = now - Math.max(0, s.updated_at - stop.at)
      clocks.stopSeen.set(key, seen)
    }
    if (now - seen < ALERT_MS && !stopped.includes(name)) stopped.push(name)
  }
  for (const key of clocks.stopSeen.keys()) {
    if (!live.has(key)) clocks.stopSeen.delete(key)
  }

  return {
    label: `${s.fleet}/${s.instance}`,
    working: isStale ? null : s.working,
    idle: isStale ? null : s.idle,
    stopped: isStale ? [] : stopped,
    admiral,
  }
}

export const sameView = (a: FleetStatusView | null, b: FleetStatusView | null): boolean =>
  JSON.stringify(a) === JSON.stringify(b)

// One run of text in the band.
export type Segment = { key: string; text: string; color?: string; bold?: boolean; dim?: boolean }

// The band laid out for a width: the segments on the left, and the one pinned
// to the right (admiral), if any.
export type Band = { left: Segment[]; right: Segment | null }

// The cells between two segments.
export const GAP = 2

// Theme colors, so the band follows Claude Code's own theme. Idle is drawn
// faint instead: the ANSI themes have no gray to give it.
const WORKING = 'success'
const STOPPED = 'warning'
const ADMIRAL = 'ide'

const width = (band: Band): number => {
  const parts = [...band.left, ...(band.right ? [band.right] : [])]
  return parts.reduce((n, p) => n + [...p.text].length, 0) + GAP * Math.max(0, parts.length - 1)
}

// fitBand lays the view out in `columns` cells, shortening it step by step:
// the full words first, then the compact forms, then dropping what matters
// least. The instance's own name is never dropped — telling which Claude this
// is is the band's first job.
export function fitBand(v: FleetStatusView, columns: number): Band {
  const name: Segment = { key: 'name', text: v.label, bold: true }
  const hasCounts = v.working !== null && v.idle !== null
  const stoppedList = (names: string[]) => names.join(', ')

  const build = (o: {
    compact: boolean
    stops: 'all' | 'first' | 'none'
    admiral: boolean
    counts: boolean
  }): Band => {
    const left: Segment[] = [name]
    if (hasCounts && o.counts) {
      left.push(
        o.compact
          ? { key: 'working', text: `●${v.working}`, color: WORKING }
          : { key: 'working', text: `● ${v.working} working`, color: WORKING },
        o.compact
          ? { key: 'idle', text: `○${v.idle}`, dim: true }
          : { key: 'idle', text: `○ ${v.idle} idle`, dim: true },
      )
    }
    if (v.stopped.length > 0 && o.stops !== 'none') {
      const shown = o.stops === 'all' ? v.stopped : v.stopped.slice(0, 1)
      const more = v.stopped.length - shown.length
      const text =
        `⚑ ${stoppedList(shown)}` + (more > 0 ? ` +${more}` : '') + (o.compact ? '' : ' stopped')
      left.push({ key: 'stopped', text, color: STOPPED })
    }
    const right: Segment | null =
      v.admiral && o.admiral
        ? { key: 'admiral', text: o.compact ? 'admiral' : 'admiral connected', color: ADMIRAL }
        : null
    return { left, right }
  }

  const steps = [
    build({ compact: false, stops: 'all', admiral: true, counts: true }),
    build({ compact: true, stops: 'all', admiral: true, counts: true }),
    build({ compact: true, stops: 'first', admiral: true, counts: true }),
    build({ compact: true, stops: 'first', admiral: false, counts: true }),
    build({ compact: true, stops: 'none', admiral: false, counts: true }),
  ]
  for (const band of steps) {
    if (width(band) <= columns) return band
  }
  return build({ compact: true, stops: 'none', admiral: false, counts: false })
}
