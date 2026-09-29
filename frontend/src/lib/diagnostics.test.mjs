import test from 'node:test'
import assert from 'node:assert/strict'
import { diagnosticPayload, diagnosticPRLink } from './diagnostics.ts'

test('diagnostic saves preserve omitted credentials and lock the target branch', () => {
  const payload = diagnosticPayload(
    {
      enabled: true,
      repository: 'oneadms/codex2api',
      base_branch: 'main',
      has_api_key: true,
      has_github_token: true,
    },
    '',
    '',
  )
  assert.equal(payload.base_branch, 'custom/main')
  assert.equal('api_key' in payload, false)
  assert.equal('github_token' in payload, false)
  assert.equal('has_api_key' in payload, false)
  assert.equal('has_github_token' in payload, false)
  const updated = diagnosticPayload(
    { base_branch: 'main' },
    ' new-key ',
    ' new-token ',
    true,
    true,
  )
  assert.equal(updated.api_key, 'new-key')
  assert.equal(updated.github_token, 'new-token')
  assert.equal(updated.clear_api_key, true)
})

test('only GitHub pull request URLs become clickable', () => {
  assert.equal(
    diagnosticPRLink('https://github.com/oneadms/codex2api/pull/123'),
    'https://github.com/oneadms/codex2api/pull/123',
  )
  for (const value of [
    'javascript:alert(1)',
    'https://github.com.evil.test/a/b/pull/1',
    'https://secret@github.com/a/b/pull/1',
    'https://github.com/a/b/issues/1',
    'bad-url',
  ])
    assert.equal(diagnosticPRLink(value), undefined)
})
