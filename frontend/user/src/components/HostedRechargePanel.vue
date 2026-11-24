<template>
  <section class="rounded-2xl border bg-card p-5 md:p-6 space-y-5" aria-live="polite">
    <!-- Header & Refresh -->
    <div class="flex flex-wrap items-center justify-between gap-3">
      <div class="flex flex-wrap items-center gap-2.5">
        <h2 class="text-lg font-bold tracking-tight">{{ t('hostedRecharge.progress.title') }}</h2>
        <Badge v-if="status" :variant="stageBadgeVariant" size="sm" class="rounded-full">
          {{ t(`hostedRecharge.progress.${status.stage}`) }}
        </Badge>
        <Badge v-if="status?.attempt_no && status.attempt_no > 0" variant="neutral" size="sm" class="rounded-full">
          {{ t('hostedRecharge.summary.attempt', { count: status.attempt_no }) }}
        </Badge>
      </div>
      <Button
        type="button"
        variant="outline"
        size="sm"
        class="rounded-full"
        :disabled="opening || submitting"
        @click="emit('refresh')"
      >
        <RefreshCw class="h-3.5 w-3.5" />
        {{ t('hostedRecharge.progress.refresh') }}
      </Button>
    </div>

    <!-- 3-Step Progress Stepper (aligned with official Quefa redeem flow) -->
    <ol v-if="status" class="grid grid-cols-1 gap-2 sm:grid-cols-3" aria-label="progress">
      <li
        v-for="step in stepperItems"
        :key="step.key"
        class="flex items-center gap-2.5 rounded-xl border px-3.5 py-2.5 text-xs font-medium transition-colors"
        :class="step.state === 'done'
          ? 'border-emerald-200 bg-emerald-50/70 text-emerald-800 dark:border-emerald-800/60 dark:bg-emerald-950/30 dark:text-emerald-200'
          : step.state === 'active'
            ? 'border-primary/60 bg-primary/10 text-primary'
            : 'border-border bg-secondary/40 text-muted-foreground'"
      >
        <span
          class="flex h-5 w-5 shrink-0 items-center justify-center rounded-full text-[11px] font-bold"
          :class="step.state === 'done'
            ? 'bg-emerald-600 text-white'
            : step.state === 'active'
              ? 'bg-primary text-primary-foreground'
              : 'bg-muted text-muted-foreground'"
        >
          <Check v-if="step.state === 'done'" class="h-3 w-3" />
          <span v-else>{{ step.index }}</span>
        </span>
        <span class="truncate">{{ step.label }}</span>
      </li>
    </ol>

    <!-- Status Hint, Granular Progress Stage & Paused Notice -->
    <div v-if="status" class="space-y-2.5">
      <p class="text-sm text-foreground leading-relaxed">{{ t(`hostedRecharge.stageHints.${status.stage}`) }}</p>

      <div
        v-if="status.paused"
        class="flex items-start gap-2 rounded-xl border border-amber-200 bg-amber-50/80 px-3.5 py-2.5 text-xs text-amber-800 dark:border-amber-800/60 dark:bg-amber-950/30 dark:text-amber-300"
        role="status"
      >
        <AlertTriangle class="h-4 w-4 shrink-0 mt-0.5" />
        <span>{{ t('hostedRecharge.summary.pausedNotice') }}</span>
      </div>

      <div
        v-if="progressStageLabel && status.stage === 'recharging'"
        class="inline-flex items-center gap-2 rounded-lg bg-secondary/80 px-3 py-1.5 text-xs font-medium text-foreground"
      >
        <Loader2 class="h-3.5 w-3.5 animate-spin text-primary" />
        <span>{{ progressStageLabel }}</span>
      </div>

      <p
        v-if="status.progress_message"
        class="rounded-xl border px-3.5 py-2.5 text-xs leading-relaxed"
        :class="status.stage === 'resubmit_required' || status.stage === 'manual_review'
          ? 'border-amber-200 bg-amber-50/70 text-amber-900 dark:border-amber-800/60 dark:bg-amber-950/30 dark:text-amber-200'
          : 'border-border bg-secondary/60 text-muted-foreground'"
      >
        {{ status.progress_message }}
      </p>

      <p class="text-xs text-muted-foreground">
        {{ t('hostedRecharge.progress.updated', { time: formatDate(status.progress_updated_at || status.updated_at) }) }}
      </p>
    </div>

    <p v-if="unavailable" class="rounded-xl bg-secondary/60 px-3.5 py-2.5 text-sm text-muted-foreground">
      {{ t('hostedRecharge.progress.retrying') }}
    </p>
    <p v-else-if="!status" class="text-sm text-muted-foreground">
      {{ t('common.loading') }}
    </p>

    <!-- Success Receipt Card -->
    <div
      v-if="status?.stage === 'succeeded'"
      class="rounded-xl border border-emerald-200 bg-emerald-50/60 p-4 dark:border-emerald-800/60 dark:bg-emerald-950/20 space-y-2.5"
    >
      <div class="grid grid-cols-1 gap-2 sm:grid-cols-3 text-xs">
        <div v-if="status.account_email_masked">
          <div class="text-muted-foreground">{{ t('hostedRecharge.summary.targetAccount') }}</div>
          <div class="mt-0.5 font-semibold text-foreground break-all">{{ status.account_email_masked }}</div>
        </div>
        <div v-if="status.completed_at">
          <div class="text-muted-foreground">{{ t('hostedRecharge.summary.completedAt') }}</div>
          <div class="mt-0.5 font-semibold text-foreground">{{ formatDate(status.completed_at) }}</div>
        </div>
        <div>
          <div class="text-muted-foreground">{{ t('hostedRecharge.summary.orderNo') }}</div>
          <div class="mt-0.5 flex items-center gap-1.5">
            <span class="font-mono font-semibold text-foreground">{{ orderNo }}</span>
            <button
              type="button"
              class="inline-flex items-center gap-1 rounded-md border border-border bg-background px-1.5 py-0.5 text-[11px] font-medium text-foreground hover:bg-secondary"
              @click="copyOrderNo"
            >
              <Check v-if="orderNoCopied" class="h-3 w-3 text-emerald-600" />
              <Copy v-else class="h-3 w-3" />
              {{ orderNoCopied ? t('hostedRecharge.summary.orderNoCopied') : t('hostedRecharge.summary.copyOrderNo') }}
            </button>
          </div>
        </div>
      </div>
    </div>

    <!-- White-Label In-Page Credential Submission Form -->
    <div
      v-if="status?.can_submit_redemption && !status.paused && !unavailable"
      class="rounded-xl border border-border bg-secondary/30 p-4 md:p-5 space-y-4"
    >
      <div class="space-y-1">
        <h3 class="text-sm font-semibold text-foreground">
          {{ t(status.stage === 'resubmit_required' ? 'hostedRecharge.submit.resubmitTitle' : 'hostedRecharge.submit.title') }}
        </h3>
        <p
          v-if="status.stage === 'resubmit_required'"
          class="text-xs text-amber-700 dark:text-amber-300"
        >
          {{ t('hostedRecharge.submit.resubmitBanner') }}
        </p>
      </div>

      <!-- How-to box for ChatGPT Session JSON -->
      <div class="rounded-lg border border-border/80 bg-background/80 p-3 text-xs text-muted-foreground space-y-1.5">
        <div class="font-semibold text-foreground">{{ t('hostedRecharge.submit.howToTitle') }}</div>
        <p>
          {{ t('hostedRecharge.submit.howToStep1') }}
          <a
            href="https://chatgpt.com/api/auth/session"
            target="_blank"
            rel="noopener noreferrer"
            class="inline-flex items-center gap-1 font-mono font-semibold text-primary underline underline-offset-2"
          >
            https://chatgpt.com/api/auth/session
            <ExternalLink class="h-3 w-3" />
          </a>
        </p>
        <p>{{ t('hostedRecharge.submit.howToStep2') }}</p>
      </div>

      <form class="space-y-3" @submit.prevent="handleSubmitCredentials">
        <div class="flex items-center justify-between gap-2">
          <label for="hosted-recharge-session" class="text-xs font-semibold text-foreground">
            {{ t('hostedRecharge.submit.sessionLabel') }}
          </label>
          <Button
            type="button"
            variant="outline"
            size="sm"
            aria-controls="hosted-recharge-session"
            :disabled="submitting"
            @click="showSession = !showSession"
          >
            {{ t(showSession ? 'hostedRecharge.submit.hide' : 'hostedRecharge.submit.show') }}
          </Button>
        </div>

        <Textarea
          id="hosted-recharge-session"
          v-model="sessionInput"
          rows="4"
          autocomplete="off"
          autocorrect="off"
          autocapitalize="off"
          spellcheck="false"
          data-sentry-mask="true"
          data-private="true"
          required
          :disabled="submitting"
          :placeholder="t('hostedRecharge.submit.sessionPlaceholder')"
          class="resize-y font-mono text-xs"
          :class="{ 'session-masked': !showSession }"
        />

        <div
          v-if="detectedEmail"
          class="rounded-lg border border-emerald-200 bg-emerald-50/70 px-3 py-2 text-xs dark:border-emerald-800/60 dark:bg-emerald-950/30 space-y-0.5"
        >
          <div class="font-semibold text-emerald-800 dark:text-emerald-200">
            {{ t('hostedRecharge.submit.detectedEmail', { email: detectedEmail }) }}
          </div>
          <div class="text-emerald-700/90 dark:text-emerald-300/80">
            {{ t('hostedRecharge.submit.detectedEmailHint') }}
          </div>
        </div>

        <label class="flex items-start gap-2.5 cursor-pointer select-none">
          <input
            v-model="confirmedEmail"
            type="checkbox"
            class="mt-0.5 h-4 w-4 rounded border-border accent-primary"
            :disabled="submitting"
          />
          <span class="text-xs text-foreground leading-relaxed">
            {{ t('hostedRecharge.submit.confirmCheckbox') }}
          </span>
        </label>

        <p class="text-xs text-muted-foreground leading-relaxed">
          {{ t('hostedRecharge.submit.privacy') }}
        </p>

        <div class="flex flex-wrap items-center gap-3 pt-1">
          <Button
            type="submit"
            class="rounded-full font-semibold"
            :disabled="submitting || !sessionInput.trim() || !confirmedEmail"
          >
            <Loader2 v-if="submitting" class="h-4 w-4 animate-spin" />
            {{
              submitting
                ? t('hostedRecharge.submit.submitting')
                : t(status.stage === 'resubmit_required' ? 'hostedRecharge.submit.resubmitButton' : 'hostedRecharge.submit.submitButton')
            }}
          </Button>
        </div>
      </form>
    </div>

    <!-- Optional Hosted Portal Fallback (when upstream returned a hosted fulfillment_url) -->
    <div v-if="status?.can_open_fulfillment && status.has_hosted_entry && !unavailable" class="flex flex-wrap items-center gap-2.5 pt-1 text-xs text-muted-foreground">
      <span v-if="status.can_submit_redemption">{{ t('hostedRecharge.entry.fallbackHint') }}</span>
      <Button
        type="button"
        :variant="status.can_submit_redemption ? 'outline' : 'default'"
        size="sm"
        class="rounded-full font-semibold"
        :disabled="opening || submitting"
        @click="handleOpenEntry"
      >
        <ExternalLink class="h-3.5 w-3.5" />
        {{
          opening
            ? t('hostedRecharge.entry.opening')
            : t(status.stage === 'resubmit_required' ? 'hostedRecharge.entry.resubmitButton' : 'hostedRecharge.entry.openButton')
        }}
      </Button>
      <span v-if="!status.can_submit_redemption">{{ t('hostedRecharge.entry.hint') }}</span>
    </div>

    <div
      v-if="fallbackUrl"
      class="flex flex-wrap items-center gap-2 rounded-lg border bg-secondary/50 px-3 py-2 text-xs text-muted-foreground"
    >
      <span>{{ t('hostedRecharge.entry.popupBlocked') }}</span>
      <a
        :href="fallbackUrl"
        target="_blank"
        rel="noopener noreferrer"
        class="font-semibold text-primary underline underline-offset-2"
      >
        {{ t('hostedRecharge.entry.openManually') }}
      </a>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, onScopeDispose, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { AlertTriangle, Check, Copy, ExternalLink, Loader2, RefreshCw } from 'lucide-vue-next'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Textarea } from '@/components/ui/textarea'
