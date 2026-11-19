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

test('checkout keeps order settlement within the correct browser session', { timeout: 20_000 }, async (t) => {
  const browserGlobals = ['window', 'document', 'localStorage', 'sessionStorage'] as const
  const originalGlobals = browserGlobals.map((name) => Object.getOwnPropertyDescriptor(globalThis, name))
  t.after(() => {
    browserGlobals.forEach((name, index) => {
      const descriptor = originalGlobals[index]
      if (descriptor) Object.defineProperty(globalThis, name, descriptor)
      else Reflect.deleteProperty(globalThis, name)
    })
  })
  const browserDocument = new EventTarget()
  const browserWindow = Object.assign(new EventTarget(), { setTimeout, clearTimeout, performance })
  for (const [name, value] of Object.entries({ window: browserWindow, document: browserDocument, localStorage: memoryStorage(), sessionStorage: memoryStorage() })) {
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
  const { useAppStore } = await vite.ssrLoadModule('/src/stores/app.ts')
  const { useBuyNowStore } = await vite.ssrLoadModule('/src/stores/buyNow.ts')
  const { default: i18n } = await vite.ssrLoadModule('/src/i18n/index.ts')

  const mountCheckout = async (scenario: TestContext, guest = false) => {
    Object.assign(globalThis, { localStorage: memoryStorage(), sessionStorage: memoryStorage() })
    let reloads = 0
    Object.assign(browserWindow, { localStorage, sessionStorage, location: { reload: () => { reloads++ } } })
    if (!guest) localStorage.setItem('user_token', 'initial-buyer-test-token')
    const submitted = Promise.withResolvers<string | undefined>()
    const result = Promise.withResolvers<Response>()
    const response = (data: unknown) => Response.json({ status_code: 0, data })
    const orderRequests: string[] = []
    const orderPath = guest ? '/api/v1/guest/orders' : '/api/v1/orders'
    scenario.mock.method(globalThis, 'fetch', async (input: string, opts: RequestInit) => {
      const path = new URL(input, 'http://shop.test').pathname
      if (path === `${orderPath}/preview`) return response({ total_amount: '100.00', currency: 'CNY' })
      if (path === '/api/v1/wallet') return response({ balance: '300.00' })
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
    const channel = { id: 12, provider_type: 'epay', channel_type: 'alipay' }
    const app = renderer.createApp({ setup() {
      useAppStore().config = { wallet_only_payment: !guest, site_currency: 'CNY', payment_channels: [channel] }
      buyNow = useBuyNowStore()
      buyNow.setItem({ productId: 1, skuId: 7, slug: 'demo', title: { 'zh-CN': 'Test product' }, priceAmount: '100.00', quantity: 1, fulfillmentType: guest ? 'manual' : 'kafuda_gpt_direct' })
      checkout = useCheckout()
      return () => h('div')
    } })
    app.use(createPinia()).use(router).use(i18n).use(createHead()).mount({})
    scenario.after(() => app.unmount())
    if (guest) {
      checkout.guestEmail.value = 'buyer@example.test'
      checkout.guestPassword.value = 'test-order-password'
      checkout.handleSelectChannel(channel)
    }
    for (let attempt = 0; !checkout.canSubmit.value && attempt < 500; attempt++) await delay(10)
    assert.equal(checkout.canSubmit.value, true)
    return {
      checkout, buyNow, router, submitted: submitted.promise, orderRequests,
      acceptOrder: () => result.resolve(response({ order_no: 'ORDER-1' })),
      replaceBuyer: () => {
        localStorage.setItem('user_token', 'next-buyer-test-token')
        browserWindow.dispatchEvent(Object.assign(new Event('storage'), { key: 'user_token', storageArea: localStorage }))
      },
      reloads: () => reloads,
    }
  }

  await t.test('late Direct settlement cannot clear selection or navigate after a sibling tab switches buyers', async (scenario) => {
    const page = await mountCheckout(scenario)
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
    const page = await mountCheckout(scenario, true)
    const submit = page.checkout.handleSubmit()
    assert.equal(await page.submitted, undefined)
    page.acceptOrder()
    await submit
    for (let attempt = 0; page.router.currentRoute.value.path !== '/pay' && attempt < 30; attempt++) await delay(10)
    assert.equal(page.router.currentRoute.value.fullPath, '/pay?guest=1&order_no=ORDER-1')
    assert.deepEqual(JSON.parse(sessionStorage.getItem('guest_order_auth') || 'null'), { email: 'buyer@example.test', order_password: 'test-order-password' })
    assert.equal(page.orderRequests.length, 1)
  })
})
