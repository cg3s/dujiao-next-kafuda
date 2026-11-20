<template>
  <section v-if="available" class="rounded-xl border bg-card p-5">
    <h2 class="text-lg font-semibold">{{ t('directPurchase.authorization.title') }}</h2>
    <p class="mt-2 text-sm text-muted-foreground">{{ t('directPurchase.authorization.description') }}</p>
    <form class="mt-4 space-y-3" @submit.prevent="submit">
      <div class="flex items-center justify-between gap-2">
        <label for="direct-order-session" class="text-sm font-medium">
          {{ t('directPurchase.authorization.sessionLabel') }}
        </label>
        <Button type="button" variant="outline" size="sm" aria-controls="direct-order-session"
          :disabled="submitting" @click="showSession = !showSession">
          {{ t(showSession ? 'directPurchase.authorization.hide' : 'directPurchase.authorization.show') }}
        </Button>
      </div>
      <Textarea
        id="direct-order-session"
        v-model="session"
        rows="4"
        autocomplete="off"
        autocorrect="off"
        autocapitalize="off"
        spellcheck="false"
        :maxlength="16000"
        required
        :disabled="submitting"
        :placeholder="t('directPurchase.authorization.sessionPlaceholder')"
        class="resize-y"
        :class="{ 'session-masked': !showSession }"
      />
      <p class="text-xs text-muted-foreground">{{ t('directPurchase.authorization.privacy') }}</p>
      <Button type="submit" :disabled="submitting || !session.trim()">
        {{ t(submitting ? 'directPurchase.authorization.submitting' : 'directPurchase.authorization.submit') }}
      </Button>
    </form>
  </section>
</template>

<script setup lang="ts">
import { onScopeDispose, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { userOrderAPI } from '../api'
import { ApiRequestError } from '../api/client'
import { Button } from '@/components/ui/button'
import { Textarea } from '@/components/ui/textarea'
import { toast } from '../composables/useToast'

const props = defineProps<{ orderNo: string; available: boolean }>()
const emit = defineEmits<{ submitted: [] }>()
const { t } = useI18n()
const session = ref('')
const submitting = ref(false)
const showSession = ref(false)
let disposed = false
watch(() => props.available, (available) => {
  if (!available) {
    session.value = ''
    showSession.value = false
  }
})
onScopeDispose(() => {
  disposed = true
  session.value = ''
})

const submit = async () => {
  const value = session.value.trim()
  if (!props.available || !value || submitting.value) return
  session.value = ''
  showSession.value = false
  submitting.value = true
  try {
    await userOrderAPI.authorizeDirect(props.orderNo, value)
    if (disposed) return
    emit('submitted')
    toast.success(t('directPurchase.authorization.accepted'))
  } catch (error) {
    if (disposed) return
    emit('submitted')
    toast.error(error instanceof ApiRequestError ? error.message : t('directPurchase.authorization.failed'))
  } finally {
    submitting.value = false
  }
}
</script>

<style scoped>
.session-masked:not(:placeholder-shown) {
  -webkit-text-security: disc;
}

@supports not (-webkit-text-security: disc) {
  .session-masked:not(:placeholder-shown),
  .session-masked::selection {
    color: transparent;
  }

  .session-masked {
    caret-color: var(--ui-text-primary);
  }
}
</style>
