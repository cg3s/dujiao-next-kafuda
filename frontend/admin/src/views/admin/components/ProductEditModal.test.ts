import { createApp, h, nextTick, reactive, type App } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import ProductEditModal from './ProductEditModal.vue'

const api = vi.hoisted(() => ({
  getProduct: vi.fn(),
  getPaymentChannels: vi.fn(),
  getKafudaCatalog: vi.fn(),
  syncKafudaCatalog: vi.fn(),
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

const catalogOptions = [
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
})
