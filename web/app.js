(() => {
  'use strict'

  const elements = {
    userBadge: document.getElementById('user-badge'),
    notice: document.getElementById('notice'),
    error: document.getElementById('error-panel'),
    loading: document.getElementById('dashboard-loading'),
    content: document.getElementById('dashboard-content'),
    refreshButton: document.getElementById('refresh-button'),
    confirmModal: document.getElementById('confirm-modal'),
    confirmTitle: document.getElementById('confirm-modal-title'),
    confirmMessage: document.getElementById('confirm-modal-message'),
    confirmCancel: document.getElementById('confirm-modal-cancel'),
    confirmAccept: document.getElementById('confirm-modal-accept')
  }

  const state = {
    userID: 0,
    token: '',
    loading: false,
    confirmResolve: null,
    confirmTrigger: null
  }

  const storageKeys = {
    userID: 'sub2api-accountinfo:user-id',
    token: 'sub2api-accountinfo:token'
  }

  const windowDefinitions = [
    ['five_hour', '5h'],
    ['seven_day', '7d'],
    ['seven_day_sonnet', '7d S'],
    ['seven_day_fable', '7d F']
  ]

  const accountTypeLabels = {
    oauth: 'OAuth',
    'setup-token': 'Setup Token',
    apikey: 'API Key',
    upstream: '上游转发',
    bedrock: 'AWS Bedrock',
    service_account: 'Service Account'
  }

  function initializeCredentials() {
    const url = new URL(window.location.href)
    const queryToken = url.searchParams.get('token') || ''
    const queryUserID = Number(url.searchParams.get('user_id')) || 0
    const theme = url.searchParams.get('theme')
    const lang = url.searchParams.get('lang')

    if (theme === 'light' || theme === 'dark') {
      document.documentElement.dataset.theme = theme
    }
    if (lang) document.documentElement.lang = lang

    try {
      if (queryToken) sessionStorage.setItem(storageKeys.token, queryToken)
      if (queryUserID > 0) sessionStorage.setItem(storageKeys.userID, String(queryUserID))
      state.token = queryToken || sessionStorage.getItem(storageKeys.token) || ''
      state.userID = queryUserID || Number(sessionStorage.getItem(storageKeys.userID)) || 0
    } catch {
      state.token = queryToken
      state.userID = queryUserID
    }

    if (queryToken) {
      url.searchParams.delete('token')
      window.history.replaceState(null, '', `${url.pathname}${url.search}${url.hash}`)
    }

    return state.userID > 0 && state.token !== ''
  }

  async function request(path, options = {}) {
    const response = await fetch(`/api/${path}`, {
      cache: 'no-store',
      credentials: 'same-origin',
      ...options,
      headers: {
        Authorization: `Bearer ${state.token}`,
        'X-Sub2API-User-ID': String(state.userID),
        'Accept-Language': document.documentElement.lang || 'zh-CN',
        ...(options.headers || {})
      }
    })
    let payload
    try {
      payload = await response.json()
    } catch {
      throw new Error(`服务返回了无法解析的响应（HTTP ${response.status}）`)
    }
    if (!response.ok || payload?.code !== 0) {
      throw new Error(payload?.message || `请求失败（HTTP ${response.status}）`)
    }
    return payload.data
  }

  function setPageLoading(active) {
    state.loading = active
    elements.refreshButton.disabled = active
    elements.refreshButton.classList.toggle('is-loading', active)
    if (active && elements.content.childElementCount === 0) {
      elements.loading.classList.remove('is-hidden')
    }
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

  function closeConfirmation(confirmed) {
    if (!state.confirmResolve) return
    const resolve = state.confirmResolve
    const trigger = state.confirmTrigger
    state.confirmResolve = null
    state.confirmTrigger = null
    elements.confirmModal.classList.add('is-hidden')
    document.body.classList.remove('modal-open')
    if (trigger?.isConnected) trigger.focus({ preventScroll: true })
    resolve(confirmed)
  }

  function showConfirmation(title, message, trigger) {
    if (state.confirmResolve) return Promise.resolve(false)
    elements.confirmTitle.textContent = title
    elements.confirmMessage.textContent = message
    elements.confirmModal.classList.remove('is-hidden')
    document.body.classList.add('modal-open')
    state.confirmTrigger = trigger
    return new Promise((resolve) => {
      state.confirmResolve = resolve
      window.requestAnimationFrame(() => {
        if (state.confirmResolve) elements.confirmAccept.focus()
      })
    })
  }

  function confirmReset(accountID, count, trigger) {
    return showConfirmation(
      `确认重置账号 #${accountID}`,
      `本次操作将消耗 1 次重置额度，当前可用 ${count} 次。重置后账号用量窗口将重新开始计算。`,
      trigger
    )
  }

  function confirmResetFinal(trigger) {
    return showConfirmation(
      '确定重置吗？',
      '重置次数非常珍贵，务必确认好再重置！\n如不确定可联系管理员！',
      trigger
    )
  }

  function handleConfirmationKeydown(event) {
    if (elements.confirmModal.classList.contains('is-hidden')) return
    if (event.key === 'Escape') {
      event.preventDefault()
      closeConfirmation(false)
      return
    }
    if (event.key !== 'Tab') return
    const focusable = [elements.confirmCancel, elements.confirmAccept]
    const first = focusable[0]
    const last = focusable[focusable.length - 1]
    if (!focusable.includes(document.activeElement)) {
      event.preventDefault()
      const target = event.shiftKey ? last : first
      target.focus()
    } else if (event.shiftKey && document.activeElement === first) {
      event.preventDefault()
      last.focus()
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault()
      first.focus()
    }
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

  function createChip(text, className = 'stat-chip') {
    const chip = document.createElement('span')
    chip.className = className
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
    const utilization = Math.max(0, Math.min(100, Number(usage.utilization) || 0))
    track.setAttribute('aria-valuenow', String(utilization))
    const bar = document.createElement('div')
    bar.className = 'progress-bar'
    bar.style.width = `${utilization}%`
    track.append(bar)

    const footer = document.createElement('div')
    footer.className = 'window-footer'
    const stats = document.createElement('div')
    stats.className = 'window-stats'
    const windowStats = usage.window_stats
    if (windowStats) {
      stats.append(
        createChip(`${formatCompact(windowStats.requests)} req`),
        createChip(`${formatCompact(windowStats.tokens)} tok`),
        createChip(`A $${formatMoney(windowStats.cost)}`)
      )
      if (windowStats.user_cost !== undefined && windowStats.user_cost !== null) {
        stats.append(createChip(`U $${formatMoney(windowStats.user_cost)}`))
      }
    }
    const reset = document.createElement('span')
    reset.className = 'reset-time'
    reset.title = usage.resets_at ? formatDate(usage.resets_at) : ''
    reset.textContent = `↻ ${formatResetTime(usage.resets_at, usage.utilization)}`
    footer.append(stats, reset)

    wrapper.append(header, track, footer)
    return wrapper
  }

  function availableCount(quota) {
    return Number(quota?.rate_limit_reset_credits?.available_count) || 0
  }

  function renderAccountActions(container, account, allowReset) {
    if (account.platform !== 'openai') return

    const actionRow = document.createElement('div')
    actionRow.className = 'account-actions'
    const countButton = document.createElement('button')
    countButton.type = 'button'
    countButton.className = 'button button-count'
    countButton.textContent = '剩余重置次数'
    actionRow.append(countButton)

    let resetButton = null
    if (allowReset) {
      resetButton = document.createElement('button')
      resetButton.type = 'button'
      resetButton.className = 'button button-reset'
      resetButton.textContent = '重置'
      resetButton.disabled = true
      actionRow.append(resetButton)
    }

    const creditInfo = document.createElement('div')
    creditInfo.className = 'credit-info is-hidden'
    let quota = null

    function updateResetButton() {
      if (!resetButton) return
      resetButton.disabled = quota === null || availableCount(quota) <= 0
      resetButton.title = quota === null
        ? '请先查询剩余重置次数'
        : availableCount(quota) > 0
          ? '消耗一次额度并重置用量窗口'
          : '当前没有可用重置次数'
    }

    async function loadQuota() {
      countButton.disabled = true
      countButton.classList.add('is-loading')
      clearError()
      try {
        quota = await request(`accounts/${account.id}/quota`)
        countButton.textContent = `剩余重置次数 ${availableCount(quota)}`
        const credits = quota?.rate_limit_reset_credits?.credits || []
        const expirations = credits
          .map((credit) => credit?.expires_at)
          .filter(Boolean)
          .sort((left, right) => new Date(left).getTime() - new Date(right).getTime())
        if (expirations.length > 0) {
          const rest = expirations.length > 1 ? `，另有 ${expirations.length - 1} 次` : ''
          creditInfo.textContent = `最早到期：${formatDate(expirations[0])}${rest}`
          creditInfo.classList.remove('is-hidden')
        } else {
          creditInfo.textContent = ''
          creditInfo.classList.add('is-hidden')
        }
        updateResetButton()
      } catch (error) {
        showError(error)
      } finally {
        countButton.disabled = false
        countButton.classList.remove('is-loading')
      }
    }

    countButton.addEventListener('click', loadQuota)
    if (resetButton) {
      resetButton.addEventListener('click', async () => {
        if (availableCount(quota) <= 0) return
        if (!await confirmReset(account.id, availableCount(quota), resetButton)) return
        if (!await confirmResetFinal(resetButton)) return
        resetButton.disabled = true
        resetButton.classList.add('is-loading')
        clearError()
        clearNotice()
        try {
          const result = await request(`accounts/${account.id}/reset`, { method: 'POST' })
          showNotice(`账号 #${account.id} 重置成功，已重置 ${Number(result?.windows_reset) || 0} 个用量窗口。`)
          await loadDashboard(true)
        } catch (error) {
          showError(error)
        } finally {
          resetButton.classList.remove('is-loading')
          updateResetButton()
        }
      })
    }

    container.append(actionRow, creditInfo)
  }

  function renderAccount(account, allowReset) {
    const card = document.createElement('article')
    card.className = 'account-card'

    const header = document.createElement('header')
    header.className = 'account-header'
    const identity = document.createElement('div')
    const name = document.createElement('h3')
    name.textContent = account.name || `账号 #${account.id}`
    const number = document.createElement('p')
    number.className = 'account-number'
    number.textContent = `账号 #${account.id}`
    identity.append(name, number)
    const badges = document.createElement('div')
    badges.className = 'account-badges'
    badges.append(
      createChip(account.platform || 'unknown', 'type-badge'),
      createChip(accountTypeLabels[account.type] || account.type || '未知类型', 'type-badge type-badge-accent')
    )
    if (account.status !== 'active') badges.append(createChip(account.status, 'type-badge type-badge-warning'))
    if (!account.schedulable) badges.append(createChip('不可调度', 'type-badge type-badge-warning'))
    header.append(identity, badges)

    const meta = document.createElement('div')
    meta.className = 'usage-meta'
    if (account.usage?.updated_at) meta.append(createChip(`更新于 ${formatDate(account.usage.updated_at)}`, 'meta-chip'))

    const windows = document.createElement('div')
    windows.className = 'usage-windows'
    let rendered = 0
    for (const [key, label] of windowDefinitions) {
      if (!account.usage?.[key]) continue
      windows.append(renderWindow(label, account.usage[key]))
      rendered += 1
    }
    if (rendered === 0) {
      const empty = document.createElement('div')
      empty.className = 'empty-state'
      empty.textContent = account.usage_error || account.usage?.error || '当前账号暂无可显示的用量窗口'
      windows.append(empty)
    }

    card.append(header, meta, windows)
    renderAccountActions(card, account, allowReset)
    return card
  }

  function renderDashboard(data) {
    elements.userBadge.textContent = `用户 #${data.user_id}`
    elements.content.replaceChildren()

    if (!Array.isArray(data.groups) || data.groups.length === 0) {
      const empty = document.createElement('div')
      empty.className = 'empty-state empty-dashboard'
      empty.textContent = '当前用户没有有效订阅'
      elements.content.append(empty)
    } else {
      for (const group of data.groups) {
        const section = document.createElement('section')
        section.className = 'group-section'
        const header = document.createElement('header')
        header.className = 'group-header'
        const title = document.createElement('div')
        const name = document.createElement('h2')
        name.textContent = group.name || `分组 #${group.id}`
        const detail = document.createElement('p')
        detail.textContent = `分组 #${group.id}${group.expires_at ? ` · 订阅到期 ${formatDate(group.expires_at)}` : ''}`
        title.append(name, detail)
        const groupBadges = document.createElement('div')
        groupBadges.className = 'account-badges'
        groupBadges.append(
          createChip(group.platform || 'unknown', 'type-badge'),
          createChip(`${group.accounts?.length || 0} 个账号`, 'type-badge type-badge-accent')
        )
        header.append(title, groupBadges)

        const accounts = document.createElement('div')
        accounts.className = 'account-list'
        if (!Array.isArray(group.accounts) || group.accounts.length === 0) {
          const empty = document.createElement('div')
          empty.className = 'empty-state'
          empty.textContent = '该订阅分组尚未绑定账号'
          accounts.append(empty)
        } else {
          for (const account of group.accounts) accounts.append(renderAccount(account, Boolean(data.allow_reset)))
        }
        section.append(header, accounts)
        elements.content.append(section)
      }
    }

    elements.loading.classList.add('is-hidden')
    elements.content.classList.remove('is-hidden')
  }

  async function loadDashboard(active) {
    if (state.loading) return
    setPageLoading(true)
    clearError()
    if (active) clearNotice()
    try {
      const suffix = active ? 'dashboard?active=1' : 'dashboard'
      renderDashboard(await request(suffix))
    } catch (error) {
      elements.loading.classList.add('is-hidden')
      showError(error)
    } finally {
      setPageLoading(false)
    }
  }

  elements.confirmCancel.addEventListener('click', () => closeConfirmation(false))
  elements.confirmAccept.addEventListener('click', () => closeConfirmation(true))
  elements.confirmModal.addEventListener('click', (event) => {
    if (event.target === elements.confirmModal) closeConfirmation(false)
  })
  document.addEventListener('keydown', handleConfirmationKeydown)

  if (!initializeCredentials()) {
    elements.loading.classList.add('is-hidden')
    elements.refreshButton.disabled = true
    showError(new Error('链接缺少有效的 user_id 或 token，请从 Sub2API 内重新打开此页面'))
  } else {
    elements.refreshButton.addEventListener('click', () => loadDashboard(true))
    loadDashboard(false)
  }
})()
