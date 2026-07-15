import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import AffiliateView from '../AffiliateView.vue'

const { copyToClipboard, getAffiliateDetail, publicSettings, showError } = vi.hoisted(() => ({
  copyToClipboard: vi.fn(),
  getAffiliateDetail: vi.fn(),
  publicSettings: { affiliate_enabled: false },
  showError: vi.fn(),
}))

vi.mock('@/api/user', () => ({
  default: {
    getAffiliateDetail,
    transferAffiliateQuota: vi.fn(),
  },
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    cachedPublicSettings: publicSettings,
    showError,
    showSuccess: vi.fn(),
  }),
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({
    refreshUser: vi.fn(),
  }),
}))

vi.mock('@/composables/useClipboard', () => ({
  useClipboard: () => ({ copyToClipboard }),
}))

vi.mock('vue-i18n', async (importOriginal) => {
  const actual = await importOriginal<typeof import('vue-i18n')>()
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key,
    }),
  }
})

function affiliateFixture(affiliateCode = 'FRIEND123') {
  return {
    user_id: 1,
    aff_code: affiliateCode,
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
        total_rebate: 3,
      },
    ],
  }
}

async function mountView() {
  const wrapper = mount(AffiliateView, {
    global: {
      stubs: {
        AppLayout: { template: '<main><slot /></main>' },
        Icon: true,
      },
    },
  })
  await flushPromises()
  return wrapper
}

describe('AffiliateView', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    publicSettings.affiliate_enabled = false
    copyToClipboard.mockResolvedValue(true)
    getAffiliateDetail.mockResolvedValue(affiliateFixture())
  })

  it('stacks long values and copy controls on mobile while retaining desktop rows', async () => {
    const affiliateCode = 'affiliate-code-that-is-long-enough-to-overflow-a-mobile-viewport'
    getAffiliateDetail.mockResolvedValue(affiliateFixture(affiliateCode))
    const wrapper = await mountView()

    const values = wrapper.findAll('code')
    expect(values).toHaveLength(2)
    for (const value of values) {
      expect(value.classes()).toEqual(expect.arrayContaining([
        'min-w-0',
        'break-all',
        'sm:flex-1',
        'sm:truncate',
      ]))
      expect(Array.from(value.element.parentElement?.classList ?? [])).toEqual(expect.arrayContaining([
        'flex-col',
        'items-stretch',
        'sm:flex-row',
        'sm:items-center',
      ]))
    }

    const copyButtons = wrapper.findAll('button').filter((button) =>
      ['affiliate.copyCode', 'affiliate.copyLink'].includes(button.text()),
    )
    expect(copyButtons).toHaveLength(2)
    for (const button of copyButtons) {
      expect(button.classes()).toEqual(expect.arrayContaining([
        'w-full',
        'sm:w-auto',
        'sm:shrink-0',
      ]))
    }

    await copyButtons[0].trigger('click')
    await copyButtons[1].trigger('click')
    await flushPromises()

    expect(copyToClipboard).toHaveBeenNthCalledWith(1, affiliateCode, 'affiliate.codeCopied')
    expect(copyToClipboard).toHaveBeenNthCalledWith(
      2,
      `${window.location.origin}/register?aff=${encodeURIComponent(affiliateCode)}`,
      'affiliate.linkCopied',
    )
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
