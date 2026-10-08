// The band the fleet status mod draws above the prompt, as its ui.render hook
// reads it from $.state.
export type FleetStatusView = {
  // "<fleet>/<instance>" of the instance this Claude Code runs in.
  label: string
  // Agents working and idle (paused) across every fleet; null while they are
  // not live (no fleet TUI connected, or a file not yet seen to change).
  working: number | null
  idle: number | null
  // "<fleet>/<instance>" of each other agent that paused moments ago.
  stopped: string[]
  // A fleet MCP server is among this session's tools (Fleet Admiral mode).
  admiral: boolean
}

declare module 'claude-code' {
  interface PluginState {
    'fleet-status': { view: FleetStatusView | null }
  }
}
