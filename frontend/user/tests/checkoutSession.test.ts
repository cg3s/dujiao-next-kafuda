import assert from 'node:assert/strict'
import test, { type TestContext } from 'node:test'
import { setTimeout as delay } from 'node:timers/promises'
import { fileURLToPath } from 'node:url'
import { createHead } from '@unhead/vue/server'
import { createPinia } from 'pinia'
import { createServer } from 'vite'
import { createRenderer, h } from 'vue'
import { createMemoryHistory, createRouter } from 'vue-router'
import type { useCheckout as CheckoutFactory } from '../src/composables/useCheckout.ts'
import type { useOrderDetail as OrderDetailFactory } from '../src/composables/useOrderDetail.ts'
import type { useBuyNowStore as BuyNowStoreFactory } from '../src/stores/buyNow.ts'

function memoryStorage() {
  const values = new Map<string, string>()
  return {
    getItem: (key: string) => values.get(key) ?? null,
    setItem: (key: string, value: string) => values.set(key, value),
    removeItem: (key: string) => values.delete(key),
  }
}

const renderer = createRenderer({
  createElement: () => ({}), createText: () => ({}), createComment: () => ({}),
  insert() {}, remove() {}, patchProp() {}, setText() {}, setElementText() {},
  parentNode: () => null, nextSibling: () => null,
})

