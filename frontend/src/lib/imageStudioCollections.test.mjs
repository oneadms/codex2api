import test from 'node:test'
import assert from 'node:assert/strict'
import { selectImageTemplates, imageAssetOrientation, selectImageAssets, selectImageJobs } from './imageStudioCollections.ts'

const template = (id, extra = {}) => ({ id, name: `Template ${id}`, prompt: '', model: 'gpt-image-2', style: '', tags: [], favorite: false, usage_count: 0, updated_at: '2026-09-09T00:00:00Z', ...extra })
const filters = { query: '', tag: '', favoritesOnly: false, sort: 'updated' }

test('template search combines normalized text, exact tags, and favorites without mutating the source', () => {
  const source = [template(1, { name: 'ＣＡＦＥ 海报', tags: ['Brand'], favorite: true }), template(2, { name: 'Cafe', tags: ['Branding'], favorite: true }), template(3, { name: 'Cafe', tags: ['brand'] })]
  assert.deepEqual(selectImageTemplates(source, { ...filters, query: ' cafe ', tag: ' BRAND ', favoritesOnly: true }).map(row => row.id), [1])
  assert.deepEqual(source.map(row => row.id), [1, 2, 3])
  assert.equal(source.length, 3)
})

test('template sorting resolves ties and invalid dates consistently', () => {
  const source = [template(1, { usage_count: 4, updated_at: 'invalid' }), template(2, { usage_count: 4 }), template(3, { usage_count: 4 }), template(4, { usage_count: 9 })]
  assert.deepEqual(selectImageTemplates(source, { ...filters, sort: 'used' }).map(row => row.id), [4, 3, 2, 1])
  assert.deepEqual(selectImageTemplates(source, { ...filters, sort: 'name' }).map(row => row.id), [1, 2, 3, 4])
})

test('image orientation uses actual output dimensions instead of requested dimensions', () => {
  assert.equal(imageAssetOrientation({ width: 1024, height: 1024, requested_size: '1536x864' }), 'square')
  assert.equal(imageAssetOrientation({ width: 0, height: 0, actual_size: '864 × 1536' }), 'portrait')
  assert.equal(imageAssetOrientation({ width: 0, height: 0, requested_size: '1536x864', actual_size: 'auto' }), 'unknown')
})

test('gallery filtering intersects orientation with prompt search and retains unknown sizes under All', () => {
  const assets = [{ id: 1, width: 1536, height: 864, filename: 'one.png' }, { id: 2, width: 1024, height: 1024, filename: 'two.png' }, { id: 3, filename: 'three.png' }]
  assert.deepEqual(selectImageAssets(assets, { query: '山野', orientation: 'landscape' }, () => '山野来信').map(row => row.id), [1])
  assert.equal(selectImageAssets(assets, { query: '', orientation: 'all' }, () => '').length, 3)
})

test('history search handles ids, model names, errors, and malformed historical parameters', () => {
  const jobs = [{ id: 21, status: 'failed', params_json: '{broken', error_message: 'Server overloaded' }, { id: 22, status: 'succeeded', params_json: '{"model":"gpt-image-2-4k"}' }]
  assert.deepEqual(selectImageJobs(jobs, { query: '#21', status: 'all' }).map(row => row.id), [21])
  assert.equal(selectImageJobs(jobs, { query: ' OVERLOADED ', status: 'failed' }).length, 1)
  assert.equal(selectImageJobs(jobs, { query: '4K', status: 'succeeded' }).length, 1)
  assert.equal(selectImageJobs(jobs, { query: '4k', status: 'failed' }).length, 0)
})
