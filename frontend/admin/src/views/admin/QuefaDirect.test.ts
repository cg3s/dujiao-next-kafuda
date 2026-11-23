import { createApp, nextTick } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const api = vi.hoisted(() => ({
  getQuefaConfig: vi.fn(),
  getQuefaCatalog: vi.fn(),
  updateQuefaConfig: vi.fn(),
  testQuefaConfig: vi.fn(),
  syncQuefaCatalog: vi.fn(),
}))

const notify = vi.hoisted(() => ({
  notifySuccess: vi.fn(),
  notifyError: vi.fn(),
}))

const auth = vi.hoisted(() => ({
  hasPermission: vi.fn(() => true),
}))

vi.mock('vue-i18n', async (importOriginal) => {
  const actual = await importOriginal<typeof import('vue-i18n')>()
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key, te: () => true }),
  }
})

vi.mock('@/api/admin', () => ({
  adminAPI: api,
}))

vi.mock('@/utils/notify', () => notify)

vi.mock('@/stores/auth', () => ({
  useAdminAuthStore: () => auth,
}))

import QuefaDirect from './QuefaDirect.vue'

const flush = async () => {
  await Promise.resolve()
  await nextTick()
  await Promise.resolve()
  await nextTick()
}

describe('QuefaDirect', () => {
  let host: HTMLDivElement
  let unmount: (() => void) | null = null

  beforeEach(() => {
    vi.clearAllMocks()
    auth.hasPermission.mockReturnValue(true)
    host = document.createElement('div')
    document.body.appendChild(host)
  })

  afterEach(() => {
    unmount?.()
    unmount = null
    host.remove()
  })

  const mountPage = async () => {
    const app = createApp(QuefaDirect)
    app.mount(host)
    unmount = () => app.unmount()
    await flush()
  }

  const findButton = (label: string) =>
    [...host.querySelectorAll('button')].find((btn) => btn.textContent?.trim() === label)

  it('loads config and catalog, renders products, and omits blank secrets on save', async () => {
    const cfgPayload = {
      config: {
        source: 'database',
        version: 2,
        api_base_url: 'https://api.example.com',
        partner_id: 'partner_1',
        key_id: 'key_1',
        client_secret_configured: true,
        webhook_secret_configured: true,
        collection_mode: 'platform_collect',
        sales_enabled: true,
        allowed_pay_domains: ['pay.example.com'],
        catalog_ttl_seconds: 300,
        poll_interval_seconds: 10,
        poll_review_after_seconds: 86400,
        webhook_max_skew_seconds: 300,
      },
      readiness: {
        ready: true,
        block_reasons: [],
      },
    }
    const catPayload = {
      revision: 4,
      stale: false,
      synced_at: '2026-01-01T00:00:00Z',
      products: [
        {
          product_code: 'gpt-plus-1m',
          title: 'GPT Plus 1M',
          currency: 'CNY',
          available: true,
          supply_price: '80.00',
          min_sale_price: '85.00',
        },
      ],
    }

    api.getQuefaConfig.mockResolvedValue({ data: { data: cfgPayload } })
    api.getQuefaCatalog.mockResolvedValue({ data: { data: catPayload } })
    api.updateQuefaConfig.mockResolvedValue({ data: { data: cfgPayload } })

    await mountPage()

    expect(api.getQuefaConfig).toHaveBeenCalledTimes(1)
    expect(api.getQuefaCatalog).toHaveBeenCalledTimes(1)
    expect(host.textContent).toContain('gpt-plus-1m')
    expect(host.textContent).toContain('GPT Plus 1M')

    const saveBtn = findButton('quefa.saveConfig')
    expect(saveBtn).toBeTruthy()
    saveBtn!.click()
    await flush()

    expect(api.updateQuefaConfig).toHaveBeenCalledTimes(1)
    const payload = api.updateQuefaConfig.mock.calls[0]?.[0]
    expect(payload).toBeTruthy()
    expect(payload.api_base_url).toBe('https://api.example.com')
    expect(payload.partner_id).toBe('partner_1')
    expect(payload.client_secret).toBeUndefined()
    expect(payload.webhook_secret).toBeUndefined()
    expect(payload.allowed_pay_domains).toEqual(['pay.example.com'])
    expect(notify.notifySuccess).toHaveBeenCalledWith('quefa.saveSuccess')
  })

  it('triggers read-only connection test and catalog sync', async () => {
    const cfgPayload = {
      config: {
        source: 'database',
        version: 1,
        api_base_url: 'https://api.example.com',
        partner_id: 'partner_1',
        key_id: 'key_1',
        client_secret_configured: true,
        webhook_secret_configured: false,
        collection_mode: 'agent_collect',
        sales_enabled: false,
        allowed_pay_domains: [],
        catalog_ttl_seconds: 300,
        poll_interval_seconds: 10,
        poll_review_after_seconds: 86400,
        webhook_max_skew_seconds: 300,
      },
      readiness: {
        ready: false,
        block_reasons: ['webhook_secret_missing'],
      },
    }

    api.getQuefaConfig.mockResolvedValue({ data: { data: cfgPayload } })
    api.getQuefaCatalog.mockResolvedValue({ data: { data: { revision: 1, stale: true, products: [] } } })
    api.testQuefaConfig.mockResolvedValue({ data: { data: { product_count: 2 } } })
    api.syncQuefaCatalog.mockResolvedValue({ data: { data: { revision: 2, stale: false, products: [] } } })

    await mountPage()

    const testBtn = findButton('quefa.testConnection')
    expect(testBtn).toBeTruthy()
    testBtn!.click()
    await flush()
    expect(api.testQuefaConfig).toHaveBeenCalledTimes(1)
    expect(notify.notifySuccess).toHaveBeenCalledWith('quefa.testSuccess')

    const syncBtn = findButton('quefa.syncCatalog')
    expect(syncBtn).toBeTruthy()
    syncBtn!.click()
    await flush()
    expect(api.syncQuefaCatalog).toHaveBeenCalledTimes(1)
    expect(notify.notifySuccess).toHaveBeenCalledWith('quefa.syncSuccess')
  })
})
