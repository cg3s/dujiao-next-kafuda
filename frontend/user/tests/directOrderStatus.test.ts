import assert from 'node:assert/strict'
import test from 'node:test'
import { directStatusOrderNo, isDirectOrderFulfillment, parseDirectOrderStatus, startDirectOrderPolling, type DirectOrderStatus } from '../src/utils/directOrderStatus.ts'

const order = {
  order_no: 'ORDER-1',
  status: 'paid',
  children: [{ items: [{ fulfillment_type: 'kafuda_gpt_direct' }] }],
}

test('polling targets only an eligible direct checkout parent', () => {
  assert.equal(directStatusOrderNo(order), order.order_no)
  assert.equal(directStatusOrderNo({ ...order, status: 'fulfilling' }), order.order_no)
  assert.equal(directStatusOrderNo({ ...order, status: 'completed' }), order.order_no)
  assert.equal(directStatusOrderNo({ ...order, status: 'refunded' }), order.order_no)
  for (const status of ['pending_payment', 'canceled', 'partially_refunded', undefined]) {
    assert.equal(directStatusOrderNo({ ...order, status }), '')
  }
  assert.equal(directStatusOrderNo({ ...order, children: [] }), '')
  assert.equal(directStatusOrderNo({ ...order, children: [...order.children, ...order.children] }), '')
  assert.equal(directStatusOrderNo({ ...order, status: 'fulfilling', children: [{ items: [{ fulfillment_type: 'auto' }] }] }), '')
})

test('manual delivery retains the direct order presentation only for a direct purchase', () => {
  const directItems = order.children[0]!.items
  assert.equal(isDirectOrderFulfillment(directItems, 'manual'), true)
  assert.equal(isDirectOrderFulfillment([{ fulfillment_type: 'manual' }], 'manual'), false)
  assert.equal(isDirectOrderFulfillment(undefined, 'kafuda_gpt_direct'), true)
  assert.equal(isDirectOrderFulfillment([...directItems, { fulfillment_type: 'manual' }], 'manual'), false)
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

test('only a completed recharge displays a boolean renewal result', () => {
  const updated_at = '2026-11-18T08:00:00Z'
  for (const renewal_cancelled of [true, false]) {
    assert.deepEqual(parseDirectOrderStatus({ stage: 'completed', updated_at, renewal_cancelled }), {
      stage: 'completed', updated_at, renewal_cancelled,
    })
  }
  assert.deepEqual(parseDirectOrderStatus({ stage: 'completed', updated_at, renewal_cancelled: 'false' }), { stage: 'completed', updated_at })
  assert.deepEqual(parseDirectOrderStatus({ stage: 'processing', updated_at, renewal_cancelled: true }), { stage: 'processing', updated_at })
})

function testPage() {
  return Object.assign(new EventTarget(), { visibilityState: 'visible' as DocumentVisibilityState })
}

function testConnectivity(online = true) {
  const events = new EventTarget()
  return {
    events,
    isOnline: () => online,
    setOnline(value: boolean) {
      online = value
      events.dispatchEvent(new Event(online ? 'online' : 'offline'))
    },
  }
}

test('known offline polls pause, reconnect while visible and detach when stopped', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] })
  const page = testPage()
  const connectivity = testConnectivity(false)
  const seen: (string | null)[] = []
  let calls = 0
  const stop = startDirectOrderPolling(async () => {
    calls++
    return { stage: 'manual_review', updated_at: '2026-11-18T08:00:00Z' }
  }, (status) => seen.push(status?.stage ?? null), page, undefined, connectivity)
  t.mock.timers.tick(60_000)
  assert.equal(calls, 0)
  assert.deepEqual(seen, [null])

  connectivity.setOnline(true)
  assert.equal(calls, 1)
  await Promise.resolve()
  connectivity.setOnline(false)
  t.mock.timers.tick(60_000)
  assert.equal(calls, 1)
  assert.deepEqual(seen, [null, 'manual_review', null])

  page.visibilityState = 'hidden'
  page.dispatchEvent(new Event('visibilitychange'))
  connectivity.setOnline(true)
  assert.equal(calls, 1)
  page.visibilityState = 'visible'
  page.dispatchEvent(new Event('visibilitychange'))
  assert.equal(calls, 2)
  await Promise.resolve()
  stop()
  connectivity.setOnline(false)
  connectivity.setOnline(true)
  t.mock.timers.tick(60_000)
  assert.equal(calls, 2)
  assert.deepEqual(seen, [null, 'manual_review', null, 'manual_review'])
})

