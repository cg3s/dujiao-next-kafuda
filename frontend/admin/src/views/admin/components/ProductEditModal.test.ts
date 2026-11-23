import { createApp, h, nextTick, reactive, type App } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import ProductEditModal from './ProductEditModal.vue'

const api = vi.hoisted(() => ({
  getProduct: vi.fn(),
  getPaymentChannels: vi.fn(),
  getKafudaCatalog: vi.fn(),
  syncKafudaCatalog: vi.fn(),
  getQuefaCatalog: vi.fn(),
  syncQuefaCatalog: vi.fn(),
  updateProduct: vi.fn(),
}))
const notifyError = vi.hoisted(() => vi.fn())

vi.mock('@/api/admin', () => ({ adminAPI: api }))
vi.mock('@/utils/notify', () => ({ notifyError }))
vi.mock('@/components/RichEditor.vue', () => ({ default: { render: () => null } }))
vi.mock('@/components/admin/MediaPicker.vue', () => ({ default: { render: () => null } }))
vi.mock('vue-i18n', async (importOriginal) => {
  const actual = await importOriginal<typeof import('vue-i18n')>()
  const { ref } = await import('vue')
  return { ...actual, useI18n: () => ({ t: (key: string) => key, locale: ref('zh-CN') }) }
})
vi.mock('@/components/ui/dialog', async () => {
  const { defineComponent } = await import('vue')
  const passthrough = defineComponent({
    inheritAttrs: false,
    setup: (_, { slots }) => () => slots.default?.(),
  })
  return {
    Dialog: defineComponent({
      props: { open: Boolean },
      setup: (props, { slots }) => () => props.open ? slots.default?.() : null,
    }),
    DialogScrollContent: passthrough,
    DialogHeader: passthrough,
    DialogTitle: passthrough,
  }
})

const deferredProduct = () => {
  let resolve!: (value: ReturnType<typeof productResponse>) => void
  let reject!: (error: Error) => void
  const promise = new Promise<ReturnType<typeof productResponse>>((resolvePromise, rejectPromise) => {
    resolve = resolvePromise
    reject = rejectPromise
  })
  return { promise, resolve, reject }
}

const productResponse = (id: number, country: string) => ({
  data: {
    data: {
      id,
      slug: `product-${id}`,
      title: { 'zh-CN': `product-${id}` },
      category_id: 1,
      fulfillment_type: 'kafuda_gpt_direct',
      skus: [{
        id,
        sku_code: `KGPT_${country}`,
        spec_values: { plan: 'plus', country },
        price_amount: 10,
        cost_price_amount: 0,
        direct_cost_configured: true,
        is_active: true,
      }],
    },
  },
})

const catalogOptions: Array<{
  sku_code: string
  plan: string
  country: string
  label?: string
  available: boolean
  catalog_revision: number
  estimated_cost_cny?: number
}> = [
  { sku_code: 'KGPT_PH', plan: 'plus', country: 'PH', available: true, catalog_revision: 0 },
  { sku_code: 'KGPT_US', plan: 'plus', country: 'US', available: true, catalog_revision: 0 },
]
const catalogResponse = (options = catalogOptions) => ({ data: { data: { options } } })

let app: App | undefined
let host: HTMLDivElement

beforeEach(() => {
  vi.resetAllMocks()
  host = document.createElement('div')
  document.body.append(host)
  api.getPaymentChannels.mockResolvedValue({ data: { data: [] } })
  api.getKafudaCatalog.mockResolvedValue(catalogResponse())
  api.syncKafudaCatalog.mockResolvedValue(catalogResponse())
  api.updateProduct.mockResolvedValue({})
})

afterEach(() => {
  app?.unmount()
  app = undefined
  host.remove()
})

const mountModal = () => {
  const context = reactive({ open: false, productId: null as number | null })
  app = createApp({
    render: () => h(ProductEditModal, {
      modelValue: context.open,
      productId: context.productId,
      categories: [{
        id: 1, parent_id: 0, slug: 'category', name: { 'zh-CN': 'category' },
        icon: '', sort_order: 0, is_active: true, created_at: '',
      }],
      siteCurrency: 'USD',
      supportedLocales: ['zh-CN'],
      'onUpdate:modelValue': (open: boolean) => { context.open = open },
    }),
  })
  app.mount(host)
  return context
}

