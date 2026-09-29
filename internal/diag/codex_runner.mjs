import { Codex } from '@openai/codex-sdk'

const input = JSON.parse(await new Promise((resolve, reject) => {
  const chunks = []
  process.stdin.on('data', chunk => chunks.push(chunk))
  process.stdin.on('end', () => resolve(Buffer.concat(chunks).toString()))
  process.stdin.on('error', reject)
}))

const codex = new Codex({
  apiKey: input.apiKey,
  baseUrl: input.baseUrl,
  env: input.env,
  config: {
    model_provider: 'codex2api',
    model_providers: {
      codex2api: {
        name: 'codex2api',
        base_url: input.baseUrl,
        wire_api: 'responses',
        env_key: 'CODEX_API_KEY',
        requires_openai_auth: false,
      },
    },
  },
})

const thread = codex.startThread({
  model: input.model,
  workingDirectory: input.workingDirectory,
  sandboxMode: 'workspace-write',
  approvalPolicy: 'never',
  networkAccessEnabled: false,
  webSearchMode: 'disabled',
})

const schema = {
  type: 'object',
  additionalProperties: false,
  required: ['title', 'root_cause', 'confidence', 'can_fix'],
  properties: {
    title: { type: 'string' },
    root_cause: { type: 'string' },
    confidence: { type: 'number' },
    can_fix: { type: 'boolean' },
  },
}

const turn = await thread.run(input.prompt, { outputSchema: schema, signal: AbortSignal.timeout(input.timeoutMs) })
process.stdout.write(turn.finalResponse)
