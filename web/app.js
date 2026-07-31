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
    ['five_hour', '5小时'],
    ['seven_day', '7天'],
    ['seven_day_sonnet', '7天 S'],
    ['seven_day_fable', '7天 F']
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

  function estimateWindowBudget(usage) {
    const utilization = Number(usage?.utilization)
    const spent = Number(usage?.window_stats?.cost)
    if (!Number.isFinite(utilization) || utilization <= 0 || !Number.isFinite(spent) || spent <= 0) {
      return null
    }
    const total = spent / (utilization / 100)
    if (!Number.isFinite(total) || total <= 0) return null
    return {
      total,
      remaining: Math.max(0, total - spent)
    }
  }

  function windowProgressValues(usage) {
    const accountUtilization = Math.max(0, Math.min(100, Number(usage?.utilization) || 0))
    const userUtilization = Number(usage?.user_utilization)
    const hasUser = usage?.user_utilization !== undefined &&
      usage?.user_utilization !== null &&
      Number.isFinite(userUtilization) &&
      userUtilization >= 0
    return {
      account: accountUtilization,
      user: hasUser ? Math.min(accountUtilization, Math.max(0, Math.min(100, userUtilization))) : null,
      hasUser
    }
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

  function formatFullDate(value) {
    const date = new Date(value)
    if (Number.isNaN(date.getTime())) return String(value || '到期时间未提供')
    return new Intl.DateTimeFormat(undefined, {
      year: 'numeric',
      month: '2-digit',
      day: '2-digit',
      hour: '2-digit',
      minute: '2-digit',
      second: '2-digit',
      hour12: false
    }).format(date)
  }

  function formatExpiryDistance(value) {
    const expiresAt = new Date(value).getTime()
    if (!Number.isFinite(expiresAt)) return '到期时间格式未知'
    const remaining = expiresAt - Date.now()
    if (remaining <= 0) return '已到期'
    const minutes = Math.max(1, Math.ceil(remaining / 60000))
    const days = Math.floor(minutes / 1440)
    const hours = Math.floor((minutes % 1440) / 60)
    const restMinutes = minutes % 60
    if (days > 0) return `${days}天${hours > 0 ? ` ${hours}小时` : ''}后到期`
    if (hours > 0) return `${hours}小时${restMinutes > 0 ? ` ${restMinutes}分钟` : ''}后到期`
    return `${restMinutes}分钟后到期`
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

  function formatResetStatus(value, utilization) {
    const distance = formatResetTime(value, utilization)
    if (distance === '等待刷新') return distance
    if (distance === '现在') return '即将重置'
    if (distance === '-') return '重置时间未知'
    return `${distance}后重置`
  }

  function hasRenderableUsageWindow(usage) {
    if (!usage || typeof usage !== 'object') return false
    if (usage.reset_time_pending) return true
    if (!usage.resets_at) return false
    return Number.isFinite(new Date(usage.resets_at).getTime())
  }

  function createChip(text, className = 'stat-chip') {
    const chip = document.createElement('span')
    chip.className = className
    chip.textContent = text
    return chip
  }

  function createWindowMetric(label, value, className = '') {
    const metric = document.createElement('div')
    metric.className = `window-metric${className ? ` ${className}` : ''}`
    const labelNode = document.createElement('span')
    labelNode.className = 'window-metric-label'
    labelNode.textContent = label
    const valueNode = document.createElement('strong')
    valueNode.className = 'window-metric-value'
    valueNode.textContent = value
    metric.append(labelNode, valueNode)
    return metric
  }

  function createWindowDetailGroup(title, metrics, className = '') {
    const group = document.createElement('section')
    group.className = `window-detail-group${className ? ` ${className}` : ''}`
    const heading = document.createElement('p')
    heading.className = 'window-detail-title'
    heading.textContent = title
    const content = document.createElement('div')
    content.className = 'window-metrics'
    content.append(...metrics)
    group.append(heading, content)
    return group
  }

  function createWindowAlert(text) {
    const alert = document.createElement('p')
    alert.className = 'window-alert'
    alert.textContent = text
    return alert
  }

  function createWindowPercentage(label, value, className) {
    const item = document.createElement('span')
    item.className = `window-percentage ${className}`
    const labelNode = document.createElement('span')
    labelNode.className = 'window-percentage-label'
    labelNode.textContent = label
    const valueNode = document.createElement('strong')
    valueNode.className = 'window-percentage-value'
    valueNode.textContent = value
    item.append(labelNode, valueNode)
    return item
  }

  function renderWindow(label, usage) {
    const wrapper = document.createElement('section')
    wrapper.className = 'usage-window'

    const header = document.createElement('div')
    header.className = 'window-header'
    const heading = document.createElement('div')
    heading.className = 'window-heading'
    const labelNode = document.createElement('span')
    labelNode.className = 'window-label'
    labelNode.textContent = label
    const reset = document.createElement('span')
    reset.className = 'window-reset-status'
    reset.title = usage.reset_time_pending
      ? '暂未获取到重置时间，请等待使用一些时间后再查看'
      : (usage.resets_at ? formatDate(usage.resets_at) : '')
    reset.textContent = usage.reset_time_pending
      ? '重置时间待确认'
      : formatResetStatus(usage.resets_at, usage.utilization)
    if (usage.reset_time_pending) reset.classList.add('is-pending')
    heading.append(labelNode, reset)
    const progress = windowProgressValues(usage)
    const userPercentageText = progress.hasUser
      ? formatPercent(progress.user)
      : usage.reset_time_pending
        ? '待确认'
        : usage.user_window_stats_unavailable
          ? '暂不可用'
          : '待统计'
    const percentages = document.createElement('div')
    percentages.className = 'window-percentages'
    percentages.append(
      createWindowPercentage('您的用量', userPercentageText, 'window-percentage-user'),
      createWindowPercentage(
        '账号总用量',
        formatPercent(progress.account),
        'window-percentage-account'
      )
    )
    if (progress.hasUser) {
      percentages.title = usage.user_utilization_estimated
        ? '您的用量比例按账号用量和用户消费占比估算'
        : '您的用量与账号总用量'
    }
    header.append(heading, percentages)

    const track = document.createElement('div')
    track.className = 'progress-track'
    track.setAttribute('role', 'progressbar')
    track.setAttribute('aria-valuemin', '0')
    track.setAttribute('aria-valuemax', '100')
    track.setAttribute('aria-valuenow', String(progress.account))
    track.setAttribute(
      'aria-valuetext',
      progress.hasUser
        ? `账号总用量 ${formatPercent(progress.account)}，您的用量 ${formatPercent(progress.user)}`
        : `账号总用量 ${formatPercent(progress.account)}，您的用量比例${userPercentageText}`
    )
    const accountBar = document.createElement('div')
    accountBar.className = 'progress-bar progress-bar-account'
    accountBar.style.width = `${progress.account}%`
    accountBar.title = `账号总用量 ${formatPercent(progress.account)}`
    const userBar = document.createElement('div')
    userBar.className = 'progress-bar progress-bar-user'
    userBar.style.width = progress.hasUser ? `${progress.user}%` : '0%'
    userBar.title = progress.hasUser ? `您的用量 ${formatPercent(progress.user)}` : `您的用量比例${userPercentageText}`
    track.append(accountBar, userBar)

    const alerts = document.createElement('div')
    alerts.className = 'window-alerts'
    if (usage.reset_time_pending) {
      alerts.append(createWindowAlert('暂未获取到重置时间，请等待使用一些时间后再查看'))
    }

    const details = document.createElement('div')
    details.className = 'window-details'
    const windowStats = usage.window_stats
    if (windowStats) {
      details.append(createWindowDetailGroup('账号用量', [
        createWindowMetric('请求数', formatCompact(windowStats.requests)),
        createWindowMetric('令牌', `${formatCompact(windowStats.tokens)} Token`),
        createWindowMetric('消费', `$${formatMoney(windowStats.cost)}`)
      ], 'window-detail-group-account'))

      const budget = estimateWindowBudget(usage)
      if (budget) {
        details.append(createWindowDetailGroup('额度估算', [
          createWindowMetric('总额度', `$${formatMoney(budget.total)}`, 'window-metric-estimate'),
          createWindowMetric('剩余额度', `$${formatMoney(budget.remaining)}`, 'window-metric-remaining')
        ], 'window-detail-group-budget'))
      }
    }
    const userWindowStats = usage.user_window_stats
    if (userWindowStats && userWindowStats.cost !== undefined && userWindowStats.cost !== null) {
      const userCostLabel = usage.reset_time_pending ? '临时统计消费' : '窗口消费'
      details.append(createWindowDetailGroup('您的用量', [
        createWindowMetric(userCostLabel, `$${formatMoney(userWindowStats.cost)}`, 'window-metric-user')
      ], 'window-detail-group-user'))
    } else if (usage.user_window_stats_unavailable) {
      details.append(createWindowDetailGroup('您的用量', [
        createWindowMetric('消费统计', '暂不可用', 'window-metric-warning')
      ], 'window-detail-group-user window-detail-group-warning'))
    }

    wrapper.append(header, track)
    if (alerts.childElementCount > 0) wrapper.append(alerts)
    if (details.childElementCount > 0) wrapper.append(details)
    return wrapper
  }

  function availableCount(quota) {
    return Number(quota?.rate_limit_reset_credits?.available_count) || 0
  }

  function resetCreditExpirations(quota) {
    const credits = quota?.rate_limit_reset_credits?.credits
    if (!Array.isArray(credits)) return []
    return credits
      .map((credit, index) => ({
        expiresAt: String(credit?.expires_at || '').trim(),
        index
      }))
      .sort((left, right) => {
        const leftTime = new Date(left.expiresAt).getTime()
        const rightTime = new Date(right.expiresAt).getTime()
        const safeLeftTime = Number.isFinite(leftTime) ? leftTime : Number.POSITIVE_INFINITY
        const safeRightTime = Number.isFinite(rightTime) ? rightTime : Number.POSITIVE_INFINITY
        return safeLeftTime - safeRightTime || left.index - right.index
      })
  }

  function renderCreditPanel(panel, quota) {
    const count = availableCount(quota)
    const credits = resetCreditExpirations(quota)
    const summary = document.createElement('div')
    summary.className = 'credit-summary'

    const summaryIcon = document.createElement('span')
    summaryIcon.className = 'credit-summary-icon'
    summaryIcon.setAttribute('aria-hidden', 'true')
    summaryIcon.textContent = '↻'

    const summaryCopy = document.createElement('div')
    summaryCopy.className = 'credit-summary-copy'
    const kicker = document.createElement('p')
    kicker.className = 'credit-kicker'
    kicker.textContent = 'RESET CREDITS'
    const title = document.createElement('h4')
    title.textContent = '重置额度'
    const description = document.createElement('p')
    description.textContent = '每次重置用量窗口会消耗 1 次额度'
    summaryCopy.append(kicker, title, description)

    const countBadge = document.createElement('div')
    countBadge.className = `credit-count${count > 0 ? '' : ' is-empty'}`
    const countValue = document.createElement('strong')
    countValue.textContent = String(count)
    const countLabel = document.createElement('span')
    countLabel.textContent = '次可用'
    countBadge.append(countValue, countLabel)
    summary.append(summaryIcon, summaryCopy, countBadge)

    const expirationSection = document.createElement('div')
    expirationSection.className = 'credit-expirations'
    const expirationHeader = document.createElement('div')
    expirationHeader.className = 'credit-expiration-header'
    const expirationTitle = document.createElement('span')
    expirationTitle.textContent = '额度到期时间'
    const timezoneHint = document.createElement('span')
    timezoneHint.textContent = '按当前设备时区显示'
    expirationHeader.append(expirationTitle, timezoneHint)
    expirationSection.append(expirationHeader)

    if (credits.length > 0) {
      const list = document.createElement('ol')
      list.className = 'credit-expiration-list'
      credits.forEach((credit, index) => {
        const item = document.createElement('li')
        const label = document.createElement('span')
        label.className = 'credit-expiration-label'
        label.textContent = `额度 ${index + 1}`
        const timing = document.createElement('div')
        timing.className = 'credit-expiration-timing'
        const time = document.createElement('time')
        time.textContent = formatFullDate(credit.expiresAt)
        if (credit.expiresAt) {
          time.dateTime = credit.expiresAt
          time.title = credit.expiresAt
        }
        const distance = document.createElement('span')
        distance.textContent = credit.expiresAt ? formatExpiryDistance(credit.expiresAt) : '服务端未返回到期时间'
        timing.append(time, distance)
        item.append(label, timing)
        list.append(item)
      })
      expirationSection.append(list)
      const missingCount = Math.max(count - credits.length, 0)
      if (missingCount > 0) {
        const missing = document.createElement('p')
        missing.className = 'credit-expiration-note'
        missing.textContent = `另有 ${missingCount} 次额度未返回具体到期时间`
        expirationSection.append(missing)
      }
    } else {
      const empty = document.createElement('p')
      empty.className = 'credit-expiration-empty'
      empty.textContent = count > 0 ? '服务端暂未返回具体到期时间' : '当前没有可用的重置额度'
      expirationSection.append(empty)
    }

    panel.replaceChildren(summary, expirationSection)
    panel.classList.remove('is-hidden')
  }

  function renderAccountActions(container, account, allowReset) {
    if (account.platform !== 'openai' || account.type !== 'oauth') return

    const actionRow = document.createElement('div')
    actionRow.className = 'account-actions'
    const countButton = document.createElement('button')
    countButton.type = 'button'
    countButton.className = 'button quota-query-button'
    const countIcon = document.createElement('span')
    countIcon.className = 'button-icon quota-query-icon'
    countIcon.setAttribute('aria-hidden', 'true')
    const countGlyph = document.createElement('span')
    countGlyph.className = 'quota-query-glyph'
    countGlyph.textContent = '↻'
    countIcon.append(countGlyph)
    const countCopy = document.createElement('span')
    countCopy.className = 'quota-query-copy'
    const countLabel = document.createElement('span')
    countLabel.className = 'quota-query-label'
    countLabel.textContent = '查看重置额度'
    const countHint = document.createElement('span')
    countHint.className = 'quota-query-hint'
    countHint.textContent = '剩余次数与全部到期时间'
    countCopy.append(countLabel, countHint)
    countButton.append(countIcon, countCopy)
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

    const creditPanel = document.createElement('section')
    creditPanel.id = `credit-panel-${account.id}`
    creditPanel.className = 'credit-panel is-hidden'
    creditPanel.setAttribute('aria-live', 'polite')
    countButton.setAttribute('aria-controls', creditPanel.id)
    countButton.setAttribute('aria-expanded', 'false')
    let quota = null

    function updateResetButton() {
      if (!resetButton) return
      resetButton.disabled = quota === null || availableCount(quota) <= 0
      resetButton.title = quota === null
        ? '请先查看重置额度'
        : availableCount(quota) > 0
          ? '消耗一次额度并重置用量窗口'
          : '当前没有可用重置次数'
    }

    async function loadQuota() {
      countButton.disabled = true
      countButton.classList.add('is-loading')
      countLabel.textContent = quota === null ? '正在查询额度…' : '正在刷新额度…'
      countHint.textContent = '正在获取最新额度信息'
      clearError()
      try {
        quota = await request(`accounts/${account.id}/quota`)
        renderCreditPanel(creditPanel, quota)
        countButton.setAttribute('aria-expanded', 'true')
        updateResetButton()
      } catch (error) {
        showError(error)
      } finally {
        countLabel.textContent = quota === null ? '重新查询额度' : '刷新重置额度'
        countHint.textContent = quota === null
          ? '查询失败，点击重新获取'
          : `当前剩余 ${availableCount(quota)} 次，点击刷新`
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

    container.append(actionRow, creditPanel)
  }

  function renderAccount(account, allowReset) {
    const renderableWindows = windowDefinitions.filter(([key]) => (
      hasRenderableUsageWindow(account.usage?.[key])
    ))
    if (renderableWindows.length === 0) return null

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
    for (const [key, label] of renderableWindows) {
      windows.append(renderWindow(label, account.usage[key]))
    }

    card.append(header, meta, windows)
    renderAccountActions(card, account, allowReset)
    return card
  }

  function renderAutoResetBanner(schedules) {
    const banner = document.createElement('aside')
    banner.className = 'auto-reset-banner'
    banner.setAttribute('role', 'status')
    const icon = document.createElement('span')
    icon.className = 'auto-reset-banner-icon'
    icon.setAttribute('aria-hidden', 'true')
    icon.textContent = '◷'
    const copy = document.createElement('div')
    copy.className = 'auto-reset-banner-copy'
    const intro = document.createElement('div')
    intro.className = 'auto-reset-banner-intro'
    const title = document.createElement('strong')
    title.textContent = '自动重置已开启'
    const description = document.createElement('p')
    description.textContent = '系统会在重置额度到期前 10 分钟自动使用。'
    intro.append(title, description)

    const schedulePanel = document.createElement('div')
    schedulePanel.className = 'auto-reset-schedule'
    const scheduleTitle = document.createElement('strong')
    scheduleTitle.className = 'auto-reset-schedule-title'
    scheduleTitle.textContent = '已识别的自动重置时间'
    schedulePanel.append(scheduleTitle)
    if (Array.isArray(schedules) && schedules.length > 0) {
      const scheduleList = document.createElement('ul')
      scheduleList.className = 'auto-reset-schedule-list'
      schedules.forEach((schedule) => {
        const item = document.createElement('li')
        item.className = 'auto-reset-schedule-item'
        const account = document.createElement('span')
        account.className = 'auto-reset-schedule-account'
        account.textContent = `账号 #${schedule.account_id}`
        const timing = document.createElement('div')
        timing.className = 'auto-reset-schedule-timing'
        const resetAt = document.createElement('time')
        resetAt.textContent = `预计 ${formatFullDate(schedule.reset_at)} 执行`
        if (schedule.reset_at) {
          resetAt.dateTime = schedule.reset_at
          resetAt.title = schedule.reset_at
        }
        const expiresAt = document.createElement('span')
        expiresAt.textContent = schedule.expires_at
          ? `额度 ${formatFullDate(schedule.expires_at)} 到期`
          : '额度到期时间未提供'
        timing.append(resetAt, expiresAt)
        item.append(account, timing)
        scheduleList.append(item)
      })
      schedulePanel.append(scheduleList)
    } else {
      const empty = document.createElement('p')
      empty.className = 'auto-reset-schedule-empty'
      empty.textContent = '当前暂未识别到会执行的自动重置，系统会继续扫描账号和额度。'
      schedulePanel.append(empty)
    }

    copy.append(intro, schedulePanel)
    banner.append(icon, copy)
    return banner
  }

  function renderDashboard(data) {
    elements.userBadge.textContent = `用户 #${data.user_id}`
    elements.content.replaceChildren()
    if (data.auto_reset_credits) {
      elements.content.append(renderAutoResetBanner(data.auto_reset_schedules))
    }

    if (!Array.isArray(data.groups) || data.groups.length === 0) {
      const empty = document.createElement('div')
      empty.className = 'empty-state empty-dashboard'
      empty.textContent = '当前用户没有有效订阅'
      elements.content.append(empty)
    } else {
      for (const group of data.groups) {
        const sourceAccounts = Array.isArray(group.accounts) ? group.accounts : []
        const renderedAccountCards = sourceAccounts
          .map((account) => renderAccount(account, Boolean(data.allow_reset)))
          .filter(Boolean)
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
          createChip(`${renderedAccountCards.length} 个账号`, 'type-badge type-badge-accent')
        )
        header.append(title, groupBadges)

        const accounts = document.createElement('div')
        accounts.className = 'account-list'
        if (sourceAccounts.length === 0) {
          const empty = document.createElement('div')
          empty.className = 'empty-state'
          empty.textContent = '该订阅分组尚未绑定账号'
          accounts.append(empty)
        } else if (renderedAccountCards.length === 0) {
          const empty = document.createElement('div')
          empty.className = 'empty-state'
          empty.textContent = '该订阅分组暂无可显示的用量窗口'
          accounts.append(empty)
        } else {
          accounts.append(...renderedAccountCards)
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
