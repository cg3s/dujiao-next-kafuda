import { createApp, nextTick } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const api = vi.hoisted(() => ({
  getKafudaConfig: vi.fn(),
  updateKafudaConfig: vi.fn(),
  testKafudaConfig: vi.fn(),
  syncKafudaCatalog: vi.fn(),
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

import KafudaDirect from './KafudaDirect.vue'

const flush = async () => {
  await Promise.resolve()
  await nextTick()
  await Promise.resolve()
  await nextTick()
}

describe('KafudaDirect', () => {
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
    const app = createApp(KafudaDirect)
    app.mount(host)
    unmount = () => app.unmount()
    await flush()
  }

  const findButton = (label: string) =>
    [...host.querySelectorAll('button')].find((btn) => btn.textContent?.trim() === label)

  it('loads config, lets operator pick a card, and omits blank api_key on save', async () => {
    const initial = {
      api_base_url: 'https://kafuda.com',
      has_api_key: true,
      product_code: '',
      max_cost_cents: 2500,
      allowed_kinds: ['new'],
      sales_enabled: false,
      reference_lookup_verified: true,
      catalog_ttl_seconds: 3600,
      catalog_refresh_interval_seconds: 0,
      poll_review_after_seconds: 86400,
      poll_interval_seconds: 10,
      config_source: 'database',
      version: 2,
      configured: true,
      gateway_config_invalid: false,
      worker_ready: true,
      sales_block_reason: 'sales_disabled',
      catalog_enabled: true,
      catalog_stale: false,
      catalog_options_count: 12,
      cards: [
        {
          product_code: 'XL537872',
          name: 'Visa XL',
          restricted: false,
          gpt_recommended: true,
          open_fee_cents: 100,
          min_amount_cents: 500,
        },
        {
          product_code: 'PP5450RC',
          name: 'Restricted Card',
          restricted: true,
          gpt_recommended: false,
          open_fee_cents: 100,
          min_amount_cents: 500,
        },
      ],
    }

    api.getKafudaConfig.mockResolvedValue({ data: { data: initial } })
    api.updateKafudaConfig.mockResolvedValue({
      data: {
        data: {
          ...initial,
          product_code: 'XL537872',
          version: 3,
        },
      },
    })

    await mountPage()

    expect(api.getKafudaConfig).toHaveBeenCalledTimes(1)
    expect(host.textContent).toContain('kafuda.salesBlockReasons.sales_disabled')
    expect(host.textContent).toContain('XL537872')

    const useButtons = [...host.querySelectorAll('button')].filter(
      (btn) => btn.textContent?.trim() === 'kafuda.config.actions.useCard'
    )
    expect(useButtons).toHaveLength(2)
    expect(useButtons[0]?.disabled).toBe(false)
    expect(useButtons[1]?.disabled).toBe(true)

    useButtons[0]!.click()
    await flush()

    const saveBtn = findButton('kafuda.config.actions.save')
    expect(saveBtn).toBeTruthy()
    saveBtn!.click()
    await flush()

    expect(api.updateKafudaConfig).toHaveBeenCalledTimes(1)
    const payload = api.updateKafudaConfig.mock.calls[0]![0]
    expect(payload.product_code).toBe('XL537872')
    expect(payload.api_key).toBeUndefined()
    expect(payload.clear_api_key).toBeUndefined()
    expect(notify.notifySuccess).toHaveBeenCalledWith('kafuda.config.alerts.saveSuccess')
  })

  it('tests connection with unsaved credentials and populates returned cards', async () => {
    api.getKafudaConfig.mockResolvedValue({
      data: {
        data: {
          api_base_url: 'https://kafuda.com',
          has_api_key: false,
          product_code: '',
          max_cost_cents: 0,
          allowed_kinds: ['new'],
          sales_enabled: false,
          reference_lookup_verified: false,
          catalog_ttl_seconds: 3600,
          catalog_refresh_interval_seconds: 0,
          poll_review_after_seconds: 86400,
          poll_interval_seconds: 10,
          config_source: 'config_file',
          version: 0,
          configured: false,
          gateway_config_invalid: false,
          worker_ready: true,
          sales_block_reason: 'gateway_not_configured',
          catalog_enabled: false,
          catalog_stale: true,
          catalog_options_count: 0,
          cards: [],
        },
      },
    })
    api.testKafudaConfig.mockResolvedValue({
      data: {
        data: {
          ok: true,
          enabled: true,
          options_count: 8,
          cards: [
            {
              product_code: 'NEWCARD',
              name: 'New Card',
              restricted: false,
              gpt_recommended: true,
              open_fee_cents: 50,
              min_amount_cents: 200,
            },
          ],
        },
      },
    })

    await mountPage()

    const keyInput = host.querySelector<HTMLInputElement>('#kafuda-api-key')
    expect(keyInput).toBeTruthy()
    keyInput!.value = 'kf_test_ephemeral_key'
    keyInput!.dispatchEvent(new Event('input'))
    await flush()

    const testBtn = findButton('kafuda.config.actions.test')
    testBtn!.click()
    await flush()

    expect(api.testKafudaConfig).toHaveBeenCalledWith({
      api_base_url: 'https://kafuda.com',
      api_key: 'kf_test_ephemeral_key',
    })
    expect(host.textContent).toContain('NEWCARD')
  })
})
