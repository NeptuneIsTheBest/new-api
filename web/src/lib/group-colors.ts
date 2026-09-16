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
export const GROUP_COLORS = [
  'blue',
  'green',
  'cyan',
  'purple',
  'pink',
  'red',
  'orange',
  'yellow',
  'grey',
] as const

export type GroupColor = (typeof GROUP_COLORS)[number]
export type GroupColors = Record<string, GroupColor>

export const GROUP_COLORS_QUERY_KEY = ['group-colors'] as const

export function isGroupColor(value: unknown): value is GroupColor {
  return GROUP_COLORS.some((color) => color === value)
}

export function isGroupColors(value: unknown): value is GroupColors {
  return (
    value !== null &&
    typeof value === 'object' &&
    !Array.isArray(value) &&
    Object.entries(value).every(
      ([group, color]) =>
        group !== '' &&
        group === group.trim() &&
        group !== 'auto' &&
        isGroupColor(color)
    )
  )
}

export function parseGroupColors(value: string): GroupColors {
  try {
    const parsed: unknown = JSON.parse(value)
    return isGroupColors(parsed) ? parsed : {}
  } catch {
    return {}
  }
}
