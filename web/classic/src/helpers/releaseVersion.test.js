/*
Copyright (C) 2025 QuantumNous

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

import assert from 'node:assert/strict';
import { describe, test } from 'node:test';

import {
  compareReleaseVersions,
  isUpToDateWithUpstream,
} from './releaseVersion';

describe('release version comparison', () => {
  test('compares base versions before fork versions', () => {
    assert.equal(compareReleaseVersions('v1.2.4-fork.1', 'v1.2.3-fork.9'), 1);
    assert.equal(
      isUpToDateWithUpstream('v1.2.4-fork.1', 'v1.2.3-fork.9'),
      true,
    );
  });

  test('treats stable releases as newer than prereleases on the same base', () => {
    assert.equal(compareReleaseVersions('v1.2.3-beta.9-fork.9', 'v1.2.3'), -1);
    assert.equal(
      compareReleaseVersions('v1.2.3-rc.1-fork.9', 'v1.2.3-fork.1'),
      -1,
    );
  });

  test('compares generic prerelease suffixes before fork versions', () => {
    assert.equal(
      compareReleaseVersions(
        'v1.2.3-preview.2-fork.1',
        'v1.2.3-preview.1-fork.9',
      ),
      1,
    );
    assert.equal(
      compareReleaseVersions('v1.2.3-alpha.1-fork.9', 'v1.2.3-beta.1-fork.1'),
      -1,
    );
  });

  test('uses fork versions as the final tie breaker', () => {
    assert.equal(compareReleaseVersions('v1.2.3-fork.2', 'v1.2.3-fork.1'), 1);
    assert.equal(isUpToDateWithUpstream('v1.2.3-fork.2', 'v1.2.3'), true);
    assert.equal(
      compareReleaseVersions('v1.2.3-rc.1-fork.2', 'v1.2.3-rc.1-fork.1'),
      1,
    );
  });

  test('supports exact equality and malformed fallback', () => {
    assert.equal(compareReleaseVersions('v1.2.3', '1.2.3'), 0);
    assert.equal(compareReleaseVersions('dev-build', 'dev-build'), 0);
    assert.equal(isUpToDateWithUpstream('dev-build', 'v1.2.3'), false);
  });
});
