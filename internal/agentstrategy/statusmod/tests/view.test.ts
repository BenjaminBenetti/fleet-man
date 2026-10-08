import { describe, expect, test } from 'claude-code/testing'

import type { FleetStatusView } from '../types'
import { ALERT_MS, STALE_MS, fitBand, newClocks, parseStatus, toView } from '../hooks/view'
import type { Status } from '../hooks/view'

const status = (over: Partial<Status> = {}): Status => ({
  updated_at: 1_000_000,
  fleet: 'fleet-man',
  instance: 'feature-auth',
  working: 3,
  idle: 2,
  stops: [],
  ...over,
})

// The band as text: left || right, each side's segments joined by " | ".
const text = (band: ReturnType<typeof fitBand>) =>
  [band.left, band.right].map(side => side.map(s => s.text).join(' | ')).join(' || ')

describe('parseStatus', () => {
  test('reads the file the daemon writes', () => {
    const s = status({ stops: [{ fleet: 'f', instance: 'i', at: 5 }] })
    expect(parseStatus(JSON.stringify(s))).toEqual(s)
  })

  test('refuses what is not a status', () => {
    expect(parseStatus('not json')).toBe(null)
    expect(parseStatus('null')).toBe(null)
    expect(parseStatus(JSON.stringify({ fleet: 'f' }))).toBe(null)
    expect(parseStatus(JSON.stringify(status({ working: -1 })))).toBe(null)
  })

  test('drops malformed stops and keeps the rest', () => {
    const raw = { ...status(), stops: [{ fleet: 'f' }, { fleet: 'f', instance: 'i', at: 7 }] }
    expect(parseStatus(JSON.stringify(raw))?.stops).toEqual([{ fleet: 'f', instance: 'i', at: 7 }])
  })
})

describe('toView', () => {
  test('labels the instance and carries the counts', () => {
    const v = toView(status(), 0, true, newClocks())
    expect(v).toEqual({ label: 'fleet-man/feature-auth', working: 3, idle: 2, stopped: [], admiral: true })
  })

  test('shows a stop for ALERT_MS of the mod clock, whatever the daemon clock says', () => {
    const clocks = newClocks()
    // The daemon wrote the stop 2s after it happened; the mod clock is far off.
    const s = status({ updated_at: 50_000, stops: [{ fleet: 'api', instance: 'bugfix-42', at: 48_000 }] })
    expect(toView(s, 7, false, clocks).stopped).toEqual(['api/bugfix-42'])
    expect(toView(s, 7 + ALERT_MS - 2_001, false, clocks).stopped).toEqual(['api/bugfix-42'])
    expect(toView(s, 7 + ALERT_MS - 2_000, false, clocks).stopped).toEqual([])
  })

  test('names an instance once when it stopped twice', () => {
    const stops = [
      { fleet: 'api', instance: 'a', at: 999_000 },
      { fleet: 'api', instance: 'a', at: 1_000_000 },
    ]
    expect(toView(status({ stops }), 0, false, newClocks()).stopped).toEqual(['api/a'])
  })

  test('hides the counts once the file stops changing', () => {
    const clocks = newClocks()
    toView(status(), 0, false, clocks)
    expect(toView(status(), STALE_MS, false, clocks).working).toBe(3)
    const stale = toView(status(), STALE_MS + 1, false, clocks)
    expect(stale.working).toBe(null)
    expect(stale.idle).toBe(null)
    // A fresh write brings them back.
    expect(toView(status({ updated_at: 2_000_000 }), STALE_MS + 2, false, clocks).working).toBe(3)
  })
})

describe('fitBand', () => {
  const v: FleetStatusView = {
    label: 'fleet-man/feature-auth',
    working: 3,
    idle: 2,
    stopped: ['api/bugfix-42', 'web/cache'],
    admiral: true,
  }

  test('spells everything out when there is room', () => {
    expect(text(fitBand(v, 200))).toBe(
      'fleet-man/feature-auth | ⚑ api/bugfix-42, web/cache stopped || ● 3 working | ○ 2 idle | Admiral connected',
    )
  })

  test('shortens step by step as the band narrows', () => {
    expect(text(fitBand(v, 80))).toBe('fleet-man/feature-auth | ⚑ api/bugfix-42, web/cache || ●3 | ○2 | Admiral')
    expect(text(fitBand(v, 60))).toBe('fleet-man/feature-auth | ⚑ api/bugfix-42 +1 || ●3 | ○2 | Admiral')
    expect(text(fitBand(v, 50))).toBe('fleet-man/feature-auth | ⚑ api/bugfix-42 +1 || ●3 | ○2')
    expect(text(fitBand(v, 36))).toBe('fleet-man/feature-auth || ●3 | ○2')
  })

  test('never drops the instance name', () => {
    expect(text(fitBand(v, 10))).toBe('fleet-man/feature-auth || ')
  })

  test('leaves out what it does not know', () => {
    const quiet = fitBand({ ...v, working: null, idle: null, stopped: [], admiral: false }, 200)
    expect(text(quiet)).toBe('fleet-man/feature-auth || ')
  })
})
