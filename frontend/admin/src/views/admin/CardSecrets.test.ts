import { createApp, h, nextTick } from 'vue'
import { createMemoryHistory, createRouter } from 'vue-router'
import { expect, it, vi } from 'vitest'
import CardSecrets from './CardSecrets.vue'

const api = vi.hoisted(() => ({
  getProducts: vi.fn(),
  getCardSecrets: vi.fn(),
  batchUpdateCardSecretStatus: vi.fn(),
  batchDeleteCardSecrets: vi.fn(),
}))

vi.mock('@/api/admin', () => ({ adminAPI: api }))
vi.mock('@/utils/confirm', () => ({ confirmAction: async () => true }))
vi.mock('vue-i18n', async (importOriginal) => ({
  ...await importOriginal<typeof import('vue-i18n')>(),
  useI18n: () => ({ t: (key: string, params?: { count?: number }) => (
    params?.count === undefined ? key : `${key} ${params.count}`
  ) }),
}))

const actions = [
  { trigger: 'batch.applyStatus', success: 'success.batchStatusUpdated', request: api.batchUpdateCardSecretStatus },
  { trigger: 'batch.deleteSelected', success: 'success.batchDeleted', request: api.batchDeleteCardSecrets },
]

it.each(actions)('reports the actual count for $trigger and falls back when omitted', async ({ trigger, success, request }) => {
  vi.resetAllMocks()
  api.getProducts.mockResolvedValue({ data: { data: [] } })
  api.getCardSecrets.mockResolvedValue({ data: { data: [{
    id: 42, product_id: 1, sku_id: 2, secret: 'fixture', status: 'available', created_at: '', updated_at: '',
  }] } })
  request.mockResolvedValueOnce({ data: { data: { affected: 0 } } })
    .mockResolvedValueOnce({ data: { data: {} } })

  const router = createRouter({
    history: createMemoryHistory(),
    routes: [{ path: '/:pathMatch(.*)*', component: { render: () => null } }],
  })
  await router.push('/card-secrets')
  const host = document.createElement('div')
  document.body.append(host)
  const app = createApp({ render: () => h(CardSecrets) }).use(router)
  app.mount(host)

  try {
    const checkbox = () => host.querySelector<HTMLButtonElement>('tbody button[role="checkbox"]')
    const button = () => [...host.querySelectorAll('button')].find(
      (item) => item.textContent?.trim() === `admin.cardSecrets.${trigger}`,
    )
    await vi.waitFor(() => expect(checkbox()).not.toBeNull())
    expect(button()?.disabled).toBe(true)
    expect(request).not.toHaveBeenCalled()

    for (const count of [0, 1]) {
      checkbox()!.click()
      await nextTick()
      expect(button()?.disabled).toBe(false)
      button()!.click()
      await vi.waitFor(() => expect(host.textContent).toContain(`admin.cardSecrets.${success} ${count}`))
      await vi.waitFor(() => expect(button()?.disabled).toBe(true))
    }
    expect(request).toHaveBeenCalledTimes(2)
    for (const [payload] of request.mock.calls) expect(payload.ids).toEqual([42])
  } finally {
    app.unmount()
    host.remove()
  }
})