test('checkout and order detail keep settlement within the correct browser session', { timeout: 20_000 }, async (t) => {
  const browserGlobals = ['window', 'document', 'navigator', 'localStorage', 'sessionStorage'] as const
  const originalGlobals = browserGlobals.map((name) => Object.getOwnPropertyDescriptor(globalThis, name))
  t.after(() => {
    browserGlobals.forEach((name, index) => {
      const descriptor = originalGlobals[index]
      if (descriptor) Object.defineProperty(globalThis, name, descriptor)
      else Reflect.deleteProperty(globalThis, name)
    })
  })
  const browserDocument = Object.assign(new EventTarget(), { visibilityState: 'visible' })
  const browserWindow = Object.assign(new EventTarget(), { setTimeout, clearTimeout, performance })
  const browserNavigator = { onLine: true }
  for (const [name, value] of Object.entries({ window: browserWindow, document: browserDocument, navigator: browserNavigator, localStorage: memoryStorage(), sessionStorage: memoryStorage() })) {
    Object.defineProperty(globalThis, name, { configurable: true, writable: true, value })
  }

  // Load the same composable and API client as both storefronts, without opening a server port.
  const vite = await createServer({
    root: fileURLToPath(new URL('..', import.meta.url)),
    envFile: false,
    appType: 'custom',
    server: { middlewareMode: true, hmr: false, watch: null, ws: false },
  })
  t.after(() => vite.close())
  const { useCheckout } = await vite.ssrLoadModule('/src/composables/useCheckout.ts')
  const { useOrderDetail } = await vite.ssrLoadModule('/src/composables/useOrderDetail.ts')
  const { useAppStore } = await vite.ssrLoadModule('/src/stores/app.ts')
  const { useBuyNowStore } = await vite.ssrLoadModule('/src/stores/buyNow.ts')
  const { default: i18n } = await vite.ssrLoadModule('/src/i18n/index.ts')

  const mountCheckout = async (scenario: TestContext, {
    guest = false, direct = !guest, online = guest, initialPreviewFailure, coupon = '',
  }: { guest?: boolean; direct?: boolean; online?: boolean; initialPreviewFailure?: 'network' | 'temporary' | 'validation'; coupon?: string } = {}) => {
    Object.assign(globalThis, { localStorage: memoryStorage(), sessionStorage: memoryStorage() })
    let reloads = 0
    Object.assign(browserWindow, { localStorage, sessionStorage, location: { reload: () => { reloads++ } } })
    if (!guest) localStorage.setItem('user_token', 'initial-buyer-test-token')
    const submitted = Promise.withResolvers<string | undefined>()
    const result = Promise.withResolvers<Response>()
    const response = (data: unknown) => Response.json({ status_code: 0, data })
    const requests: string[] = []
    const orderRequests: string[] = []
    const previewRequests: unknown[] = []
    const orderPath = guest ? '/api/v1/guest/orders' : '/api/v1/orders'
    const channel = { id: 12, provider_type: 'epay', channel_type: 'alipay' }
    let unavailablePath = ''
    scenario.mock.method(globalThis, 'fetch', async (input: string, opts: RequestInit) => {
      const path = new URL(input, 'http://shop.test').pathname
      requests.push(path)
      if (path === unavailablePath) return Response.json({ status_code: 500, msg: 'Test service unavailable' }, { status: 503 })
      if (path === `${orderPath}/preview`) {
        previewRequests.push(JSON.parse(String(opts.body)))
        if (previewRequests.length === 1) {
          if (initialPreviewFailure === 'network') throw new TypeError('Test connection interrupted')
          if (initialPreviewFailure === 'temporary') return Response.json({ status_code: 500, msg: 'Test service unavailable' }, { status: 502 })
          if (initialPreviewFailure === 'validation') return Response.json({ status_code: 400, msg: 'Retired SKU is unavailable' })
        }
        return response({ total_amount: '100.00', currency: 'CNY' })
      }
      if (path === '/api/v1/wallet') return response({ balance: '300.00' })
      if (path === '/api/v1/order/payment-channels') return response([channel])
      if (path === `${orderPath}/create-and-pay`) {
        orderRequests.push(path)
        submitted.resolve(new Headers(opts.headers).get('Authorization') ?? undefined)
        return result.promise
      }
      assert.fail(`Unexpected checkout request: ${path}`)
    })
    const router = createRouter({ history: createMemoryHistory(), routes: [
      { name: 'checkout', path: '/checkout', component: {} },
      { name: 'payment', path: '/pay', component: {} },
    ] })
    await router.push('/checkout?mode=buynow')
    let checkout!: ReturnType<typeof CheckoutFactory>
    let buyNow!: ReturnType<typeof BuyNowStoreFactory>
    const app = renderer.createApp({ setup() {
      useAppStore().config = { wallet_only_payment: !online, site_currency: 'CNY', payment_channels: [channel] }
      buyNow = useBuyNowStore()
      buyNow.setItem({ productId: 1, skuId: 7, slug: 'demo', title: { 'zh-CN': 'Test product' }, priceAmount: '100.00', quantity: 1, fulfillmentType: direct ? 'kafuda_gpt_direct' : 'manual' })
      checkout = useCheckout()
      return () => h('div')
    } })
    app.use(createPinia()).use(router).use(i18n).use(createHead()).mount({})
    scenario.after(() => app.unmount())
    if (coupon) checkout.couponCode.value = coupon
    if (guest) {
      checkout.guestEmail.value = 'buyer@example.test'
      checkout.guestPassword.value = 'test-order-password'
    }
    if (initialPreviewFailure) {
      for (let attempt = 0; checkout.checkoutAlert.value?.level !== 'error' && attempt < 500; attempt++) await delay(10)
      assert.equal(checkout.checkoutAlert.value?.level, 'error')
    } else {
      if (online) {
        for (let attempt = 0; checkout.paymentChannels.value.length === 0 && attempt < 500; attempt++) await delay(10)
        assert.equal(checkout.paymentChannels.value.length, 1)
        checkout.handleSelectChannel(channel)
      }
      for (let attempt = 0; !checkout.canSubmit.value && attempt < 500; attempt++) await delay(10)
      assert.equal(checkout.canSubmit.value, true)
    }
    return {
      checkout, buyNow, router, channel, submitted: submitted.promise, requests, orderRequests, previewRequests,
      failRequestsTo: (path: string) => { unavailablePath = path },
      acceptOrder: () => result.resolve(response({ order_no: 'ORDER-1' })),
      replaceBuyer: () => {
        localStorage.setItem('user_token', 'next-buyer-test-token')
        browserWindow.dispatchEvent(Object.assign(new Event('storage'), { key: 'user_token', storageArea: localStorage }))
      },
      reloads: () => reloads,
    }
  }

  for (const failure of ['network', 'temporary'] as const) {
    await t.test(`Direct online checkout recovers after an initial ${failure} preview failure`, async (scenario) => {
      const page = await mountCheckout(scenario, { online: true, initialPreviewFailure: failure, coupon: 'DIRECT-COUPON' })
      assert.equal(page.checkout.canSubmit.value, false)
      assert.equal(page.checkout.canRetryDirectPreview.value, true)
      assert.equal(page.checkout.paymentChannels.value.length, 0)
      const retry = page.checkout.retryDirectPreview()
      assert.equal(page.checkout.retryDirectPreview(), undefined)
      assert.equal(await retry, true)
      assert.equal(page.checkout.canRetryDirectPreview.value, false)
      assert.equal(page.checkout.couponCode.value, 'DIRECT-COUPON')
      assert.equal(page.buyNow.item?.skuId, 7)
      assert.equal(page.reloads(), 0)
      assert.equal(page.orderRequests.length, 0)
      assert.equal(page.previewRequests.length, 2)
      assert.deepEqual(page.previewRequests[1], page.previewRequests[0])

      for (let attempt = 0; page.checkout.paymentChannels.value.length === 0 && attempt < 100; attempt++) await delay(10)
      assert.equal(page.checkout.paymentChannels.value.length, 1)
      assert.equal(page.requests.filter((path) => path === '/api/v1/order/payment-channels').length, 1)
      page.checkout.handleSelectChannel(page.channel)
      assert.equal(page.checkout.canSubmit.value, true)
      const submit = page.checkout.handleSubmit()
      assert.equal(await page.submitted, 'Bearer initial-buyer-test-token')
      page.acceptOrder()
      await submit
      for (let attempt = 0; page.router.currentRoute.value.path !== '/pay' && attempt < 30; attempt++) await delay(10)
      assert.equal(page.router.currentRoute.value.fullPath, '/pay?order_no=ORDER-1')
      assert.equal(page.orderRequests.length, 1)
    })
  }

  for (const { direct, failure } of [
    { direct: false, failure: 'temporary' },
    { direct: true, failure: 'validation' },
  ] as const) {
    await t.test(`${direct ? 'Direct validation failure' : 'ordinary checkout'} does not expose Direct preview retry`, async (scenario) => {
      const page = await mountCheckout(scenario, { direct, online: true, initialPreviewFailure: failure })
      assert.equal(page.checkout.canRetryDirectPreview.value, false)
      assert.equal(page.checkout.retryDirectPreview(), undefined)
      assert.equal(page.previewRequests.length, 1)
      assert.equal(page.orderRequests.length, 0)
    })
  }

  for (const direct of [true, false]) {
    await t.test(`late ${direct ? 'Direct' : 'ordinary'} settlement cannot clear selection or navigate after a sibling tab switches buyers`, async (scenario) => {
      const page = await mountCheckout(scenario, { direct })
      const submit = page.checkout.handleSubmit()
      assert.equal(await page.submitted, 'Bearer initial-buyer-test-token')
      page.replaceBuyer()
      assert.equal(page.reloads(), 1)
      assert.equal(page.checkout.walletBalance.value, '0')
      assert.equal(page.checkout.canSubmit.value, false)
      await page.checkout.handleSubmit()
      page.acceptOrder()
      await submit
      assert.equal(page.orderRequests.length, 1)
      assert.equal(page.buyNow.item?.skuId, 7)
      assert.equal(page.router.currentRoute.value.path, '/checkout')
    })
  }

  for (const [payment, unavailablePath] of [
    ['wallet', '/api/v1/wallet'],
    ['online', '/api/v1/order/payment-channels'],
  ] as const) {
    await t.test(`ordinary ${payment} checkout submits after preview without a second blocking payment check`, async (scenario) => {
      const page = await mountCheckout(scenario, { direct: false, online: payment === 'online' })
      page.failRequestsTo(unavailablePath)
      const first = page.requests.length
      const submit = page.checkout.handleSubmit()
      const authorization = await Promise.race([page.submitted, submit.then(() => undefined)])
      assert.equal(authorization, 'Bearer initial-buyer-test-token')
      assert.deepEqual(page.requests.slice(first), ['/api/v1/orders/preview', '/api/v1/orders/create-and-pay'])
      page.acceptOrder()
      await submit
      for (let attempt = 0; page.router.currentRoute.value.path !== '/pay' && attempt < 30; attempt++) await delay(10)
      assert.equal(page.router.currentRoute.value.fullPath, '/pay?order_no=ORDER-1')
    })

    await t.test(`Direct ${payment} checkout requires a successful payment recheck`, async (scenario) => {
      const page = await mountCheckout(scenario, { online: payment === 'online' })
      page.failRequestsTo(unavailablePath)
      const first = page.requests.length
      await page.checkout.handleSubmit()
      assert.deepEqual(page.requests.slice(first), ['/api/v1/orders/preview', unavailablePath])
      assert.equal(page.orderRequests.length, 0)
      assert.equal(page.buyNow.item?.skuId, 7)
      assert.equal(page.router.currentRoute.value.path, '/checkout')
    })
  }

  await t.test('a successful order still opens payment after the same buyer edits the coupon', async (scenario) => {
    const page = await mountCheckout(scenario)
    const submit = page.checkout.handleSubmit()
    assert.equal(await page.submitted, 'Bearer initial-buyer-test-token')
    page.checkout.couponCode.value = 'UPDATED'
    page.acceptOrder()
    await submit
    for (let attempt = 0; page.router.currentRoute.value.path !== '/pay' && attempt < 30; attempt++) await delay(10)
    assert.equal(page.router.currentRoute.value.fullPath, '/pay?order_no=ORDER-1')
    assert.equal(page.buyNow.item, null)
    assert.equal(page.orderRequests.length, 1)
  })

  await t.test('ordinary guest checkout retains the order lookup in the current tab and opens payment', async (scenario) => {
    const page = await mountCheckout(scenario, { guest: true })
    const submit = page.checkout.handleSubmit()
    assert.equal(await page.submitted, undefined)
    page.acceptOrder()
    await submit
    for (let attempt = 0; page.router.currentRoute.value.path !== '/pay' && attempt < 30; attempt++) await delay(10)
    assert.equal(page.router.currentRoute.value.fullPath, '/pay?guest=1&order_no=ORDER-1')
    assert.deepEqual(JSON.parse(sessionStorage.getItem('guest_order_auth') || 'null'), { email: 'buyer@example.test', order_password: 'test-order-password' })
    assert.equal(page.orderRequests.length, 1)
  })

  const mountOrderDetail = async (scenario: TestContext, direct: boolean) => {
    Object.assign(globalThis, { localStorage: memoryStorage(), sessionStorage: memoryStorage() })
    localStorage.setItem('user_token', 'initial-buyer-test-token')
    browserDocument.visibilityState = 'visible'
    browserNavigator.onLine = true
    const response = (data: unknown) => Response.json({ status_code: 0, data })
    const order = {
      order_no: 'ORDER-1', status: 'pending_payment',
      children: [{ items: [{ fulfillment_type: direct ? 'kafuda_gpt_direct' : 'manual' }] }],
    }
    const originalPath = '/api/v1/orders/ORDER-1'
    const requests: string[] = []
    const refreshed = Promise.withResolvers<Response>()
    scenario.mock.method(globalThis, 'fetch', async (input: string, opts: RequestInit) => {
      const path = new URL(input, 'http://shop.test').pathname
      assert.equal(new Headers(opts.headers).get('Authorization'), 'Bearer initial-buyer-test-token')
      requests.push(path)
      if (path === originalPath) return requests.length === 1 ? response(order) : refreshed.promise
      if (path === `${originalPath}/direct-status`) return response({ stage: 'waiting_session', updated_at: '2026-11-18T08:00:00Z' })
      if (path === '/api/v1/orders/ORDER-2') return response({ ...order, order_no: 'ORDER-2', status: 'canceled' })
      assert.fail(`Unexpected order detail request: ${path}`)
    })
    const router = createRouter({ history: createMemoryHistory(), routes: [{ name: 'orderDetail', path: '/orders/:order_no', component: {} }] })
    await router.push('/orders/ORDER-1')
    let detail!: ReturnType<typeof OrderDetailFactory>
    const app = renderer.createApp({ setup() {
      detail = useOrderDetail()
      return () => h('div')
    } })
    app.use(createPinia()).use(router).use(i18n).use(createHead()).mount({})
    scenario.after(() => app.unmount())
    for (let attempt = 0; !detail.order.value && attempt < 100; attempt++) await delay(10)
    assert.equal(detail.order.value?.status, 'pending_payment')
    assert.equal(detail.directOrderNo.value, '')
    assert.deepEqual(requests, [originalPath])
    const setVisibility = (value: string) => {
      browserDocument.visibilityState = value
      browserDocument.dispatchEvent(new Event('visibilitychange'))
    }
    const setOnline = (value: boolean) => {
      browserNavigator.onLine = value
      browserWindow.dispatchEvent(new Event(value ? 'online' : 'offline'))
    }
    return { detail, requests, router, originalPath, setVisibility, setOnline, receivePayment: () => refreshed.resolve(response({ ...order, status: 'paid' })) }
  }

  await t.test('an open pending Direct order starts progress only after an authoritative paid refresh', async (scenario) => {
    const page = await mountOrderDetail(scenario, true)
    page.setVisibility('hidden')
    page.setOnline(false)
    page.setOnline(true)
    assert.deepEqual(page.requests, [page.originalPath])

    page.setVisibility('visible')
    browserWindow.dispatchEvent(new Event('pageshow'))
    assert.deepEqual(page.requests, [page.originalPath, page.originalPath])
    assert.equal(page.detail.directStatus.value, null)
    page.receivePayment()
    for (let attempt = 0; !page.detail.directStatus.value && attempt < 100; attempt++) await delay(10)
    assert.equal(page.detail.order.value?.status, 'paid')
    assert.equal(page.detail.directStatus.value?.stage, 'waiting_session')
    assert.deepEqual(page.requests, [page.originalPath, page.originalPath, `${page.originalPath}/direct-status`])
  })

  await t.test('ordinary pending order detail does not use Direct payment refresh', async (scenario) => {
    const page = await mountOrderDetail(scenario, false)
    page.setVisibility('hidden')
    page.setOnline(false)
    page.setVisibility('visible')
    page.setOnline(true)
    browserWindow.dispatchEvent(new Event('pageshow'))
    assert.deepEqual(page.requests, [page.originalPath])
    assert.equal(page.detail.order.value?.status, 'pending_payment')
    assert.equal(page.detail.directStatus.value, null)
  })

  await t.test('a pending Direct payment refresh cannot update a different open order', async (scenario) => {
    const page = await mountOrderDetail(scenario, true)
    browserWindow.dispatchEvent(new Event('pageshow'))
    assert.deepEqual(page.requests, [page.originalPath, page.originalPath])
    await page.router.push('/orders/ORDER-2')
    for (let attempt = 0; page.detail.order.value?.order_no !== 'ORDER-2' && attempt < 100; attempt++) await delay(10)
    page.receivePayment()
    await delay(10)
    assert.equal(page.detail.order.value?.order_no, 'ORDER-2')
    assert.equal(page.detail.order.value?.status, 'canceled')
    assert.equal(page.detail.directStatus.value, null)
    assert.deepEqual(page.requests, [page.originalPath, page.originalPath, '/api/v1/orders/ORDER-2'])
  })
})
