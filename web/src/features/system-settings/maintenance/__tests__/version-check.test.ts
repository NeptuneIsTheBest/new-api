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
import assert from 'node:assert/strict'

import { describe, test } from 'vitest'

import { isUpdateAvailable } from '../version-check'

describe('update availability for fork releases', () => {
  test('does not report the matching upstream release as an update', () => {
    assert.equal(
      isUpdateAvailable('v1.0.0-rc.30-fork.2', 'v1.0.0-rc.30'),
      false
    )
  })

  test('reports a newer upstream release than the fork base', () => {
    assert.equal(isUpdateAvailable('v1.0.0-rc.29-fork.3', 'v1.0.0-rc.30'), true)
  })

  test('does not report an older upstream release as an update', () => {
    assert.equal(
      isUpdateAvailable('v1.0.0-rc.31-fork.1', 'v1.0.0-rc.30'),
      false
    )
  })

  test('compares numeric prerelease identifiers numerically', () => {
    assert.equal(isUpdateAvailable('v1.0.0-rc.9-fork.1', 'v1.0.0-rc.10'), true)
    assert.equal(isUpdateAvailable('v1.0.0-rc.10-fork.1', 'v1.0.0-rc.9'), false)
  })

  test('preserves conservative handling for absent or unrecognized versions', () => {
    assert.equal(isUpdateAvailable(undefined, 'v1.0.0-rc.30'), true)
    assert.equal(isUpdateAvailable('v1.0.0-rc.30', 'v1.0.0-rc.30'), false)
    assert.equal(isUpdateAvailable('v1.0.0-rc.29', 'v1.0.0-rc.30'), true)
    assert.equal(isUpdateAvailable('custom-fork.1', 'v1.0.0-rc.30'), true)
  })
})
