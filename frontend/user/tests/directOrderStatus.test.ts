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
  for (const stage of ['waiting_session', 'processing', 'manual_review', 'completed']) {
    assert.deepEqual(parseDirectOrderStatus({ stage, updated_at, internal: 'ignored' }), { stage, updated_at })
  }
  for (const input of [null, {}, { stage: 'failed', updated_at }, { stage: 'processing', updated_at: 'invalid' }]) {
    assert.equal(parseDirectOrderStatus(input), null)
  }
})

test('polling retries an offline refresh and stops after completion', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] })
  let calls = 0
  const seen: (string | null)[] = []
  const stop = startDirectOrderPolling(async () => {
    calls++
    if (calls === 2) throw new Error('offline')
    return { stage: calls === 1 ? 'manual_review' : 'completed', updated_at: '2026-11-18T08:00:00Z' }
  }, (status) => seen.push(status?.stage ?? null))
  await Promise.resolve()
  for (let tick = 0; tick < 3; tick++) {
    t.mock.timers.tick(10_000)
    await Promise.resolve()
  }
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
  }, () => assert.fail('late response reached the page'))
  stop()
  response.resolve({ stage: 'processing', updated_at: '2026-11-18T08:00:00Z' })
  await Promise.resolve()
  t.mock.timers.tick(10_000)
  assert.equal(calls, 1)
})