const flushRequests = async () => {
  await new Promise<void>((resolve) => setTimeout(resolve, 0))
  await nextTick()
}

const submitUpdate = () => {
  const form = host.querySelector('form')
  expect(form).not.toBeNull()
  form!.dispatchEvent(new Event('submit', { cancelable: true }))
  expect(api.updateProduct).toHaveBeenCalledOnce()
  return JSON.parse(JSON.stringify(api.updateProduct.mock.calls[0]?.[1]))
}

describe('ProductEditModal product loading', () => {
  it('ignores a late response and omits unchanged Direct product and SKU activation', async () => {
    const first = deferredProduct()
    const second = deferredProduct()
    api.getProduct.mockImplementation((id) => id === 1 ? first.promise : second.promise)
    const context = mountModal()
    context.productId = 1
    context.open = true
    await nextTick()
    expect(api.getProduct.mock.calls).toEqual([[1]])
    expect(host.querySelector('form')).toBeNull()

    context.productId = 2
    await nextTick()
    expect(api.getProduct.mock.calls).toEqual([[1], [2]])
    second.resolve(productResponse(2, 'US'))
    await flushRequests()
    first.resolve(productResponse(1, 'PH'))
    await flushRequests()

    expect(api.getKafudaCatalog).toHaveBeenCalledTimes(1)
    expect(host.querySelector<HTMLInputElement>('input[placeholder="admin.products.form.slugPlaceholder"]')?.value).toBe('product-2')
    const payload = submitUpdate()
    expect(api.updateProduct.mock.calls[0]?.[0]).toBe(2)
    expect(payload).toMatchObject({ slug: 'product-2', fulfillment_type: 'kafuda_gpt_direct' })
    expect(payload).not.toHaveProperty('skus')
    expect(payload).not.toHaveProperty('is_active')
  })

  it('ignores a stale failure after closing and reopening the same product', async () => {
    const first = deferredProduct()
    const second = deferredProduct()
    api.getProduct.mockReturnValueOnce(first.promise).mockReturnValueOnce(second.promise)
    const context = mountModal()
    context.productId = 1
    context.open = true
    await nextTick()
    context.open = false
    await nextTick()
    context.open = true
    await nextTick()
    first.reject(new Error('stale failure'))
    await flushRequests()
    expect(context.open).toBe(true)
    expect(notifyError).not.toHaveBeenCalled()
    expect(host.querySelector('form')).toBeNull()

    second.resolve(productResponse(1, 'PH'))
    await flushRequests()
    expect(host.querySelector('form')).not.toBeNull()
  })
})

