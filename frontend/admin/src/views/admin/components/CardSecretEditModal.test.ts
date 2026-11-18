import { createApp, h, nextTick, reactive } from 'vue'
import { expect, it, vi } from 'vitest'
import CardSecretEditModal from './CardSecretEditModal.vue'

const api = vi.hoisted(() => ({ updateCardSecret: vi.fn() }))

vi.mock('@/api/admin', () => ({ adminAPI: api }))
vi.mock('vue-i18n', async (importOriginal) => ({
  ...await importOriginal<typeof import('vue-i18n')>(),
  useI18n: () => ({ t: (key: string) => key }),
}))
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

it('sends a text edit without restoring the status seen when the card was opened', async () => {
  api.updateCardSecret.mockResolvedValue({})
  const card = reactive({
    id: 42, product_id: 1, sku_id: 2, secret: 'original', status: 'available', created_at: '', updated_at: '',
  })
  const host = document.createElement('div')
  document.body.append(host)
  const app = createApp({ render: () => h(CardSecretEditModal, { modelValue: true, cardSecret: card }) })
  app.mount(host)

  try {
    const submit = host.querySelector<HTMLButtonElement>('button[type="submit"]')!
    expect(submit.disabled).toBe(true)

    card.status = 'used'
    const textarea = host.querySelector('textarea')!
    textarea.value = 'replacement'
    textarea.dispatchEvent(new Event('input', { bubbles: true }))
    await nextTick()

    expect(submit.disabled).toBe(false)
    host.querySelector('form')!.dispatchEvent(new Event('submit', { cancelable: true }))
    expect(api.updateCardSecret.mock.calls).toEqual([[42, { secret: 'replacement' }]])
    await nextTick()
  } finally {
    app.unmount()
    host.remove()
  }
})
