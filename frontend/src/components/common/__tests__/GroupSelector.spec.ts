import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import GroupSelector from '../GroupSelector.vue'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: { count?: number }) =>
        key === 'common.selectedCount' ? `（已选 ${params?.count ?? 0} 个）` : key
    })
  }
})

describe('GroupSelector', () => {
  it('uses a custom label without repeating the default group title', () => {
    const wrapper = mount(GroupSelector, {
      props: {
        modelValue: [],
        groups: [],
        label: '适用分组'
      }
    })

    expect(wrapper.text()).toContain('适用分组 （已选 0 个）')
    expect(wrapper.text()).not.toContain('admin.users.groups')
  })
})
