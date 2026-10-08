import { atom, read, update } from 'claude-code'
import type { EngineInterface, Register } from 'claude-code'

import type { FleetStatusView } from '../types'
import { GAP, STATUS_FILE, TICK_MS, fitBand, newClocks, parseStatus, sameView, toView } from './view'

// The fleet status mod: a band above the prompt naming the fleet instance this
// Claude Code runs in, how many agents are working and idle across every fleet,
// which ones just stopped, and whether the fleet MCP (Fleet Admiral) is here.
// Fleet installs it into every instance (`fleet claude-mod-env`, from fleet.rc);
// the daemon feeds it through STATUS_FILE.

const view = atom({ plugin: 'fleet-status', key: 'view' } as const, null)

// A fleet MCP server's tools carry its fleet_list tool, whatever the server is
// named here (the in-instance plugin's, or one the user registered).
const isFleetMCPTool = (t: { name: string; mcp: boolean }) => t.mcp && t.name.endsWith('__fleet_list')

async function readStatus($: EngineInterface) {
  try {
    return parseStatus(await $.fs.read(STATUS_FILE))
  } catch {
    return null // no file: the setting is off, or this instance has no control mount
  }
}

async function hasAdmiral($: EngineInterface) {
  try {
    return (await $.tool.list()).some(isFleetMCPTool)
  } catch {
    return false
  }
}

export const register: Register = on => {
  on('session.start', async ($, e, next) => {
    const clocks = newClocks()
    let last: FleetStatusView | null = null
    let isReading = false

    const tick = async () => {
      if (isReading) return
      isReading = true
      try {
        const status = await readStatus($)
        const now = await $.clock.now()
        const v = status === null ? null : toView(status, now, await hasAdmiral($), clocks)
        if (!sameView(last, v)) {
          last = v
          await update($, view, () => v)
        }
      } finally {
        isReading = false
      }
    }

    void tick()
    $.clock.every(TICK_MS, () => void tick())
    return next(e)
  })

  // The band is one slot. A survey has it to itself; a band another plugin
  // beneath draws is kept, stacked above this one; only the engine's own empty
  // drawing is replaced.
  on('ui.render', { component: 'AbovePrompt' }, async ($, e, next) => {
    const v = await read($, view)
    if (v === null || e.props.hasSurvey) return next(e)

    const { Box, Text } = $.ui.resolve(e)
    const band = fitBand(v, e.props.bodyColumns)
    const below = await next(e)

    const row = (
      <Box flexDirection="row" justifyContent="space-between" columnGap={GAP}>
        <Box flexDirection="row" columnGap={GAP} flexShrink={1}>
          {band.left.map(s => (
            <Text key={s.key} color={s.color} bold={s.bold} dimColor={s.dim} wrap="truncate-end">
              {s.text}
            </Text>
          ))}
        </Box>
        {band.right.length > 0 && (
          <Box flexDirection="row" columnGap={GAP} flexShrink={0}>
            {band.right.map(s => (
              <Text key={s.key} color={s.color} bold={s.bold} dimColor={s.dim}>
                {s.text}
              </Text>
            ))}
          </Box>
        )}
      </Box>
    )
    if (below.type === 'engine') return row
    return (
      <Box flexDirection="column">
        {below}
        {row}
      </Box>
    )
  })
}
