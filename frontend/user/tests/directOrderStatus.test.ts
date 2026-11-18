import assert from 'node:assert/strict'
import test from 'node:test'
import { directStatusOrderNo, parseDirectOrderStatus, startDirectOrderPolling } from '../src/utils/directOrderStatus.ts'

const order = {
  order_no: 'ORDER-1',
  status: 'paid',
  children: [{ items: [{ fulfillment_type: 'kafuda_gpt_direct' }] }],
}

test('polling targets only an eligible direct checkout parent', () => {
  assert.equal(directStatusOrderNo(order), order.order_no)
  assert.equal(directStatusOrderNo({ ...order, status: 'completed' }), order.order_no)
  for (const status of ['pending_payment', 'canceled', 'partially_refunded', 'refunded']) {
    assert.equal(directStatusOrderNo({ ...order, status }), '')
  }
  assert.equal(directStatusOrderNo({ ...order, children: [] }), '')
  assert.equal(directStatusOrderNo({ ...order, children: [...order.children, ...order.children] }), '')
  assert.equal(directStatusOrderNo({ ...order, children: [{ items: [{ fulfillment_type: 'auto' }] }] }), '')
})

test('buyer progress accepts only the public stages and a valid timestamp', () => {
  const updated_at = '2026-11-18T08:00:00Z'
  for (const stage of ['waiting_session', 'checking', 'submitting', 'processing', 'manual_review', 'completed', 'refunded']) {
    assert.deepEqual(parseDirectOrderStatus({ stage, updated_at, internal: 'ignored' }), { stage, updated_at })
  }
  for (const input of [null, {}, { stage: 'failed', updated_at }, { stage: 'manual_delivery', updated_at }, { stage: 'processing', updated_at: 'invalid' }]) {
    assert.equal(parseDirectOrderStatus(input), null)
  }
})

function testPage() {
  return Object.assign(new EventTarget(), { visibilityState: 'visible' as DocumentVisibilityState })
}

test('polling retries an offline refresh and stops after completion', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] })
  let calls = 0
  const seen: (string | null)[] = []
  const stop = startDirectOrderPolling(async () => {
    calls++
    if (calls === 2) throw new Error('offline')
    return { stage: calls === 1 ? 'manual_review' : 'completed', updated_at: '2026-11-18T08:00:00Z' }
  }, (status) => seen.push(status?.stage ?? null), testPage())
  await Promise.resolve()
  t.mock.timers.tick(59_999)
  assert.equal(calls, 1)
  t.mock.timers.tick(1)
  await Promise.resolve()
  t.mock.timers.tick(60_000)
  await Promise.resolve()
  t.mock.timers.tick(60_000)
  assert.deepEqual(seen, ['manual_review', null, 'completed'])
  assert.equal(calls, 3)
  stop()
})

test('leaving the page ignores an in-flight response and does not schedule another poll', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] })
  const response = Promise.withResolvers<unknown>()
  let calls = 0
  const stop = startDirectOrderPolling(() => {
    calls++
    return response.promise
  }, () => assert.fail('late response reached the page'), testPage())
  stop()
  response.resolve({ stage: 'processing', updated_at: '2026-11-18T08:00:00Z' })
  await Promise.resolve()
  t.mock.timers.tick(10_000)
  assert.equal(calls, 1)
})

test('a hidden tab pauses polling and refreshes immediately when visible again', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] })
  const page = testPage()
  const response = Promise.withResolvers<unknown>()
  const status = { stage: 'processing', updated_at: '2026-11-18T08:00:00Z' }
  let calls = 0
  page.visibilityState = 'hidden'
  const stop = startDirectOrderPolling(() => ++calls === 2 ? response.promise : Promise.resolve(status), () => {}, page)
  const changeVisibility = (state: DocumentVisibilityState) => {
    page.visibilityState = state
    page.dispatchEvent(new Event('visibilitychange'))
  }

  t.mock.timers.tick(60_000)
  assert.equal(calls, 0)
  changeVisibility('visible')
  assert.equal(calls, 1)
  await Promise.resolve()
  changeVisibility('hidden')
  t.mock.timers.tick(60_000)
  assert.equal(calls, 1)
  changeVisibility('visible')
  assert.equal(calls, 2)
  changeVisibility('hidden')
  changeVisibility('visible')
  assert.equal(calls, 2)
  response.resolve(status)
  await Promise.resolve()
  t.mock.timers.tick(10_000)
  assert.equal(calls, 3)
  stop()
  changeVisibility('visible')
  assert.equal(calls, 3)
})

test('a confirmed refund stops polls and visibility refreshes', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] })
  const page = testPage()
  const stages: (string | undefined)[] = []
  let calls = 0
  startDirectOrderPolling(async () => {
    calls++
    return { stage: calls === 1 ? 'manual_review' : 'refunded', updated_at: '2026-11-18T08:00:00Z' }
  }, (status) => stages.push(status?.stage), page)
  await Promise.resolve()
  t.mock.timers.tick(60_000)
  await Promise.resolve()
  page.dispatchEvent(new Event('visibilitychange'))
  t.mock.timers.tick(60_000)
  assert.deepEqual(stages, ['manual_review', 'refunded'])
  assert.equal(calls, 2)
})
