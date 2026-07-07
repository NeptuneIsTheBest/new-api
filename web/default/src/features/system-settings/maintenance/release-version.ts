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
type ParsedReleaseVersion = {
  base: [number, number, number]
  prerelease: string[]
  fork: number
}

const RELEASE_VERSION_PATTERN = /^v?(\d+)\.(\d+)\.(\d+)(?:-(.+))?$/i
const FORK_SUFFIX_PATTERN = /^fork\.(\d+)$/i
const NUMERIC_IDENTIFIER_PATTERN = /^\d+$/

function normalizeVersion(version: unknown): string {
  return String(version ?? '').trim()
}

function parseReleaseVersion(version: unknown): ParsedReleaseVersion | null {
  const normalized = normalizeVersion(version)
  const [versionCore] = normalized.split('+')
  const match = RELEASE_VERSION_PATTERN.exec(versionCore)
  if (!match) return null

  const suffixSegments = (match[4] ?? '').split('-').filter(Boolean)
  const forkMatch = FORK_SUFFIX_PATTERN.exec(suffixSegments.at(-1) ?? '')
  const fork = forkMatch ? Number(forkMatch[1]) : 0
  const prereleaseSegments = forkMatch
    ? suffixSegments.slice(0, -1)
    : suffixSegments

  return {
    base: [Number(match[1]), Number(match[2]), Number(match[3])],
    prerelease: prereleaseSegments.flatMap((segment) =>
      segment.split('.').filter(Boolean)
    ),
    fork,
  }
}

function compareNumber(current: number, latest: number): number {
  if (current > latest) return 1
  if (current < latest) return -1
  return 0
}

function comparePrereleaseIdentifier(
  currentIdentifier: string,
  latestIdentifier: string
): number {
  const currentNumeric = NUMERIC_IDENTIFIER_PATTERN.test(currentIdentifier)
  const latestNumeric = NUMERIC_IDENTIFIER_PATTERN.test(latestIdentifier)

  if (currentNumeric && latestNumeric) {
    return compareNumber(Number(currentIdentifier), Number(latestIdentifier))
  }
  if (currentNumeric) return -1
  if (latestNumeric) return 1

  const currentLower = currentIdentifier.toLowerCase()
  const latestLower = latestIdentifier.toLowerCase()
  if (currentLower > latestLower) return 1
  if (currentLower < latestLower) return -1
  return 0
}

function comparePrerelease(
  currentPrerelease: string[],
  latestPrerelease: string[]
): number {
  if (currentPrerelease.length === 0 && latestPrerelease.length > 0) return 1
  if (currentPrerelease.length > 0 && latestPrerelease.length === 0) return -1

  const maxLength = Math.max(currentPrerelease.length, latestPrerelease.length)
  for (let index = 0; index < maxLength; index += 1) {
    const currentIdentifier = currentPrerelease[index]
    const latestIdentifier = latestPrerelease[index]
    if (currentIdentifier === undefined) return -1
    if (latestIdentifier === undefined) return 1

    const comparison = comparePrereleaseIdentifier(
      currentIdentifier,
      latestIdentifier
    )
    if (comparison !== 0) return comparison
  }

  return 0
}

export function compareReleaseVersions(
  currentVersion: unknown,
  latestVersion: unknown
): number {
  const normalizedCurrent = normalizeVersion(currentVersion)
  const normalizedLatest = normalizeVersion(latestVersion)
  const current = parseReleaseVersion(normalizedCurrent)
  const latest = parseReleaseVersion(normalizedLatest)

  if (!current || !latest) {
    return normalizedCurrent === normalizedLatest ? 0 : -1
  }

  for (let index = 0; index < current.base.length; index += 1) {
    const comparison = compareNumber(current.base[index], latest.base[index])
    if (comparison !== 0) return comparison
  }

  const prereleaseComparison = comparePrerelease(
    current.prerelease,
    latest.prerelease
  )
  if (prereleaseComparison !== 0) return prereleaseComparison

  return compareNumber(current.fork, latest.fork)
}

export function isUpToDateWithUpstream(
  currentVersion: unknown,
  latestVersion: unknown
): boolean {
  return compareReleaseVersions(currentVersion, latestVersion) >= 0
}
