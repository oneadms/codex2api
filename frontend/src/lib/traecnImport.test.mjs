import assert from 'node:assert/strict'
import test from 'node:test'
import { parseTraeCNImportJSON, selectTraeCNImportEntries, traeCNImportPreviewLabel } from './traecnImport.ts'

test('import preview reads the exported envelope, aliases and device codes', () => {
  const payload = JSON.stringify({
    version: 1,
    channel: 'traecn',
    accounts: [
      { name: 'a1', email: 'a1@example.com', refresh_token: 'rt-1', machine_id: 'm1', device_id: 'd1', host: 'https://trae-api-cn.mchost.guru' },
      { name: 'a2', refreshToken: 'rt-2', userId: 'u-2' },
      { name: 'broken' },
    ],
  })
  const { items, error } = parseTraeCNImportJSON(payload)
  assert.equal(error, undefined)
  assert.equal(items.length, 3)
  assert.deepEqual(items[0].refresh_token, 'rt-1')
  assert.deepEqual(items[0].machine_id, 'm1')
  assert.equal(items[1].refresh_token, 'rt-2')
  assert.equal(items[1].user_id, 'u-2')
  assert.equal(items[2].problem, 'missing_refresh_token')
  assert.equal(traeCNImportPreviewLabel(items[0]), 'a1@example.com')
  assert.equal(traeCNImportPreviewLabel(items[2]), 'broken')
})

test('import preview accepts bare arrays and single objects', () => {
  const bare = parseTraeCNImportJSON('[{"rt":"rt-a"},{"refresh_token":"rt-b"}]')
  assert.deepEqual(bare.items.map((item) => item.refresh_token), ['rt-a', 'rt-b'])

  const single = parseTraeCNImportJSON('{"name":"solo","RefreshToken":"rt-c","accessToken":"at-c"}')
  assert.equal(single.items.length, 1)
  assert.equal(single.items[0].refresh_token, 'rt-c')
  assert.equal(single.items[0].access_token, 'at-c')

  const nested = parseTraeCNImportJSON('{"data":[{"refresh_token":"rt-d"}]}')
  assert.equal(nested.items[0].refresh_token, 'rt-d')
})

test('import preview reports malformed input instead of throwing', () => {
  assert.equal(parseTraeCNImportJSON('').error, 'empty')
  assert.equal(parseTraeCNImportJSON('not json').error, 'invalid_json')
  assert.equal(parseTraeCNImportJSON('123').error, 'invalid_shape')
  assert.equal(parseTraeCNImportJSON('{"accounts":[]}').error, 'no_accounts')
})

test('only the selected entries are handed to the import API', () => {
  const payload = JSON.stringify({
    accounts: [
      { name: 'a1', refresh_token: 'rt-1' },
      { name: 'a2', refresh_token: 'rt-2' },
      { name: 'a3', refresh_token: 'rt-3' },
    ],
  })
  const picked = selectTraeCNImportEntries(payload, [2, 0])
  assert.deepEqual(picked.map((entry) => entry.refresh_token), ['rt-3', 'rt-1'])

  const bare = selectTraeCNImportEntries('[{"refresh_token":"rt-a"},{"refresh_token":"rt-b"}]', [1])
  assert.deepEqual(bare.map((entry) => entry.refresh_token), ['rt-b'])

  assert.deepEqual(selectTraeCNImportEntries('broken', [0]), [])
  assert.deepEqual(selectTraeCNImportEntries(payload, [99]), [])
})
