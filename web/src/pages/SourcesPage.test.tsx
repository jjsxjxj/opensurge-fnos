// @vitest-environment jsdom
import { cleanup, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { Overview, Source } from '../types'

vi.mock('../api', () => ({
  api: {
    sources: vi.fn(),
    importURL: vi.fn(),
    importFile: vi.fn(),
    refreshSource: vi.fn(),
    applySource: vi.fn(),
    sourceEdit: vi.fn(),
    updateSource: vi.fn(),
    deleteSource: vi.fn(),
  },
}))

import { api } from '../api'
import { SourcesPage } from './SourcesPage'

const document_ = 'rules:\n  - MATCH,DIRECT\n'

function source(overrides: Partial<Source> = {}): Source {
  return {
    id: 'abc12345',
    name: '一元机场',
    kind: 'mihomo_profile',
    origin: 'https://example.com/profile',
    digest: 'd'.repeat(64),
    size: 128,
    valid: true,
    validation: 'structural validation passed; apply runs mihomo engine validation',
    desired: false,
    applied: false,
    versions: [],
    imported_at: '2026-08-01T00:00:00Z',
    inventory: { proxies: [], proxy_providers: [], proxy_groups: [], rule_providers: [], rule_count: 514, terminal_match: true, warnings: [] },
    diff: { proxies_added: [], proxies_removed: [], groups_added: [], groups_removed: [], proxy_providers_added: [], proxy_providers_removed: [], rule_providers_added: [], rule_providers_removed: [], rule_count_delta: 0 },
    ...overrides,
  }
}

const overview = { status: { gateway: 'running', mihomo: 'running' } } as unknown as Overview

describe('SourcesPage', () => {
  beforeEach(() => {
    vi.mocked(api.sources).mockResolvedValue({ revision: 'rev-1', sources: [source()] })
    vi.mocked(api.sourceEdit).mockResolvedValue({ schema_version: 1, id: 'abc12345', name: '一元机场', kind: 'mihomo_profile', origin: 'https://example.com/profile', digest: 'd'.repeat(64), url: 'https://example.com/profile?token=secret', document: document_ })
    vi.mocked(api.updateSource).mockResolvedValue(source({ name: '新名字' }))
    vi.mocked(api.deleteSource).mockResolvedValue({ revision: 'rev-2', sources: [] })
    vi.mocked(api.refreshSource).mockResolvedValue(source())
    vi.mocked(api.applySource).mockResolvedValue(source())
  })

  afterEach(() => { cleanup(); vi.clearAllMocks() })

  it('edits the source name and keeps the untouched fields out of the request', async () => {
    render(<SourcesPage overview={overview} onChanged={vi.fn(async () => {})} />)
    await screen.findByRole('heading', { name: '一元机场' })

    await userEvent.click(screen.getByRole('button', { name: '编辑' }))
    expect(api.sourceEdit).toHaveBeenCalledWith('abc12345')
    const dialog = within(await screen.findByRole('dialog'))
    const nameInput = dialog.getByLabelText('来源名称')
    expect((dialog.getByLabelText('订阅地址') as HTMLInputElement).value).toBe('https://example.com/profile?token=secret')
    expect((dialog.getByLabelText('YAML 内容') as HTMLTextAreaElement).value).toBe(document_)

    await userEvent.clear(nameInput)
    await userEvent.type(nameInput, '新名字')
    await userEvent.click(dialog.getByRole('button', { name: '保存并重新校验' }))

    await waitFor(() => expect(api.updateSource).toHaveBeenCalledTimes(1))
    const [, payload] = vi.mocked(api.updateSource).mock.calls[0]
    expect(payload).toEqual({ name: '新名字' })
  })

  it('sends only the edited YAML document', async () => {
    render(<SourcesPage overview={overview} onChanged={vi.fn(async () => {})} />)
    await screen.findByRole('heading', { name: '一元机场' })

    await userEvent.click(screen.getByRole('button', { name: '编辑' }))
    const dialog = within(await screen.findByRole('dialog'))
    const editor = dialog.getByLabelText('YAML 内容')
    await userEvent.clear(editor)
    await userEvent.type(editor, 'rules:\n  - DOMAIN,example.com,REJECT\n')

    await userEvent.click(dialog.getByRole('button', { name: '保存并重新校验' }))

    await waitFor(() => expect(api.updateSource).toHaveBeenCalledTimes(1))
    const [id, payload] = vi.mocked(api.updateSource).mock.calls[0]
    expect(id).toBe('abc12345')
    expect(payload.name).toBe('一元机场')
    expect(payload.url).toBeUndefined()
    expect(payload.document).toContain('DOMAIN,example.com,REJECT')
  })

  it('does not call the API when the editor is saved without changes', async () => {
    render(<SourcesPage overview={overview} onChanged={vi.fn(async () => {})} />)
    await screen.findByRole('heading', { name: '一元机场' })

    await userEvent.click(screen.getByRole('button', { name: '编辑' }))
    const dialog = within(await screen.findByRole('dialog'))
    await userEvent.click(dialog.getByRole('button', { name: '保存并重新校验' }))

    expect(await screen.findByText('没有需要保存的修改。')).toBeTruthy()
    expect(api.updateSource).not.toHaveBeenCalled()
    expect(screen.queryByRole('dialog')).toBeNull()
  })

  it('deletes a desired source after an explicit confirmation', async () => {
    vi.mocked(api.sources).mockResolvedValue({ revision: 'rev-1', sources: [source({ desired: true })] })
    const onChanged = vi.fn(async () => {})
    render(<SourcesPage overview={overview} onChanged={onChanged} />)
    await screen.findByRole('heading', { name: '一元机场' })

    await userEvent.click(screen.getByRole('button', { name: '删除' }))
    const dialog = within(screen.getByRole('dialog'))
    expect(dialog.getByText(/下次启动版本/)).toBeTruthy()
    expect(api.deleteSource).not.toHaveBeenCalled()

    await userEvent.click(dialog.getByRole('button', { name: '确认删除' }))

    await waitFor(() => expect(api.deleteSource).toHaveBeenCalledWith('abc12345'))
    expect(await screen.findByText(/已删除，快照文件与保存的订阅链接已一并清理/)).toBeTruthy()
    await waitFor(() => expect(onChanged).toHaveBeenCalled())
  })

  it('refuses to delete the running version and points at the gateway stop', async () => {
    vi.mocked(api.sources).mockResolvedValue({ revision: 'rev-1', sources: [source({ applied: true })] })
    render(<SourcesPage overview={overview} onChanged={vi.fn(async () => {})} />)
    await screen.findByRole('heading', { name: '一元机场' })

    await userEvent.click(screen.getByRole('button', { name: '删除' }))
    const dialog = within(screen.getByRole('dialog'))
    expect(dialog.getByText(/请先停止网关/)).toBeTruthy()
    expect(dialog.queryByRole('button', { name: '确认删除' })).toBeNull()
    expect(dialog.getByRole('button', { name: '关闭' })).toBeTruthy()
    expect(api.deleteSource).not.toHaveBeenCalled()
  })

  it('keeps the library visible when a delete is refused by the server', async () => {
    vi.mocked(api.deleteSource).mockRejectedValue(new Error('the applied source cannot be deleted; stop the gateway first'))
    render(<SourcesPage overview={overview} onChanged={vi.fn(async () => {})} />)
    await screen.findByRole('heading', { name: '一元机场' })

    await userEvent.click(screen.getByRole('button', { name: '删除' }))
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: '确认删除' }))

    expect(await screen.findByText('the applied source cannot be deleted; stop the gateway first')).toBeTruthy()
    await waitFor(() => expect(api.sources).toHaveBeenCalledTimes(2))
    expect(screen.getByRole('heading', { name: '一元机场' })).toBeTruthy()
  })
})