describe('ProductEditModal Direct SKU edits', () => {
  it('explains a missing cost and accepts an explicitly entered zero', async () => {
    const product = productResponse(1, 'PH')
    product.data.data.skus[0]!.direct_cost_configured = false
    api.getProduct.mockResolvedValue(product)
    const context = mountModal()
    context.productId = 1
    context.open = true
    await flushRequests()

    const cost = host.querySelector<HTMLInputElement>('input[placeholder="admin.products.form.skuCostPricePlaceholder"]')
    expect(cost?.value).toBe('')
    expect(host.textContent).toContain('kafuda.costMissing')
    host.querySelector('form')!.dispatchEvent(new Event('submit', { cancelable: true }))
    expect(api.updateProduct).not.toHaveBeenCalled()

    cost!.value = '0'
    cost!.dispatchEvent(new Event('input', { bubbles: true }))
    await nextTick()
    expect(host.textContent).not.toContain('kafuda.costMissing')
    const payload = submitUpdate()
    expect(payload.skus).toHaveLength(1)
    expect(payload.skus[0]).toMatchObject({ id: 1, cost_price_amount: 0 })
  })

  it('saves an explicitly confirmed historical cost at the existing amount after catalog refresh', async () => {
    const product = productResponse(1, 'PH')
    Object.assign(product.data.data.skus[0]!, { cost_price_amount: 45, direct_cost_configured: false })
    product.data.data.skus.push(productResponse(2, 'US').data.data.skus[0]!)
    api.getProduct.mockResolvedValue(product)
    const context = mountModal()
    context.productId = 1
    context.open = true
    await flushRequests()

    const findButton = (text: string) => [...host.querySelectorAll('button')].find((button) => button.textContent?.trim() === text)
    expect(host.textContent).toContain('kafuda.costUnconfirmed')
    const confirm = findButton('kafuda.confirmCost')
    expect(confirm).toBeDefined()
    confirm!.click()
    await nextTick()
    expect(host.textContent).not.toContain('kafuda.costUnconfirmed')
    expect(api.updateProduct).not.toHaveBeenCalled()

    findButton('kafuda.sync')!.click()
    await flushRequests()
    expect(host.textContent).not.toContain('kafuda.costUnconfirmed')
    const payload = submitUpdate()
    expect(payload.skus).toHaveLength(1)
    expect(payload.skus[0]).toMatchObject({ id: 1, sku_code: 'KGPT_PH', cost_price_amount: 45 })
    for (const field of ['price_amount', 'is_active', 'sort_order', 'direct_cost_configured']) {
      expect(payload.skus[0]).not.toHaveProperty(field)
    }
  })

  it('shows the catalog block once and replaces it after refresh', async () => {
    api.getProduct.mockResolvedValue(productResponse(1, 'PH'))
    api.getKafudaCatalog.mockResolvedValue({ data: { data: { options: catalogOptions, stale: true, sales_block_reason: 'catalog_stale' } } })
    api.syncKafudaCatalog.mockResolvedValue({ data: { data: { options: catalogOptions, stale: false, sales_block_reason: 'card_product_unavailable' } } })
    const context = mountModal()
    context.productId = 1
    context.open = true
    await flushRequests()

    const staleMessage = 'kafuda.salesBlockReasons.catalog_stale'
    expect([...host.querySelectorAll('p')].filter((element) => element.textContent?.trim() === staleMessage)).toHaveLength(1)

    const refresh = [...host.querySelectorAll('button')].find((button) => button.textContent?.trim() === 'kafuda.sync')
    expect(refresh).toBeDefined()
    refresh!.click()
    await flushRequests()

    expect(host.textContent).not.toContain(staleMessage)
    expect(host.querySelector('p[role="status"]')?.textContent?.trim()).toBe('kafuda.salesBlockReasons.card_product_unavailable')
  })

  it('keeps a catalog read error visible and retries without refreshing the supplier', async () => {
    api.getProduct.mockResolvedValue(productResponse(1, 'PH'))
    api.getKafudaCatalog.mockRejectedValueOnce(Object.assign(new Error('Access denied'), { __notified: true }))
    api.getKafudaCatalog.mockResolvedValueOnce({ data: { data: { options: catalogOptions, sales_block_reason: 'worker_not_ready' } } })
    const context = mountModal()
    context.productId = 1
    context.open = true
    await flushRequests()

    expect(host.querySelector('[role="alert"]')?.textContent?.trim()).toBe('Access denied')
    expect(host.textContent).not.toContain('kafuda.unavailable')
    expect(host.textContent).not.toContain('kafuda.salesBlockReasons.catalog_stale')
    expect(notifyError).not.toHaveBeenCalled()

    const retry = [...host.querySelectorAll('button')].find((button) => button.textContent?.trim() === 'kafuda.retryRead')
    expect(retry).toBeDefined()
    retry!.click()
    await flushRequests()
    expect(api.getKafudaCatalog).toHaveBeenCalledTimes(2)
    expect(api.syncKafudaCatalog).not.toHaveBeenCalled()
    expect(host.querySelector('[role="alert"]')).toBeNull()
    expect(host.querySelector('p[role="status"]')?.textContent?.trim()).toBe('kafuda.salesBlockReasons.worker_not_ready')
  })

  it('keeps the last loaded catalog and pending edits when refreshing fails', async () => {
    api.getProduct.mockResolvedValue(productResponse(1, 'PH'))
    api.syncKafudaCatalog.mockRejectedValueOnce(Object.assign(new Error('Rate limited'), { __notified: true }))
    const context = mountModal()
    context.productId = 1
    context.open = true
    await flushRequests()

    const cost = host.querySelector<HTMLInputElement>('input[placeholder="admin.products.form.skuCostPricePlaceholder"]')
    cost!.value = '5'
    cost!.dispatchEvent(new Event('input', { bubbles: true }))
    const refresh = [...host.querySelectorAll('button')].find((button) => button.textContent?.trim() === 'kafuda.sync')
    refresh!.click()
    await flushRequests()

    expect(host.querySelector('[role="alert"]')?.textContent?.trim()).toBe('Rate limited')
    expect(host.textContent).not.toContain('kafuda.unavailable')
    const payload = submitUpdate()
    expect(payload.skus).toHaveLength(1)
    expect(payload.skus[0]).toMatchObject({ id: 1, cost_price_amount: 5 })
  })

  it('submits only edited fields from each region after refreshing the catalog', async () => {
    const product = productResponse(1, 'PH')
    product.data.data.skus.push(productResponse(2, 'US').data.data.skus[0]!)
    api.getProduct.mockResolvedValue(product)
    const context = mountModal()
    context.productId = 1
    context.open = true
    await flushRequests()

    const changeInput = async (placeholder: string, value: string) => {
      const input = host.querySelector<HTMLInputElement>(`input[placeholder="admin.products.form.${placeholder}"]`)
      expect(input).not.toBeNull()
      input!.value = value
      input!.dispatchEvent(new Event('input', { bubbles: true }))
      await nextTick()
    }
    const selectRegion = async (index: number) => {
      const tab = host.querySelectorAll('[role="tab"]')[index]
      expect(tab).toBeDefined()
      tab!.dispatchEvent(new MouseEvent('mousedown', { bubbles: true, button: 0 }))
      await nextTick()
      expect(tab!.getAttribute('aria-selected')).toBe('true')
    }
    await selectRegion(0)
    await changeInput('skuPricePlaceholder', '15')
    await selectRegion(1)
    await changeInput('skuCostPricePlaceholder', '5')
    const refresh = [...host.querySelectorAll('button')].find((button) => button.textContent?.trim() === 'kafuda.sync')
    expect(refresh).toBeDefined()
    refresh!.click()
    await flushRequests()
    expect(api.syncKafudaCatalog).toHaveBeenCalledOnce()

    const payload = submitUpdate()
    expect(payload.skus).toHaveLength(2)
    const [philippines, unitedStates] = payload.skus
    expect(philippines).toMatchObject({ id: 1, sku_code: 'KGPT_PH', price_amount: 15, direct_catalog_revision: 0 })
    expect(philippines).not.toHaveProperty('cost_price_amount')
    expect(unitedStates).toMatchObject({ id: 2, sku_code: 'KGPT_US', cost_price_amount: 5, direct_catalog_revision: 0 })
    expect(unitedStates).not.toHaveProperty('price_amount')
    for (const sku of payload.skus) {
      expect(sku).not.toHaveProperty('is_active')
      expect(sku).not.toHaveProperty('sort_order')
    }
    expect(payload).not.toHaveProperty('is_active')
  })

  it('submits intentional activation using the revision shown for review', async () => {
    api.getProduct.mockResolvedValue(productResponse(1, 'PH'))
    api.getKafudaCatalog.mockResolvedValue(catalogResponse([{ ...catalogOptions[0]!, catalog_revision: 2 }]))
    const context = mountModal()
    context.productId = 1
    context.open = true
    await flushRequests()
    expect(host.textContent).toContain('kafuda.review')

    const price = host.querySelector<HTMLInputElement>('input[placeholder="admin.products.form.skuPricePlaceholder"]')
    const activate = price?.closest('.rounded-lg')?.querySelector<HTMLButtonElement>('button[role="switch"]')
    expect(activate?.getAttribute('aria-checked')).toBe('false')
    activate!.click()
    host.querySelector<HTMLButtonElement>('#is_active')?.click()
    await nextTick()

    const payload = submitUpdate()
    expect(payload.is_active).toBe(false)
    expect(payload.skus).toHaveLength(1)
    expect(payload.skus[0]).toMatchObject({ id: 1, sku_code: 'KGPT_PH', is_active: true, direct_catalog_revision: 2 })
    for (const field of ['price_amount', 'cost_price_amount', 'sort_order']) {
      expect(payload.skus[0]).not.toHaveProperty(field)
    }
  })

  it('presets direct cost prices from estimated_cost_cny and applies markup to all SKUs', async () => {
    const product = productResponse(1, 'PH')
    product.data.data.skus[0]!.cost_price_amount = 100
    product.data.data.skus[0]!.direct_cost_configured = true
    api.getProduct.mockResolvedValue(product)
    api.getKafudaCatalog.mockResolvedValue(catalogResponse([
      { ...catalogOptions[0]!, estimated_cost_cny: 120 },
      { ...catalogOptions[1]!, estimated_cost_cny: 50.5 },
    ]))
    const context = mountModal()
    context.productId = 1
    context.open = true
    await flushRequests()

    const markupInput = host.querySelector<HTMLInputElement>('input[aria-label="kafuda.markupLabel"]')
    expect(markupInput?.value).toBe('10')

    const applyButton = [...host.querySelectorAll('button')].find((b) => b.textContent?.trim() === 'kafuda.applyMarkup')
    expect(applyButton).toBeTruthy()
    applyButton!.click()
    await nextTick()

    const payload = submitUpdate()
    expect(payload.skus).toHaveLength(2)
    // Existing PH SKU kept its configured cost (100) and got +10% price (110).
    expect(payload.skus[0]).toMatchObject({ sku_code: 'KGPT_PH', price_amount: 110 })
    // New US SKU got preset cost from estimated_cost_cny (50.5) and +10% price (55.55).
    expect(payload.skus[1]).toMatchObject({ sku_code: 'KGPT_US', cost_price_amount: 50.5, price_amount: 55.55 })
  })

  it('populates editable direct SKU code and spec name from catalog defaults', async () => {
    api.getProduct.mockResolvedValue(productResponse(1, 'PH'))
    api.getKafudaCatalog.mockResolvedValue(catalogResponse([
      { ...catalogOptions[0]!, sku_code: 'GPT-PLUS-PH', label: 'ChatGPT Plus (PH)' },
      { ...catalogOptions[1]!, sku_code: '', plan: 'pro_monthly', country: 'US', label: 'ChatGPT Pro (US)' },
    ]))
    const context = mountModal()
    context.productId = 1
    context.open = true
    await flushRequests()

    const codeInput = host.querySelector<HTMLInputElement>('input[placeholder="admin.products.form.skuCodePlaceholder"]')
    const specInput = host.querySelector<HTMLInputElement>('input[placeholder="admin.products.form.skuSpecPlaceholder"]')
    expect(codeInput?.value).toBe('KGPT_PH')
    expect(specInput?.value).toBe('ChatGPT Plus (PH)')

    codeInput!.value = 'CUSTOM-GPT-PH'
    codeInput!.dispatchEvent(new Event('input', { bubbles: true }))
    specInput!.value = '菲律宾 Plus 月卡'
    specInput!.dispatchEvent(new Event('input', { bubbles: true }))
    await nextTick()

    const tabs = host.querySelectorAll('[role="tab"]')
    tabs[1]!.dispatchEvent(new MouseEvent('mousedown', { bubbles: true, button: 0 }))
    await nextTick()
    const usCodeInput = host.querySelector<HTMLInputElement>('input[placeholder="admin.products.form.skuCodePlaceholder"]')
    const usSpecInput = host.querySelector<HTMLInputElement>('input[placeholder="admin.products.form.skuSpecPlaceholder"]')
    expect(usCodeInput?.value).toBe('GPT-PRO-MONTHLY-US')
    expect(usSpecInput?.value).toBe('ChatGPT Pro (US)')

    usCodeInput!.value = 'CUSTOM-GPT-US'
    usCodeInput!.dispatchEvent(new Event('input', { bubbles: true }))
    await nextTick()

    const payload = submitUpdate()
    expect(payload.skus).toHaveLength(2)
    expect(payload.skus[0]).toMatchObject({
      id: 1,
      sku_code: 'CUSTOM-GPT-PH',
      spec_values: expect.objectContaining({ plan: 'plus', country: 'PH', 'zh-CN': '菲律宾 Plus 月卡' }),
    })
    expect(payload.skus[1]).toMatchObject({
      sku_code: 'CUSTOM-GPT-US',
      spec_values: expect.objectContaining({ plan: 'pro_monthly', country: 'US', 'zh-CN': 'ChatGPT Pro (US)' }),
    })
  })
})

