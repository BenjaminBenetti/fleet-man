import { expect, mock, test } from 'claude-code/testing'
import type { On } from 'claude-code'

import { STATUS_FILE, TICK_MS } from '../hooks/view'

const band = (bodyColumns: number) =>
  ({
    plugin: 'fleet-status',
    component: 'AbovePrompt',
    props: {
      hasSurvey: false,
      isWorking: false,
      maxRows: 10,
      bodyColumns,
      scroll: { offset: 0, bodyRows: 10 },
      view: {},
    },
  }) as const

// The world beneath the mod: the session, the status file (undefined =
// missing) and the session's tools.
function world(on: On, files: { status?: string }, tools: { name: string; mcp: boolean }[] = []) {
  on('session.start', ($, e) => ({ cwd: e.cwd }))
  on('fs.read', ($, e) =>
    e.path === STATUS_FILE && files.status !== undefined ? { value: files.status } : { deny: 'ENOENT' },
  )
  on('tool.list', () => ({ value: tools.map(t => ({ ...t, description: '' })) }))
  // The engine's own band, drawn when the mod hands the site on: empty.
  on('ui.render', { component: 'AbovePrompt' }, ($, e) => {
    const { Box } = $.ui.resolve(e)
    return <Box />
  })
}

// The text of the band's run that matches, or undefined.
const shown = async (ui: { find: (q: { type: string; text: RegExp }) => Promise<{ text?: string } | undefined> }, text: RegExp) =>
  (await ui.find({ type: 'Text', text }))?.text

const statusText = (over: Record<string, unknown> = {}) =>
  JSON.stringify({
    updated_at: 1_000,
    fleet: 'fleet-man',
    instance: 'feature-auth',
    working: 3,
    idle: 2,
    stops: [],
    ...over,
  })

test('draws the instance, the counts and admiral on every surface', async ($, on) => {
  const clock = mock.clock(on, { now: 1_000 })
  world(on, { status: statusText() }, [{ name: 'mcp__plugin_fleet_fleet__fleet_list', mcp: true }])
  await $.session.start({ cwd: '/', surface: 'terminal', isInteractive: true })
  await clock.settle()

  for (const surface of ['terminal', 'desktop'] as const) {
    const ui = await $.ui.mount({ ...band(120), surface })
    expect(await shown(ui, /^fleet-man\/feature-auth$/)).toBeDefined()
    expect(await shown(ui, /working/)).toBe('● 3 working')
    expect(await shown(ui, /idle/)).toBe('○ 2 idle')
    expect(await shown(ui, /admiral/)).toBe('admiral connected')
    expect(await shown(ui, /⚑/)).toBe(undefined)
    await ui.unmount()
  }
})

test('raises a stop for a while, then lets it go', async ($, on) => {
  const clock = mock.clock(on, { now: 1_000 })
  const files = { status: statusText() }
  world(on, files)
  await $.session.start({ cwd: '/', surface: 'terminal', isInteractive: true })
  await clock.settle()
  const ui = await $.ui.mount({ ...band(120), surface: 'terminal' })
  expect(await shown(ui, /admiral/)).toBe(undefined)

  files.status = statusText({ updated_at: 3_000, working: 2, idle: 3, stops: [{ fleet: 'api', instance: 'bugfix-42', at: 3_000 }] })
  await clock.advance(TICK_MS)
  expect(await shown(ui, /⚑/)).toBe('⚑ api/bugfix-42 stopped')
  expect(await shown(ui, /working/)).toBe('● 2 working')

  await clock.advance(16_000)
  expect(await shown(ui, /⚑/)).toBe(undefined)
  await ui.unmount()
})

test('draws nothing of its own without the status file', async ($, on) => {
  const clock = mock.clock(on, { now: 1_000 })
  const files: { status?: string } = { status: statusText() }
  world(on, files)
  await $.session.start({ cwd: '/', surface: 'terminal', isInteractive: true })
  await clock.settle()
  const ui = await $.ui.mount({ ...band(120), surface: 'terminal' })
  expect(await shown(ui, /feature-auth/)).toBeDefined()

  // The setting switched off: the daemon removes the file.
  files.status = undefined
  await clock.advance(TICK_MS)
  expect(await shown(ui, /feature-auth/)).toBe(undefined)
  await ui.unmount()
})
