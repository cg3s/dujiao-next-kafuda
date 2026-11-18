import { createApp, h, nextTick, reactive, type App } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import ProductEditModal from './ProductEditModal.vue'

const api = vi.hoisted(() => ({
  getProduct: vi.fn(),
  getPaymentChannels: vi.fn(),
  getKafudaCatalog: vi.fn(),
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

let app: App | undefined
let host: HTMLDivElement

beforeEach(() => {
  vi.resetAllMocks()
  host = document.createElement('div')
  document.body.append(host)
  api.getPaymentChannels.mockResolvedValue({ data: { data: [] } })
  api.getKafudaCatalog.mockResolvedValue({ data: { data: { options: [
    { sku_code: 'KGPT_PH', plan: 'plus', country: 'PH', available: true },
    { sku_code: 'KGPT_US', plan: 'plus', country: 'US', available: true },
  ] } } })
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

describe('ProductEditModal product loading', () => {
  it('ignores a late response after switching products and submits the current Direct SKUs', async () => {
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
    host.querySelector('form')?.dispatchEvent(new Event('submit', { cancelable: true }))
    expect(api.updateProduct).toHaveBeenCalledWith(2, expect.objectContaining({
      skus: expect.arrayContaining([expect.objectContaining({ id: 2, sku_code: 'KGPT_US', is_active: true })]),
    }))
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