describe('ProductEditModal Quefa Hosted SKU auto-sync', () => {
  it('auto-syncs Quefa catalog into SKUs on edit, preserves saved prices, blocks quote_required, and supports batch markup', async () => {
    api.getProduct.mockResolvedValue({
      data: {
        data: {
          id: 9,
          slug: 'quefa-plus',
          title: { 'zh-CN': 'Quefa Plus' },
          category_id: 1,
          fulfillment_type: 'quefa_hosted',
          skus: [{
            id: 91,
            sku_code: 'chatgpt_plus_cdk_1m',
            spec_values: { product_code: 'chatgpt_plus_cdk_1m', 'zh-CN': 'Plus 月卡自定义' },
            price_amount: 145,
            cost_price_amount: 112,
            is_active: true,
            sort_order: 0,
          }],
        },
      },
    })
    api.getQuefaCatalog.mockResolvedValue({
      data: {
        data: {
          stale: false,
          synced_at: '2026-11-24T00:00:00Z',
          readiness: { ready: true, block_reasons: [] },
          products: [
            {
              product_code: 'chatgpt_plus_cdk_1m',
              title: 'ChatGPT Plus 月卡',
              supply_price: '110.00',
              min_sale_price: '115.00',
              quote_required: false,
              currency: 'CNY',
              available: true,
            },
            {
              product_code: 'chatgpt_pro_cdk_1m',
              title: 'ChatGPT Pro 月卡',
              supply_price: '1000.00',
              min_sale_price: '1050.00',
              quote_required: false,
              currency: 'CNY',
              available: true,
            },
            {
              product_code: 'chatgpt_plus_us_direct_1m',
              title: 'ChatGPT Plus US 直充',
              supply_price: '0.00',
              min_sale_price: '0.00',
              quote_required: true,
              currency: 'CNY',
              available: true,
            },
          ],
        },
      },
    })

    const context = mountModal()
    context.productId = 9
    context.open = true
    await flushRequests()

    expect(api.getQuefaCatalog).toHaveBeenCalledTimes(1)
    const codeInputs = [...host.querySelectorAll<HTMLInputElement>('input[placeholder="admin.products.form.skuCodePlaceholder"]')]
    expect(codeInputs.map((el) => el.value)).toEqual([
      'chatgpt_plus_cdk_1m',
      'chatgpt_pro_cdk_1m',
      'chatgpt_plus_us_direct_1m',
    ])

    const priceInputs = [...host.querySelectorAll<HTMLInputElement>('input[placeholder="admin.products.form.skuPricePlaceholder"]')]
    const costInputs = [...host.querySelectorAll<HTMLInputElement>('input[placeholder="admin.products.form.skuCostPricePlaceholder"]')]
    // Existing SKU preserves saved price & cost; newly synced fixed-price SKU prefills supply cost and starts at price 0 (inactive)
    expect(priceInputs[0]?.value).toBe('145')
    expect(costInputs[0]?.value).toBe('112')
    expect(priceInputs[1]?.value).toBe('0')
    expect(costInputs[1]?.value).toBe('1000')
    expect(host.textContent).toContain('quefa.skuQuoteRequired')

    // Apply 10% markup to all SKUs with cost
    const markupBtn = [...host.querySelectorAll('button')].find((b) => b.textContent?.trim() === 'kafuda.applyMarkup')
    expect(markupBtn).toBeTruthy()
    markupBtn!.click()
    await nextTick()

    const payload = submitUpdate()
    expect(payload.skus).toHaveLength(3)
    expect(payload.skus[0]).toMatchObject({
      id: 91,
      sku_code: 'chatgpt_plus_cdk_1m',
      price_amount: 123.2,
      cost_price_amount: 112,
      is_active: true,
    })
    expect(payload.skus[1]).toMatchObject({
      sku_code: 'chatgpt_pro_cdk_1m',
      price_amount: 1100,
      cost_price_amount: 1000,
      is_active: false,
    })
    expect(payload.skus[2]).toMatchObject({
      sku_code: 'chatgpt_plus_us_direct_1m',
      price_amount: 0,
      is_active: false,
    })
  })
})
