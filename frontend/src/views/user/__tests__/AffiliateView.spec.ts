import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, shallowMount } from '@vue/test-utils'

import AffiliateView from '../AffiliateView.vue'

const getAffiliateDetail = vi.hoisted(() => vi.fn())
const showError = vi.hoisted(() => vi.fn())
const publicSettings = vi.hoisted(() => ({ affiliate_enabled: false }))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key })
  }
})

vi.mock('@/api/user', () => ({
  default: {
    getAffiliateDetail,
    transferAffiliateQuota: vi.fn()
  }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    cachedPublicSettings: publicSettings,
    showError,
    showSuccess: vi.fn()
  })
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({ refreshUser: vi.fn() })
}))

function affiliateFixture() {
  return {
    user_id: 1,
    aff_code: 'FRIEND123',
    inviter_id: null,
    aff_count: 2,
    aff_quota: 10,
    aff_frozen_quota: 0,
    aff_history_quota: 20,
    effective_rebate_rate_percent: 15,
    invitees: [
      {
        user_id: 2,
        email: 'invitee@example.com',
        username: 'invitee',
        created_at: '2026-07-15T00:00:00Z',
        total_rebate: 3
      }
    ]
  }
}

async function mountView() {
  const wrapper = shallowMount(AffiliateView, {
    global: {
      stubs: {
        AppLayout: { template: '<div><slot /></div>' },
        Icon: true
      }
    }
  })
  await flushPromises()
  return wrapper
}

describe('AffiliateView invitation-only mode', () => {
  beforeEach(() => {
    publicSettings.affiliate_enabled = false
    getAffiliateDetail.mockReset().mockResolvedValue(affiliateFixture())
    showError.mockReset()
  })

  it('shows reusable invitation details without rebate claims when rebates are disabled', async () => {
    const wrapper = await mountView()

    expect(wrapper.text()).toContain('affiliate.invitationDescription')
    expect(wrapper.text()).toContain('affiliate.stats.invitedUsers')
    expect(wrapper.text()).not.toContain('affiliate.stats.rebateRate')
    expect(wrapper.text()).not.toContain('affiliate.transfer.title')
    expect(wrapper.text()).not.toContain('affiliate.invitees.columns.rebate')
  })

  it('keeps rebate details when the affiliate feature is enabled', async () => {
    publicSettings.affiliate_enabled = true
    const wrapper = await mountView()

    expect(wrapper.text()).toContain('affiliate.description')
    expect(wrapper.text()).toContain('affiliate.stats.rebateRate')
    expect(wrapper.text()).toContain('affiliate.transfer.title')
    expect(wrapper.text()).toContain('affiliate.invitees.columns.rebate')
  })
})
