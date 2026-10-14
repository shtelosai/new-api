/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
// Channel type ids: 1 = OpenAI, 14 = Anthropic Claude
const OPENAI_CHANNEL_TYPE = 1
const ANTHROPIC_CHANNEL_TYPE = 14

export type TworkOMPWire = 'responses' | 'chat-completions' | 'anthropic-messages'

/**
 * Twork runtime of a channel as stored in `setting`.
 * - legacy: used by old Twork clients (default)
 * - omp: used only by new OMP clients
 * - managed: Pi / Codex / async image channels, editable only from Twork CMS
 */
export type TworkChannelRoute =
  | { runtime: 'legacy' }
  | { runtime: 'omp'; wire: TworkOMPWire | '' }
  | { runtime: 'managed'; managedRuntime: string }

export const TWORK_OMP_WIRE_LABELS: Record<TworkOMPWire, string> = {
  responses: 'Responses',
  'chat-completions': 'Chat Completions',
  'anthropic-messages': 'Anthropic Messages',
}

/** Must match model.TworkOMPFormWires in the backend. */
export function getTworkOMPFormWires(channelType: number): TworkOMPWire[] {
  if (channelType === ANTHROPIC_CHANNEL_TYPE) return ['anthropic-messages']
  if (channelType === OPENAI_CHANNEL_TYPE) return ['responses', 'chat-completions']
  return []
}

export function parseTworkChannelRoute(
  setting: string | null | undefined
): TworkChannelRoute {
  if (!setting) return { runtime: 'legacy' }
  let parsed: Record<string, unknown>
  try {
    parsed = JSON.parse(setting)
  } catch {
    return { runtime: 'legacy' }
  }
  const runtime = parsed.twork_runtime
  if (runtime === undefined || runtime === '' || runtime === 'legacy') {
    return { runtime: 'legacy' }
  }
  if (runtime === 'omp') {
    const wire = parsed.twork_wire_api
    return {
      runtime: 'omp',
      wire:
        typeof wire === 'string' && wire in TWORK_OMP_WIRE_LABELS
          ? (wire as TworkOMPWire)
          : '',
    }
  }
  return { runtime: 'managed', managedRuntime: String(runtime) }
}
