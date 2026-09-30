import { Codex } from '@openai/codex-sdk'
import { writeFileSync, appendFileSync } from 'fs'

const input = JSON.parse(await new Promise((resolve, reject) => {
  const chunks = []
  process.stdin.on('data', chunk => chunks.push(chunk))
  process.stdin.on('end', () => resolve(Buffer.concat(chunks).toString()))
  process.stdin.on('error', reject)
}))

// 事件日志写到运行产物目录，方便事后排查卡在哪一步。
const eventLog = input.eventLog || ''
const log = (msg) => {
  const line = new Date().toISOString() + ' ' + msg + '\n'
  process.stderr.write(line)
  if (eventLog) { try { appendFileSync(eventLog, line) } catch {} }
}
if (eventLog) { try { writeFileSync(eventLog, '') } catch {} }

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
        // 上游流静默超过 3 分钟即判定失败并重试一次，避免挂死到 12 分钟总超时。
        stream_idle_timeout_ms: 180000,
        stream_max_retries: 1,
        request_max_retries: 2,
      },
    },
  },
})

const thread = codex.startThread({
  model: input.model,
  modelReasoningEffort: input.reasoningEffort || undefined,
  workingDirectory: input.workingDirectory,
  // Docker 容器内 bwrap 无法创建命名空间，改用无沙箱模式；
  // 工作区是隔离的临时 checkout，Go 侧会校验 diff 后才应用。
  sandboxMode: 'danger-full-access',
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

// 空闲看门狗：事件流超过 idleTimeoutMs 没有任何事件就主动终止，
// 不等 12 分钟总超时。
const idleMs = input.idleTimeoutMs || 180000
let idleTimer = null
const resetIdle = () => {
  if (idleTimer) clearTimeout(idleTimer)
  idleTimer = setTimeout(() => {
    log('IDLE: no events for ' + Math.round(idleMs / 1000) + 's, aborting')
    process.exit(2)
  }, idleMs)
}
resetIdle()

const { events } = await thread.runStreamed(input.prompt, { outputSchema: schema, signal: AbortSignal.timeout(input.timeoutMs) })
let finalResponse = ''
let turnFailure = null
for await (const event of events) {
  resetIdle()
  const type = event.type || 'unknown'
  if (type === 'item.completed' && event.item?.type === 'agent_message') {
    finalResponse = event.item.text
    log('agent_message received (' + (finalResponse || '').length + ' chars)')
  } else if (type === 'turn.completed') {
    log('turn.completed')
  } else if (type === 'turn.failed') {
    turnFailure = event.error
    log('turn.failed: ' + (turnFailure?.message || JSON.stringify(turnFailure)))
    break
  } else if (type === 'thread.started') {
    log('thread.started id=' + (event.thread_id || ''))
  } else {
    log(type)
  }
}
if (idleTimer) clearTimeout(idleTimer)
if (turnFailure) {
  throw new Error('Codex turn failed: ' + (turnFailure.message || JSON.stringify(turnFailure)))
}
process.stdout.write(finalResponse)