import { guestOrderAPI, userOrderAPI } from '../api'
import { ApiRequestError } from '../api/client'
import { toast } from '../composables/useToast'
import type { HostedRechargeStatus } from '../utils/hostedRechargeStatus'

const MAX_CREDENTIAL_BYTES = 256 * 1024

const props = defineProps<{
  orderNo: string
  status: HostedRechargeStatus | null
  unavailable: boolean
  formatDate: (value: string) => string
  guestAuth?: { email: string; order_password: string }
}>()

const emit = defineEmits<{
  (e: 'refresh'): void
}>()

const { t, te } = useI18n()
const opening = ref(false)
const submitting = ref(false)
const fallbackUrl = ref('')
const sessionInput = ref('')
const showSession = ref(false)
const confirmedEmail = ref(false)
const orderNoCopied = ref(false)
let disposed = false

watch(() => props.orderNo, () => {
  fallbackUrl.value = ''
  sessionInput.value = ''
  showSession.value = false
  confirmedEmail.value = false
})

watch(() => props.status?.can_open_fulfillment, (canOpen) => {
  if (!canOpen) fallbackUrl.value = ''
})

watch(() => props.status?.can_submit_redemption, (canSubmit) => {
  if (!canSubmit) {
    sessionInput.value = ''
    showSession.value = false
    confirmedEmail.value = false
  }
})

