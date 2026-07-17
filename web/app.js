(() => {
  'use strict'

  const elements = {
    accountBadge: document.getElementById('account-badge'),
    notice: document.getElementById('notice'),
    loading: document.getElementById('usage-loading'),
    content: document.getElementById('usage-content'),
    meta: document.getElementById('usage-meta'),
    windows: document.getElementById('usage-windows'),
    error: document.getElementById('error-panel'),
    queryButton: document.getElementById('query-button'),
    countButton: document.getElementById('count-button'),
    countLabel: document.getElementById('count-label'),
    resetButton: document.getElementById('reset-button'),
    creditInfo: document.getElementById('credit-info'),
    resetPolicy: document.getElementById('reset-policy')
  }

  const state = {
    basePath: window.location.pathname.replace(/\/+$/, ''),
    allowReset: false,
    quota: null,
    busy: new Set()
  }

  const windowDefinitions = [
    ['five_hour', '5h'],
    ['seven_day', '7d'],
    ['seven_day_sonnet', '7d S'],
    ['seven_day_fable', '7d F']
  ]

  async function request(action, options = {}) {
    const response = await fetch(`${state.basePath}/api/${action}`, {
      cache: 'no-store',
      credentials: 'same-origin',
      ...options
    })
    let payload
    try {
      payload = await response.json()
    } catch {
      throw new Error(`服务返回了无法解析的响应（HTTP ${response.status}）`)
    }
    if (!response.ok || (payload && typeof payload === 'object' && payload.code !== undefined && payload.code !== 0)) {
      throw new Error(payload?.message || payload?.reason || `请求失败（HTTP ${response.status}）`)
    }
    if (payload && typeof payload === 'object' && payload.code === 0 && 'data' in payload) {
      return payload.data
    }
    return payload
  }

  function setBusy(name, active) {
    if (active) state.busy.add(name)
    else state.busy.delete(name)
    const anyBusy = state.busy.size > 0
    elements.queryButton.disabled = anyBusy
    elements.countButton.disabled = anyBusy
    elements.queryButton.classList.toggle('is-loading', state.busy.has('usage'))
    elements.countButton.classList.toggle('is-loading', state.busy.has('quota'))
    elements.resetButton.classList.toggle('is-loading', state.busy.has('reset'))
    updateResetButton()
  }

  function showError(error) {
    elements.error.textContent = error instanceof Error ? error.message : String(error)
    elements.error.classList.remove('is-hidden')
  }

  function clearError() {
    elements.error.textContent = ''
    elements.error.classList.add('is-hidden')
  }

  function showNotice(message) {
    elements.notice.textContent = message
    elements.notice.classList.remove('is-hidden')
  }

  function clearNotice() {
    elements.notice.textContent = ''
    elements.notice.classList.add('is-hidden')
  }

  function formatPercent(value) {
    const number = Number(value)
    if (!Number.isFinite(number)) return '0%'
    return `${number >= 10 ? number.toFixed(0) : number.toFixed(1)}%`
  }

  function formatCompact(value) {
    const number = Number(value)
    if (!Number.isFinite(number)) return '0'
    return new Intl.NumberFormat(undefined, {
      notation: number >= 10000 ? 'compact' : 'standard',
      maximumFractionDigits: 1
    }).format(number)
  }

  function formatMoney(value) {
    const number = Number(value)
    if (!Number.isFinite(number)) return '0.00'
    return number.toFixed(number >= 100 ? 0 : number >= 1 ? 2 : 4)
  }

  function formatDate(value) {
    const date = new Date(value)
    if (Number.isNaN(date.getTime())) return String(value || '-')
    return new Intl.DateTimeFormat(undefined, {
      month: '2-digit',
      day: '2-digit',
      hour: '2-digit',
      minute: '2-digit'
    }).format(date)
  }

  function formatResetTime(value, utilization) {
    if (!value) return '-'
    const resetAt = new Date(value)
    if (Number.isNaN(resetAt.getTime())) return String(value)
    const remaining = resetAt.getTime() - Date.now()
    if (remaining <= 0) return Number(utilization) > 0 ? '等待刷新' : '现在'
    const minutes = Math.max(1, Math.ceil(remaining / 60000))
    const days = Math.floor(minutes / 1440)
    const hours = Math.floor((minutes % 1440) / 60)
    const restMinutes = minutes % 60
    if (days > 0) return `${days}天 ${hours}小时`
    if (hours > 0) return `${hours}小时 ${restMinutes}分钟`
    return `${restMinutes}分钟`
  }

  function makeChip(text) {
    const chip = document.createElement('span')
    chip.className = 'stat-chip'
    chip.textContent = text
    return chip
  }

  function renderWindow(label, usage) {
    const wrapper = document.createElement('section')
    wrapper.className = 'usage-window'

    const header = document.createElement('div')
    header.className = 'window-header'
    const labelNode = document.createElement('span')
    labelNode.className = 'window-label'
    labelNode.textContent = label
    const valueNode = document.createElement('span')
    valueNode.className = 'window-value'
    valueNode.textContent = formatPercent(usage.utilization)
    header.append(labelNode, valueNode)

    const track = document.createElement('div')
    track.className = 'progress-track'
    track.setAttribute('role', 'progressbar')
    track.setAttribute('aria-valuemin', '0')
    track.setAttribute('aria-valuemax', '100')
    track.setAttribute('aria-valuenow', String(Math.max(0, Math.min(100, Number(usage.utilization) || 0))))
    const bar = document.createElement('div')
    bar.className = 'progress-bar'
    bar.style.width = `${Math.max(0, Math.min(100, Number(usage.utilization) || 0))}%`
    track.append(bar)

    const footer = document.createElement('div')
    footer.className = 'window-footer'
    const stats = document.createElement('div')
    stats.className = 'window-stats'
    const windowStats = usage.window_stats
    if (windowStats) {
      stats.append(
        makeChip(`${formatCompact(windowStats.requests)} req`),
        makeChip(`${formatCompact(windowStats.tokens)} tok`),
        makeChip(`A $${formatMoney(windowStats.cost)}`)
      )
      if (windowStats.user_cost !== undefined && windowStats.user_cost !== null) {
        stats.append(makeChip(`U $${formatMoney(windowStats.user_cost)}`))
      }
    } else if (usage.limit_requests) {
      stats.append(makeChip(`${formatCompact(usage.used_requests)} / ${formatCompact(usage.limit_requests)} req`))
    }
    const reset = document.createElement('span')
    reset.className = 'reset-time'
    reset.title = usage.resets_at ? formatDate(usage.resets_at) : ''
    reset.textContent = `↻ ${formatResetTime(usage.resets_at, usage.utilization)}`
    footer.append(stats, reset)

    wrapper.append(header, track, footer)
    return wrapper
  }

  function renderUsage(data) {
    elements.loading.classList.add('is-hidden')
    elements.content.classList.remove('is-hidden')
    elements.meta.replaceChildren()
    elements.windows.replaceChildren()

    const source = document.createElement('span')
    source.className = 'meta-chip'
    source.textContent = data?.source === 'active' ? '主动查询' : '被动采样'
    elements.meta.append(source)
    if (data?.updated_at) {
      const updated = document.createElement('span')
      updated.className = 'meta-chip'
      updated.textContent = `更新于 ${formatDate(data.updated_at)}`
      elements.meta.append(updated)
    }

    let rendered = 0
    for (const [key, label] of windowDefinitions) {
      if (!data?.[key]) continue
      elements.windows.append(renderWindow(label, data[key]))
      rendered += 1
    }
    if (rendered === 0) {
      const empty = document.createElement('div')
      empty.className = 'empty-state'
      empty.textContent = data?.error || '当前账号暂无可显示的用量窗口'
      elements.windows.append(empty)
    }
  }

  function availableCount() {
    return Number(state.quota?.rate_limit_reset_credits?.available_count) || 0
  }

  function updateResetButton() {
    elements.resetButton.classList.toggle('is-hidden', !state.allowReset)
    const canReset = state.allowReset && state.quota !== null && availableCount() > 0 && state.busy.size === 0
    elements.resetButton.disabled = !canReset
    if (!state.allowReset) elements.resetButton.title = ''
    else if (state.quota === null) elements.resetButton.title = '请先点击“剩余重置次数”查询可用次数'
    else if (availableCount() <= 0) elements.resetButton.title = '当前没有可用重置次数'
    else elements.resetButton.title = '消耗一次额度并重置用量窗口'
  }

  function renderQuota(data) {
    state.quota = data
    elements.countLabel.textContent = `剩余重置次数 ${availableCount()}`
    const credits = data?.rate_limit_reset_credits?.credits || []
    const expirations = credits
      .map((credit) => credit?.expires_at)
      .filter(Boolean)
      .sort((left, right) => new Date(left).getTime() - new Date(right).getTime())
    if (expirations.length > 0) {
      const rest = expirations.length > 1 ? `，另有 ${expirations.length - 1} 次` : ''
      elements.creditInfo.textContent = `最早到期：${formatDate(expirations[0])}${rest}`
      elements.creditInfo.classList.remove('is-hidden')
    } else {
      elements.creditInfo.textContent = ''
      elements.creditInfo.classList.add('is-hidden')
    }
    updateResetButton()
  }

  async function loadUsage(active) {
    clearError()
    clearNotice()
    setBusy('usage', true)
    try {
      const suffix = active ? 'usage?active=1' : 'usage'
      renderUsage(await request(suffix))
    } catch (error) {
      elements.loading.classList.add('is-hidden')
      showError(error)
    } finally {
      setBusy('usage', false)
    }
  }

  async function loadQuota() {
    clearError()
    clearNotice()
    setBusy('quota', true)
    try {
      renderQuota(await request('quota'))
    } catch (error) {
      showError(error)
    } finally {
      setBusy('quota', false)
    }
  }

  async function resetQuota() {
    if (!state.allowReset || availableCount() <= 0) return
    if (!window.confirm(`确定消耗一次重置额度吗？当前可用 ${availableCount()} 次。`)) return
    clearError()
    clearNotice()
    setBusy('reset', true)
    try {
      const result = await request('reset', { method: 'POST' })
      await Promise.all([loadQuota(), loadUsage(true)])
      showNotice(`重置成功，已重置 ${Number(result?.windows_reset) || 0} 个用量窗口。`)
    } catch (error) {
      showError(error)
    } finally {
      setBusy('reset', false)
    }
  }

  async function initialize() {
    try {
      const publicConfig = await request('config')
      state.allowReset = Boolean(publicConfig.allow_reset)
      elements.accountBadge.textContent = `账号 #${publicConfig.account_id}`
      document.title = `账号 #${publicConfig.account_id} 用量`
      if (state.allowReset) {
        elements.resetPolicy.textContent = '重置功能已由服务端开启；查询到可用次数后即可操作。'
        elements.resetPolicy.classList.remove('is-hidden')
      } else {
        elements.resetPolicy.textContent = ''
        elements.resetPolicy.classList.add('is-hidden')
      }
      updateResetButton()
      await loadUsage(false)
    } catch (error) {
      elements.loading.classList.add('is-hidden')
      showError(error)
    }
  }

  elements.queryButton.addEventListener('click', () => loadUsage(true))
  elements.countButton.addEventListener('click', loadQuota)
  elements.resetButton.addEventListener('click', resetQuota)
  initialize()
})()