test('reconnect during a failing in-flight poll refreshes immediately without overlapping requests', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] })
  const connectivity = testConnectivity()
  const pending = Promise.withResolvers<unknown>()
  const goodStatus = { stage: 'processing', updated_at: '2026-11-18T08:00:00Z' }
  const seen: (DirectOrderStatus | null)[] = []
  let calls = 0
  const stop = startDirectOrderPolling(() => {
    calls++
    return calls === 1 ? pending.promise : Promise.resolve(goodStatus)
  }, (status) => seen.push(status), testPage(), undefined, connectivity)
  connectivity.setOnline(false)
  connectivity.setOnline(true)
  assert.equal(calls, 1)
  pending.reject(new Error('connection interrupted'))
  await Promise.resolve()
  assert.equal(calls, 2)
  await Promise.resolve()
  assert.deepEqual(seen, [null, null, goodStatus])
  t.mock.timers.tick(9_999)
  assert.equal(calls, 2)
  stop()
})

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

test('opening a refunded direct order retries an unavailable status and stops after confirmation', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] })
  const page = testPage()
  const number = directStatusOrderNo({ ...order, status: 'refunded' })
  const refund: DirectOrderStatus = { stage: 'refunded', updated_at: '2026-11-18T08:00:00Z' }
  const seen: (DirectOrderStatus | null)[] = []
  let statusRequests = 0
  assert.equal(number, order.order_no)
  startDirectOrderPolling(async () => {
    statusRequests++
    if (statusRequests === 1) throw new Error('offline')
    return refund
  }, (status) => seen.push(status), page)
  await Promise.resolve()
  t.mock.timers.tick(10_000)
  await Promise.resolve()
  t.mock.timers.tick(60_000)
  page.dispatchEvent(new Event('visibilitychange'))
  assert.equal(statusRequests, 2)
  assert.deepEqual(seen, [null, refund])
})

test('terminal progress retries order details without querying progress again', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] })
  const page = testPage()
  const stages: string[] = []
  let statusRequests = 0
  let detailRequests = 0
  const updated_at = '2026-11-18T08:00:00Z'
  startDirectOrderPolling(async () => {
    statusRequests++
    return { stage: 'completed', updated_at }
  }, (status) => {
    if (status) stages.push(status.stage)
  }, page, async (stage) => {
    assert.equal(stage, 'completed')
    detailRequests++
    if (detailRequests === 1) throw new Error('offline')
    return detailRequests === 3
  })
  await Promise.resolve()
  await Promise.resolve()
  assert.equal(detailRequests, 1)
  t.mock.timers.tick(10_000)
  await Promise.resolve()
  assert.equal(detailRequests, 2)
  t.mock.timers.tick(10_000)
  await Promise.resolve()
  assert.equal(detailRequests, 3)
  t.mock.timers.tick(60_000)
  page.dispatchEvent(new Event('visibilitychange'))
  assert.equal(detailRequests, 3)
  assert.equal(statusRequests, 1)
  assert.deepEqual(stages, ['completed'])
})

test('leaving after terminal progress cancels pending order detail retries', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] })
  const page = testPage()
  let detailRequests = 0
  const detail = Promise.withResolvers<boolean>()
  const stop = startDirectOrderPolling(async () => ({
    stage: 'refunded',
    updated_at: '2026-11-18T08:00:00Z',
  }), () => {}, page, () => {
    detailRequests++
    return detail.promise
  })
  await Promise.resolve()
  assert.equal(detailRequests, 1)
  stop()
  detail.resolve(false)
  await Promise.resolve()
  page.dispatchEvent(new Event('visibilitychange'))
  t.mock.timers.tick(60_000)
  assert.equal(detailRequests, 1)
})

test('reconnecting resumes terminal order details without polling progress again', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] })
  const connectivity = testConnectivity()
  const pending = Promise.withResolvers<boolean>()
  const completed = { stage: 'completed', updated_at: '2026-11-18T08:00:00Z' }
  const seen: (DirectOrderStatus | null)[] = []
  let statusRequests = 0
  let detailRequests = 0
  const stop = startDirectOrderPolling(async () => {
    statusRequests++
    return completed
  }, (status) => seen.push(status), testPage(), (stage) => {
    detailRequests++
    assert.equal(stage, 'completed')
    return detailRequests === 1 ? pending.promise : Promise.resolve(true)
  }, connectivity)
  await Promise.resolve()
  assert.equal(detailRequests, 1)
  connectivity.setOnline(false)
  pending.resolve(false)
  await Promise.resolve()
  t.mock.timers.tick(60_000)
  assert.equal(detailRequests, 1)
  connectivity.setOnline(true)
  assert.equal(detailRequests, 2)
  await Promise.resolve()
  connectivity.setOnline(false)
  connectivity.setOnline(true)
  t.mock.timers.tick(60_000)
  assert.equal(detailRequests, 2)
  assert.equal(statusRequests, 1)
  assert.deepEqual(seen, [completed])
  stop()
})