onScopeDispose(() => {
  disposed = true
  sessionInput.value = ''
  confirmedEmail.value = false
})

// Local-only, in-memory email preview from ChatGPT Session JSON for buyer visual confirmation.
const detectedEmail = computed(() => {
  const raw = sessionInput.value.trim()
  if (!raw || !raw.startsWith('{')) return ''
  try {
    const parsed = JSON.parse(raw) as Record<string, any>
    const email = parsed?.user?.email || parsed?.email || ''
    return typeof email === 'string' && email.includes('@') ? email.trim() : ''
  } catch {
    return ''
  }
})

const progressStageLabel = computed(() => {
  const stage = props.status?.progress_stage?.trim()
  if (!stage) return ''
  const key = `hostedRecharge.progressStages.${stage}`
  return te(key) ? t(key) : ''
})

const stepperItems = computed(() => {
  const stage = props.status?.stage
  const paidDone = stage !== undefined && stage !== 'waiting_payment' && stage !== 'closed'
  const accountDone = stage === 'recharging' || stage === 'succeeded' || stage === 'manual_review' || stage === 'refunding' || stage === 'refunded'
  const accountActive = stage === 'waiting_submit' || stage === 'resubmit_required'
  const fulfillmentDone = stage === 'succeeded'
  const fulfillmentActive = stage === 'recharging' || stage === 'manual_review' || stage === 'refunding'

  return [
    {
      key: 'payment',
      index: 1,
      label: t('hostedRecharge.steps.payment'),
      state: paidDone ? 'done' : 'active',
    },
    {
      key: 'account',
      index: 2,
      label: t('hostedRecharge.steps.account'),
      state: accountDone ? 'done' : accountActive ? 'active' : 'pending',
    },
    {
      key: 'fulfillment',
      index: 3,
      label: t('hostedRecharge.steps.fulfillment'),
      state: fulfillmentDone ? 'done' : fulfillmentActive ? 'active' : 'pending',
    },
  ] as const
})

