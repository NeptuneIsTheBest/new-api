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
type SemanticVersion = {
  core: readonly [string, string, string]
  prerelease: readonly string[]
}

const forkTagPattern = /^(.*)-fork\.(0|[1-9]\d*)$/
const numericIdentifierPattern = /^\d+$/
const semanticVersionPattern =
  /^v?(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$/

function parseSemanticVersion(version: string): SemanticVersion | null {
  const match = semanticVersionPattern.exec(version)
  if (!match) return null

  const prerelease = match[4]?.split('.') ?? []
  const hasInvalidNumericIdentifier = prerelease.some(
    (identifier) =>
      identifier.length > 1 &&
      identifier.startsWith('0') &&
      numericIdentifierPattern.test(identifier)
  )
  if (hasInvalidNumericIdentifier) return null

  return {
    core: [match[1], match[2], match[3]],
    prerelease,
  }
}

function compareNumericIdentifiers(left: string, right: string): number {
  if (left.length !== right.length) {
    return left.length < right.length ? -1 : 1
  }
  if (left === right) return 0
  return left < right ? -1 : 1
}

function comparePrereleaseIdentifiers(left: string, right: string): number {
  const leftIsNumeric = numericIdentifierPattern.test(left)
  const rightIsNumeric = numericIdentifierPattern.test(right)

  if (leftIsNumeric && rightIsNumeric) {
    return compareNumericIdentifiers(left, right)
  }
  if (leftIsNumeric) return -1
  if (rightIsNumeric) return 1
  if (left === right) return 0
  return left < right ? -1 : 1
}

function compareSemanticVersions(
  left: SemanticVersion,
  right: SemanticVersion
): number {
  for (let index = 0; index < left.core.length; index += 1) {
    const coreComparison = compareNumericIdentifiers(
      left.core[index],
      right.core[index]
    )
    if (coreComparison !== 0) return coreComparison
  }

  if (left.prerelease.length === 0 && right.prerelease.length === 0) return 0
  if (left.prerelease.length === 0) return 1
  if (right.prerelease.length === 0) return -1

  const identifierCount = Math.max(
    left.prerelease.length,
    right.prerelease.length
  )
  for (let index = 0; index < identifierCount; index += 1) {
    const leftIdentifier = left.prerelease[index]
    const rightIdentifier = right.prerelease[index]
    if (leftIdentifier === undefined) return -1
    if (rightIdentifier === undefined) return 1

    const prereleaseComparison = comparePrereleaseIdentifiers(
      leftIdentifier,
      rightIdentifier
    )
    if (prereleaseComparison !== 0) return prereleaseComparison
  }

  return 0
}

export function isUpdateAvailable(
  currentVersion: string | null | undefined,
  latestVersion: string
): boolean {
  if (!currentVersion) return true
  if (currentVersion === latestVersion) return false

  const forkTagMatch = forkTagPattern.exec(currentVersion)
  if (!forkTagMatch) return true

  const upstreamBaseVersion = forkTagMatch[1]
  if (upstreamBaseVersion === latestVersion) return false

  const currentUpstreamVersion = parseSemanticVersion(upstreamBaseVersion)
  const latestUpstreamVersion = parseSemanticVersion(latestVersion)
  if (!currentUpstreamVersion || !latestUpstreamVersion) return true

  return (
    compareSemanticVersions(latestUpstreamVersion, currentUpstreamVersion) > 0
  )
}