const stageBadgeVariant = computed<'success' | 'warning' | 'destructive' | 'info' | 'neutral'>(() => {
  switch (props.status?.stage) {
    case 'succeeded':
      return 'success'
    case 'resubmit_required':
    case 'procurement_blocked':
    case 'manual_review':
      return 'warning'
    case 'closed':
    case 'refunded':
      return 'neutral'
    case 'refunding':
      return 'destructive'
    default:
      return 'info'
  }
})

const handleSubmitCredentials = async () => {
  const raw = sessionInput.value.trim()
  if (!props.status?.can_submit_redemption || !raw || !confirmedEmail.value || submitting.value) return
  if (new TextEncoder().encode(raw).length > MAX_CREDENTIAL_BYTES) {
    toast.error(t('hostedRecharge.submit.tooLarge'))
    return
  }

  // Clear raw credentials from memory immediately before dispatching network request.
  sessionInput.value = ''
  showSession.value = false
  submitting.value = true
  try {
    const payload = {
      credential_mode: 'session',
      session: raw,
      customer_confirmed_email: true,
    }
    if (props.guestAuth) {
      await guestOrderAPI.submitHostedRecharge(props.orderNo, payload, props.guestAuth)
    } else {
      await userOrderAPI.submitHostedRecharge(props.orderNo, payload)
    }
    if (disposed) return
    confirmedEmail.value = false
    toast.success(t('hostedRecharge.submit.accepted'))
    emit('refresh')
  } catch (error) {
    if (disposed) return
    toast.error(error instanceof ApiRequestError ? error.message : t('hostedRecharge.submit.failed'))
    emit('refresh')
  } finally {
    submitting.value = false
  }
}

const handleOpenEntry = async () => {
  if (opening.value || !props.orderNo) return
  opening.value = true
  fallbackUrl.value = ''
  try {
    const response = props.guestAuth
      ? await guestOrderAPI.openHostedRechargeEntry(props.orderNo, props.guestAuth)
      : await userOrderAPI.openHostedRechargeEntry(props.orderNo)
    const url = String(response.data?.data?.fulfillment_url || '').trim()
    if (!url || !/^https?:\/\//i.test(url)) {
      toast.error(t('hostedRecharge.entry.failed'))
      emit('refresh')
      return
    }
    const popup = window.open(url, '_blank', 'noopener,noreferrer')
    if (!popup) {
      fallbackUrl.value = url
    }
    emit('refresh')
  } catch (error) {
    toast.error(error instanceof ApiRequestError ? error.message : t('hostedRecharge.entry.failed'))
    emit('refresh')
  } finally {
    opening.value = false
  }
}

const copyOrderNo = async () => {
  if (!props.orderNo) return
  try {
    await navigator.clipboard.writeText(props.orderNo)
    orderNoCopied.value = true
    setTimeout(() => {
      orderNoCopied.value = false
    }, 2000)
  } catch {
    // Ignore clipboard rejection in restricted contexts.
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
