function claimablePlans(preview) {
  const plans = new Map();
  for (const plan of preview.plans || []) {
    if (Number(plan.ends_at) > 0 && Number(plan.ends_at) * 1000 <= Date.now()) continue;
    if (plan.plan_id && !plans.has(plan.plan_id)) plans.set(plan.plan_id, plan);
  }
  return [...plans.values()];
}

let claimCaptchaScript, claimCaptchaSequence = 0;
const manualClaimConcurrency = 5;
let claimBrowserVerificationQueue = Promise.resolve();
const claimBrowserVerifications = new Map();
const claimJobKey = job => JSON.stringify([job.account_id, job.plan_id]);
const claimDay = (at = Date.now()) => new Date(Number(at) + 8 * 3600000).toISOString().slice(0, 10);
let claimVerificationState = { day: '', items: {} };
function claimNeedsVerification(result) {
  return result && !result.ok && (result.requires_verification || result.failure_kind === 'account_risk' ||
    ['3012', 'upstream_risk_blocked', 'captcha_risk_blocked', 'F001'].includes(String(result.code)));
}
function claimResultMessage(result) {
  const code = result.code ?? result.httpStatus;
  const message = result.message || result.error || '领取失败';
  return `${message}${code !== undefined && code !== null && code !== '' && !String(message).includes(String(code)) ? `（${code}）` : ''}`;
}
function claimVerificationRecords() {
  // Display only this page session's manual results. Historical scheduled
  // failures must not trigger a dialog or decide how the next click is handled.
  if (claimVerificationState.day !== claimDay()) claimVerificationState = { day: claimDay(), items: {} };
  return claimVerificationState.items;
}
function updateClaimVerification(job, patch) {
  const items = claimVerificationRecords(), key = claimJobKey(job);
  items[key] = { ...items[key], job, ...patch };
  window.dispatchEvent(new CustomEvent('claim-verification-change', { detail: { job, ...patch } }));
}
function pendingClaimVerification(job) {
  const entry = claimVerificationRecords()[claimJobKey(job)];
  return entry && !entry.resolved ? entry : null;
}
function rememberClaimResult(job, result) {
  if (result?.ok) updateClaimVerification(job, { resolved: true, result: null });
  else if (claimNeedsVerification(result)) {
    updateClaimVerification(job, { resolved: false, result: {
      ok: false, code: result.code, message: claimResultMessage(result), requires_verification: true,
      failure_kind: result.failure_kind, manual_verification_failed: result.manual_verification_failed,
      verification_retry_at: result.verification_retry_at || Date.now() + Math.max(0, Number(result.retry_after_ms) || 0),
    } });
  }
}
// The SDK requires more than two seconds before verification.
const claimCaptchaReadyDelay = 2100;
function loadClaimCaptchaScript() {
  if (typeof window.initAliyunCaptcha === 'function') return Promise.resolve();
  if (!claimCaptchaScript) {
    claimCaptchaScript = new Promise((resolve, reject) => {
      const script = document.createElement('script');
      const timeout = setTimeout(() => fail(), 15000);
      const fail = () => { clearTimeout(timeout); script.remove(); reject(new Error('验证组件加载失败，请检查网络后重试。')); };
      script.src = 'https://o.alicdn.com/captcha-frontend/aliyunCaptcha/AliyunCaptcha.js';
      script.async = true;
      script.onload = () => { clearTimeout(timeout); typeof window.initAliyunCaptcha === 'function' ? resolve() : fail(); };
      script.onerror = fail;
      document.head.appendChild(script);
    }).catch(error => { claimCaptchaScript = null; throw error; });
  }
  return claimCaptchaScript;
}

async function requestClaimCaptcha(config, label, { isActive = () => true, refreshConfig } = {}) {
  return new Promise((resolve, reject) => {
    const mountID = `claim-captcha-${++claimCaptchaSequence}`;
    const form = h(`<div><p class="dialog-intro">${esc(label)} ${infoTip('点击“人工领取”后启动官方验证，需要交互时显示验证码；验证通过后提交一次领取，仍被拒绝时不自动重试。3012 属于账号风控，验证码不保证解除。', '人工验证')}</p><div id="${mountID}"></div><p class="quiet-message" data-captcha-status role="status">${esc(claimResultMessage(config))}</p><div class="modal-actions"><button type="button" class="btn" data-cancel>取消</button><button type="button" class="btn primary" id="${mountID}-start">人工领取</button></div></div>`);
    const status = $('[data-captcha-status]', form), mount = $(`#${mountID}`, form);
    let instance, dialog, observer, timeout, readyTimer, initializationTimer, cooldownTimer, sessionTimer;
    let epoch = 0, settled = false, initializing = false, verifying = false, cooling = false;
    let startButton = $(`#${mountID}-start`, form);
    const setVerifying = value => {
      verifying = value;
      startButton.classList.toggle('loading', value);
      startButton.setAttribute('aria-busy', String(value));
    };
    const finish = (ticket, error) => {
      if (settled) return;
      settled = true;
      epoch++;
      clearTimeout(timeout);
      clearTimeout(readyTimer);
      clearTimeout(initializationTimer);
      clearInterval(cooldownTimer);
      clearTimeout(sessionTimer);
      observer?.disconnect();
      window.removeEventListener('pagehide', cancel);
      try { instance?.destroy?.(); } catch { }
      dialog?.close();
      error ? reject(error) : resolve(ticket);
    };
    const cancel = () => finish(null);
    const setDeadline = (wait = 0) => {
      clearTimeout(timeout);
      timeout = setTimeout(() => finish(null, new Error('验证超时，请重新点击“领取”。')), Math.min(2147483647, wait + 120000));
    };
    const bindStart = () => {
      // Remove old SDK listeners before binding a fresh session/trigger.
      const button = startButton.cloneNode(false);
      startButton.replaceWith(button);
      startButton = button;
      startButton.addEventListener('click', event => {
        if (settled || initializing || verifying || cooling || startButton.disabled) {
          event.stopImmediatePropagation(); return;
        }
        if (!instance) { event.stopImmediatePropagation(); void initialize(); return; }
        setVerifying(true);
        status.classList.remove('error');
        status.textContent = '正在验证，请按官方组件提示操作…';
      }, { capture: true });
    };
    dialog = modal('需要人工验证', form, cancel, { externalFocus: true });
    observer = new MutationObserver(() => { if (!form.isConnected || !isActive()) finish(null); });
    observer.observe(document.body, { childList: true, subtree: true });
    window.addEventListener('pagehide', cancel);
    $('[data-cancel]', form).addEventListener('click', () => finish(null));
    const initializationFailed = error => {
      if (settled) return;
      epoch++;
      initializing = false;
      clearTimeout(readyTimer);
      clearTimeout(initializationTimer);
      clearTimeout(sessionTimer);
      try { instance?.destroy?.(); } catch { }
      instance = null;
      mount.replaceChildren();
      bindStart();
      setVerifying(false);
      status.textContent = error?.message || '验证组件加载失败，请点击重新验证。';
      status.classList.add('error');
      startButton.textContent = '重新验证';
      startButton.disabled = false;
    };
    const waitForCooldown = retryAt => {
      cooling = true;
      status.classList.remove('error');
      setDeadline(Math.max(0, retryAt - Date.now()));
      const tick = () => {
        if (settled || !isActive()) { finish(null); return; }
        const seconds = Math.max(0, Math.ceil((retryAt - Date.now()) / 1000));
        startButton.disabled = seconds > 0;
        startButton.textContent = seconds ? '冷却中' : '继续验证';
        status.textContent = seconds ? `风控冷却中，${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, '0')} 后可继续。` : '冷却已结束，可继续人工验证。';
        if (!seconds) { cooling = false; clearInterval(cooldownTimer); }
      };
      clearInterval(cooldownTimer);
      cooldownTimer = setInterval(tick, 1000);
      tick();
    };
    async function initialize() {
      if (settled || initializing) return;
      initializing = true;
      setDeadline();
      const current = ++epoch;
      clearTimeout(readyTimer);
      clearTimeout(initializationTimer);
      clearTimeout(sessionTimer);
      try { instance?.destroy?.(); } catch { }
      instance = null;
      mount.replaceChildren();
      bindStart();
      setVerifying(false);
      startButton.textContent = '开始验证';
      startButton.disabled = true;
      status.classList.remove('error');
      status.textContent = '正在加载验证…';
      try {
        if (!isActive()) { finish(null); return; }
        if (config?.verification_retry_at > Date.now()) { waitForCooldown(config.verification_retry_at); return; }
        const fresh = await refreshConfig();
        if (settled || current !== epoch) return;
        if (!fresh || !isActive()) { finish(null); return; }
        config = fresh;
        if (config.verification_retry_at > Date.now()) { waitForCooldown(config.verification_retry_at); return; }
        if (!config.enabled || !config.scene_id || !config.verification_session) throw new Error('上游未提供可用验证场景，请稍后处理。');
        if (config.expires_at) sessionTimer = setTimeout(() => {
          if (!settled && current === epoch) initializationFailed(new Error('验证会话已过期，请重新加载。'));
        }, Math.max(0, config.expires_at - Date.now() - 1000));
        window.AliyunCaptchaConfig = { region: config.region, prefix: config.prefix };
        await loadClaimCaptchaScript();
        if (settled || current !== epoch) return;
        initializationTimer = setTimeout(() => {
          if (!settled && current === epoch) initializationFailed(new Error('验证组件初始化超时，请重新验证。'));
        }, 15000);
        const initialization = window.initAliyunCaptcha({
          SceneId: config.scene_id,
          mode: 'popup', language: 'cn', element: `#${mountID}`, button: `#${mountID}-start`, delayBeforeSuccess: false,
          getInstance(value) {
            if (settled || current !== epoch) { try { value?.destroy?.(); } catch {} return; }
            if (!value) { initializationFailed(); return; }
            clearTimeout(initializationTimer);
            // A failed interaction can make the SDK replace its own instance.
            // Keep that replacement without starting another verification.
            if (instance) { instance = value; return; }
            instance = value;
            status.textContent = '准备验证…';
            readyTimer = setTimeout(() => {
              if (settled || current !== epoch || !instance) return;
              if (!isActive()) { finish(null); return; }
              startButton.disabled = false;
              // The SDK-bound button starts the scene's actual verification.
              // show() only controls visual challenges and can do nothing for
              // traceless scenes, even when the method exists on the instance.
              startButton.click();
            }, claimCaptchaReadyDelay);
          },
          success(param) {
            if (settled || current !== epoch) return;
            if (!isActive()) { finish(null); return; }
            if (config.expires_at && config.expires_at <= Date.now() + 1000) { initializationFailed(new Error('验证会话已过期，请重新加载。')); return; }
            if (typeof param !== 'string' || !param.trim()) { initializationFailed(new Error('验证码结果无效，请重试。')); return; }
            finish({ captcha_verify_param: param, captcha_region: config.region, verification_session: config.verification_session });
          },
          fail(value) {
            if (settled || current !== epoch) return;
            const result = value?.Result || value?.result || value || {};
            const code = String(result.VerifyCode || result.verifyCode || value?.verifyCode || '');
            if (['F008', 'F014', 'F018', 'F019', 'F020'].includes(code)) {
              initializationFailed(new Error('验证会话已失效，请重新验证。'));
              return;
            }
            // F015 means the interaction failed (for example a misplaced
            // puzzle). The SDK refreshes it; do not destroy the user's retry.
            setVerifying(false);
            status.textContent = code === 'F015' ? '验证未通过，请在验证码中重试。' : `验证未通过${/^[FT][0-9]{3}$/.test(code) ? `（${code}）` : ''}，请重试。`;
            status.classList.add('error');
            startButton.textContent = '重新验证';
            startButton.disabled = false;
          },
          onError(error) {
            if (settled || current !== epoch) return;
            const code = String(error?.code || '');
            initializationFailed(new Error(`验证组件加载失败${/^[A-Za-z0-9_.-]{1,64}$/.test(code) ? `（${code}）` : ''}，请重新验证。`));
          },
          onClose(reason) { if (!settled && current === epoch && reason === 'userDismiss') finish(null); },
        });
        Promise.resolve(initialization).catch(() => { if (!settled && current === epoch) initializationFailed(); });
      } catch (error) { if (!settled && current === epoch) initializationFailed(error); }
      finally { if (current === epoch) initializing = false; }
    }
    // The first dialog only reports the failure. Do not fetch a verification
    // session or load the SDK until the user explicitly chooses to continue.
    bindStart();
    startButton.textContent = '人工领取';
  });
}

async function submitClaim(job, label, isActive = () => true) {
  if (!isActive()) return null;
  // Each click gets its own result first. Neither a scheduled failure nor a
  // previous canceled dialog can send the user straight to verification.
  const result = await api('/api/claims/claim', { method: 'POST', body: job });
  rememberClaimResult(job, result);
  if (!isActive()) return null;
  if (!claimNeedsVerification(result)) return result;
  return queueClaimBrowserVerification(job, label, isActive, result);
}

function queueClaimBrowserVerification(job, label, isActive, failure) {
  const key = claimJobKey(job), day = claimDay();
  if (claimBrowserVerifications.has(key)) return claimBrowserVerifications.get(key).promise;
  // The page SDK has shared configuration and a shared modal surface. Queue
  // only human verification; obtain short-lived sessions inside the dialog.
  const verify = async () => {
    if (!isActive() || claimDay() !== day) return null;
    if (document.hidden || document.querySelector('.modal-bg:not(.closing)')) return null;
    const result = await submitClaimWithBrowser(job, label, () => isActive() && claimDay() === day, failure);
    if (claimDay() === day) rememberClaimResult(job, result);
    return result;
  };
  // Coordinate tabs too, while retaining the in-memory queue on older browsers.
  const run = () => navigator.locks?.request
    ? navigator.locks.request('z2a-claim-verification', verify)
    : verify();
  const pending = claimBrowserVerificationQueue.then(run, run).finally(() => {
    claimBrowserVerifications.delete(key);
    window.dispatchEvent(new CustomEvent('claim-verification-change'));
  });
  claimBrowserVerifications.set(key, { job, promise: pending });
  window.dispatchEvent(new CustomEvent('claim-verification-change'));
  claimBrowserVerificationQueue = pending.catch(() => {});
  return pending;
}

async function submitClaimWithBrowser(job, label, isActive, result) {
  if (!isActive()) return null;
  let config, closed = false;
  const releaseConfig = value => {
    if (value?.verification_session) void api('/api/claims/verification/cancel', { method: 'POST', body: { verification_session: value.verification_session } }).catch(() => {});
  };
  const refreshConfig = async () => {
    releaseConfig(config);
    config = null;
    const fresh = await api('/api/claims/verification?account_id=' + encodeURIComponent(job.account_id) + '&plan_id=' + encodeURIComponent(job.plan_id));
    if (closed || !isActive()) { releaseConfig(fresh); return null; }
    config = fresh;
    if (fresh.verification_retry_at) updateClaimVerification(job, { result: { ...result, verification_retry_at: fresh.verification_retry_at } });
    return config;
  };
  try {
    const verification = await requestClaimCaptcha(result, label, { isActive, refreshConfig });
    if (!verification || !isActive()) return null;
    // One upstream submission per dialog, even if the verified ticket is refused.
    return await api('/api/claims/claim', { method: 'POST', body: { ...job, ...verification } });
  } finally {
    closed = true;
    releaseConfig(config);
  }
}

function claimAccountMarkup(preview, account = {}) {
  return claimablePlans(preview).map(plan => {
    const pending = pendingClaimVerification({ account_id: preview.account_id, plan_id: plan.plan_id });
    const result = pending?.result;
    const details = [preview.stale ? `本次查询未更新，保留上次仍有效的卡片。${preview.error || ''}` : '', ...(result ? [claimResultMessage(result), result.manual_verification_failed ? '人工验证后仍未领取成功，可稍后重新点击“领取”。' : '再次点击“领取”时，将根据本次结果决定是否需要人工验证。', result.verification_retry_at > Date.now() ? `冷却至 ${new Date(result.verification_retry_at).toLocaleTimeString('zh-CN', { timeZone: 'Asia/Shanghai', hour12: false })}（北京时间）。` : ''] : [])].filter(Boolean).join('\n');
    return `<div class="daily-claim-row${pending ? ' needs-verification' : ''}${preview.stale ? ' query-stale' : ''}">
    ${accountNameMarkup({ ...account, id: preview.account_id }, 'claim-account')}
    <div class="daily-claim-grants">${(plan.entitlements || []).map(entitlement => `<div><strong title="${esc(fmtInt(entitlement.grant_units))} ${esc(entitlement.unit_type || 'Token')}">${fmtQuota(entitlement.grant_units)}<small> ${esc(entitlement.unit_type || 'Token')}</small></strong><span>${esc(entitlement.show_name || entitlement.meter || '')}</span></div>`).join('') || '<span class="muted">额度以 ZCode 为准</span>'}</div>
    <div class="daily-claim-action">${details ? infoTip(details, '领取待处理') : ''}<button type="button" class="btn primary" data-claim="${esc(preview.account_id)}" data-plan="${esc(plan.plan_id)}" aria-label="领取 ${esc(account.platform_name || '当前账号')} 的每日免费 Token">领取</button></div>
  </div>`;
  }).join('');
}

const timePad2 = n => String(n).padStart(2, '0');
function normalizeDailyTime(value, fallback) {
  const digits = String(value || '').replace(/\D/g, '').slice(0, 4);
  if (!digits) return fallback || null;
  const splitAt = digits.length === 3 ? 1 : 2;
  const hour = Number(digits.length <= 2 ? digits : digits.slice(0, splitAt));
  const minute = digits.length <= 2 ? 0 : Number(digits.slice(splitAt).padEnd(2, '0'));
  return hour <= 23 && minute <= 59 ? `${timePad2(hour)}:${timePad2(minute)}` : null;
}
function formatTimeDraft(digits) {
  return digits.length <= 2 ? digits : `${digits.slice(0, 2)}:${digits.slice(2)}`;
}
// A custom HH:MM picker avoids platform differences in native time inputs.
function setupTimeField(main, hooks) {
  const field = $('#claims-time-field', main);
  const input = $('#claims-time', main);
  const trigger = $('#claims-time-trigger', main);
  const pop = $('#claims-time-pop', main);
  if (typeof pop.showPopover === 'function') pop.setAttribute('popover', 'manual');
  for (const [unit, count] of [['hour', 24], ['minute', 60]]) {
    $(`.time-field-col[data-unit="${unit}"]`, pop).innerHTML = Array.from({ length: count }, (_, i) =>
      `<button type="button" class="time-option" data-value="${timePad2(i)}" aria-label="${i} ${unit === 'hour' ? '点' : '分'}">${timePad2(i)}</button>`).join('');
  }
  const isOpen = () => !pop.hidden;
  const highlight = () => {
    const current = normalizeDailyTime(input.value, hooks.fallback()) || hooks.fallback();
    const [hour, minute] = current.split(':');
    for (const col of $$('.time-field-col', pop)) {
      const target = col.dataset.unit === 'hour' ? hour : minute;
      for (const option of $$('.time-option', col)) {
        const selected = option.dataset.value === target;
        option.classList.toggle('selected', selected);
        option.setAttribute('aria-pressed', String(selected));
        option.tabIndex = selected ? 0 : -1;
      }
    }
  };
  function open() {
    if (trigger.disabled) return;
    openSelection?.close();
    closeHint();
    pop.hidden = false;
    if (pop.hasAttribute('popover')) pop.showPopover();
    else document.body.appendChild(pop);
    openSelection = { trigger, wrapper: field, list: pop, close };
    trigger.setAttribute('aria-expanded', 'true');
    highlight();
    positionFloatingPanel(pop, field, true);
    for (const option of $$('.time-option.selected', pop)) {
      const col = option.parentElement;
      col.scrollTop = option.offsetTop - (col.clientHeight - option.offsetHeight) / 2;
    }
  }
  function close() {
    if (pop.hasAttribute('popover') && pop.matches(':popover-open')) pop.hidePopover();
    pop.hidden = true;
    if (pop.parentElement !== field) field.appendChild(pop);
    trigger.setAttribute('aria-expanded', 'false');
    if (openSelection?.trigger === trigger) openSelection = null;
  }
  async function commit() {
    const time = normalizeDailyTime(input.value, hooks.fallback());
    if (!time) { input.value = hooks.fallback(); toast('时间须为 00:00–23:59', 'err'); return; }
    input.value = time;
    if (time === hooks.fallback() || hooks.busy()) return;
    try { await hooks.save(time); }
    catch { input.value = hooks.fallback(); }
    finally { highlight(); }
  }
  trigger.addEventListener('click', () => (isOpen() ? close() : open()));
  // Keep focus in the field while picking, so blur-commit never races the click.
  pop.addEventListener('mousedown', event => event.preventDefault());
  pop.addEventListener('click', event => {
    const option = event.target.closest('.time-option');
    if (!option) return;
    const [hour, minute] = (normalizeDailyTime(input.value, hooks.fallback()) || hooks.fallback()).split(':');
    input.value = option.closest('.time-field-col').dataset.unit === 'hour'
      ? `${option.dataset.value}:${minute}` : `${hour}:${option.dataset.value}`;
    close();
    trigger.focus();
    commit();
  });
  trigger.addEventListener('keydown', event => {
    if (event.key === 'Escape' && isOpen()) { event.preventDefault(); event.stopPropagation(); close(); }
    else if (event.key === 'ArrowDown') { event.preventDefault(); if (!isOpen()) open(); $('.time-option.selected', pop)?.focus(); }
  });
  pop.addEventListener('keydown', event => {
    if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); close(); trigger.focus(); return; }
    const option = event.target.closest('.time-option');
    if (!option) return;
    const col = option.parentElement, options = $$('.time-option', col), index = options.indexOf(option);
    let next;
    if (event.key === 'ArrowUp') next = options[Math.max(0, index - 1)];
    else if (event.key === 'ArrowDown') next = options[Math.min(options.length - 1, index + 1)];
    else if (event.key === 'Home') next = options[0];
    else if (event.key === 'End') next = options.at(-1);
    else if (event.key === 'ArrowRight') next = col.nextElementSibling?.querySelector('.selected');
    else if (event.key === 'ArrowLeft') next = col.previousElementSibling?.querySelector('.selected');
    if (next) { event.preventDefault(); next.focus(); }
  });
  input.addEventListener('input', () => {
    const draft = formatTimeDraft(String(input.value).replace(/\D/g, '').slice(0, 4));
    if (input.value !== draft) input.value = draft;
    if (isOpen()) highlight();
  });
  input.addEventListener('keydown', event => {
    if (event.key === 'Enter') { event.preventDefault(); commit(); }
    else if (event.key === 'Escape') { input.value = hooks.fallback(); close(); }
    else if (event.key === 'ArrowUp' || event.key === 'ArrowDown') {
      event.preventDefault();
      const [hour, minute] = (normalizeDailyTime(input.value, hooks.fallback()) || hooks.fallback()).split(':').map(Number);
      const total = ((hour * 60 + minute + (event.key === 'ArrowUp' ? 1 : -1)) % 1440 + 1440) % 1440;
      input.value = `${timePad2(Math.floor(total / 60))}:${timePad2(total % 60)}`;
      if (isOpen()) highlight();
    }
  });
  input.addEventListener('blur', () => { if (input.value !== hooks.fallback()) commit(); });
  return { highlight, close };
}

async function pageClaims(main) {
  let previews = [], accounts = [], accountsError = '', batchRunning = false, loading = false, page = 1;
  let schedule = null, scheduleSaving = false, scheduleError = '';
  let discoveryError = '', loadController;
  const pendingClaims = new Map(), claimQueue = [], runningAccounts = new Set();
  const claimKey = claimJobKey;
  const pageSize = 8;
  main.innerHTML = `${pagehead('领卡中心', '', '<button type="button" class="btn" id="claims-refresh">' + icon('refresh', 14) + '刷新</button>')}
    <section class="daily-claim-card fade-up" aria-labelledby="daily-claim-title">
      <header class="daily-claim-header"><div class="daily-claim-title"><span class="daily-claim-icon">${icon('gift', 22)}</span><div><h2 id="daily-claim-title">每日免费 Token</h2><span>每日 19:00 开放 · 北京时间</span></div></div><div class="daily-claim-actions"><div class="daily-claim-schedule"><span class="time-field control-compact" id="claims-time-field"><input id="claims-time" type="text" inputmode="numeric" autocomplete="off" spellcheck="false" maxlength="5" value="19:02" disabled aria-label="每日浏览器定时领取时间（北京时间）"><button type="button" class="time-field-trigger" id="claims-time-trigger" aria-label="选择定时领取时间" aria-expanded="false" aria-haspopup="dialog" aria-controls="claims-time-pop" disabled>${icon('timer', 13)}</button><span class="time-field-pop control-compact" id="claims-time-pop" hidden role="dialog" aria-label="选择定时领取时间"><span class="time-field-col" data-unit="hour" role="group" aria-label="小时"></span><span class="time-field-col" data-unit="minute" role="group" aria-label="分钟"></span></span></span><label for="claims-schedule">浏览器定时领取<input id="claims-schedule" type="checkbox" role="switch" disabled></label><span id="claims-schedule-info">${infoTip('定时领卡固定使用服务端真实浏览器，需在“设置 → 上游安全验证 → 领卡验证”配置浏览器依赖。服务需保持运行，网页可关闭。“领取”和“全部领取”使用手动领卡设置。', '定时领取')}</span></div><button type="button" class="btn primary" id="claims-all" disabled>全部领取</button></div></header>
      <div id="claims-content" aria-busy="true"><div class="skeleton-block"></div></div>
      <footer class="daily-claim-footer"><span id="claims-update" class="result-count" aria-live="polite">正在查询…</span><div id="claims-notice"></div><nav class="pager" id="claims-pages" aria-label="可领取账号分页" hidden></nav></footer>
    </section>`;
  const content = $('#claims-content', main);
  const token = TOKEN;
  const active = () => main.contains(content) && TOKEN === token && !$('#app-view').classList.contains('hidden');
  const visiblePreviews = () => {
    const records = claimVerificationRecords();
    // A historical failure is not evidence that a card is still claimable.
    return previews.map(preview => ({ ...preview, plans: (preview.plans || []).filter(plan => !records[claimKey({ account_id: preview.account_id, plan_id: plan.plan_id })]?.resolved) }));
  };
  const jobs = () => visiblePreviews().flatMap(preview => claimablePlans(preview).map(plan => ({ account_id: preview.account_id, plan_id: plan.plan_id })));
  const accountFor = id => accounts.find(account => account.id === id) || { label: previews.find(preview => preview.account_id === id)?.label };
  const syncBusy = () => {
    if (!active()) return;
    const busy = batchRunning || pendingClaims.size > 0 || claimBrowserVerifications.size > 0;
    const pendingAccounts = new Set([...pendingClaims.values()].map(entry => entry.job.account_id));
    for (const entry of claimBrowserVerifications.values()) pendingAccounts.add(entry.job.account_id);
    const total = jobs().length;
    const refreshButton = $('#claims-refresh', main);
    refreshButton.disabled = busy || loading || scheduleSaving;
    refreshButton.classList.toggle('loading', loading);
    refreshButton.setAttribute('aria-busy', String(loading));
    $('#claims-schedule', main).disabled = busy || loading || scheduleSaving || !schedule;
    $('#claims-time', main).disabled = busy || loading || scheduleSaving || !schedule;
    $('#claims-time-trigger', main).disabled = busy || loading || scheduleSaving || !schedule;
    const allButton = $('#claims-all', main);
    allButton.disabled = busy || loading || scheduleSaving || !total;
    allButton.textContent = claimBrowserVerifications.size ? '等待人工验证' : busy ? `正在领取（剩余 ${pendingClaims.size}）` : total ? `全部领取（${total}）` : '全部领取';
    $$('button[data-claim]', content).forEach(button => {
      const job = { account_id: button.dataset.claim, plan_id: button.dataset.plan }, key = claimKey(job);
      const entry = pendingClaims.get(key);
      button.disabled = loading || scheduleSaving || batchRunning || pendingAccounts.has(button.dataset.claim);
      button.textContent = claimBrowserVerifications.has(key) ? '等待验证…' : entry ? entry.running ? '正在领取…' : '等待领取…' : '领取';
      button.setAttribute('aria-busy', String(!!entry?.running || claimBrowserVerifications.has(key)));
    });
  };
  function paintSchedule() {
    if (!active()) return;
    $('#claims-schedule', main).checked = !!schedule?.enabled;
    const time = schedule?.time || '19:02';
    $('#claims-time', main).value = time;
    timeField.highlight();
    const details = [`每日北京时间 ${time} 固定使用服务端真实浏览器领取；服务需保持运行，网页可关闭。请在“设置 → 上游安全验证 → 领卡验证”配置浏览器依赖。`, '定时任务独立在后台执行，可重试的失败在计划时间起 10 分钟内恢复一次。点击“领取”或“全部领取”后，本次领取遇到风控会提示需要人工验证；再点击“人工领取”才打开验证码。验证后仍失败不自动重试。'];
    if (schedule?.browser_window) {
      const window = schedule.browser_window;
      const clock = value => new Date(value).toLocaleTimeString('zh-CN', { timeZone: 'Asia/Shanghai', hour12: false, hour: '2-digit', minute: '2-digit' });
      details.push(`浏览器窗口：${clock(window.start_at)}–${clock(window.end_at)}，任务完成后提前退出；手动领取选择浏览器时按需启动，空闲 ${window.idle_seconds} 秒退出。`);
    }
    if (scheduleError) details.push('设置读取失败：' + scheduleError);
    if (schedule?.next_run_at) details.push('下次执行：' + new Date(schedule.next_run_at).toLocaleString('zh-CN', { timeZone: 'Asia/Shanghai', hour12: false }) + '。');
    const last = schedule?.last_run;
    if (last?.started_at) {
      const date = new Date(last.finished_at || last.started_at).toLocaleString('zh-CN', { timeZone: 'Asia/Shanghai', hour12: false });
      const pending = last.pending_manual ?? (last.results || []).filter(result => result.requires_verification).length;
      const state = { running: '执行中', retry_wait: '等待恢复', completed: '已完成', partial: '部分成功', failed: '领取失败', needs_verification: '需人工验证', interrupted: '已中断' }[last.status] || last.status;
      details.push(`上次执行：${date}，${state}，已领取 ${last.claimed || 0} 张${last.failed ? `，失败 ${last.failed} 项` : ''}${pending ? `，${pending} 项需人工验证` : ''}。`);
      if (last.next_retry_at && schedule.enabled) details.push('恢复时间：' + new Date(last.next_retry_at).toLocaleString('zh-CN', { timeZone: 'Asia/Shanghai', hour12: false }) + '。');
      const reasons = [...new Set((last.results || []).filter(result => !result.ok && !result.no_cards).map(result => result.error || result.message).filter(Boolean))];
      if (reasons.length) details.push('未领取原因：' + reasons.join('；'));
    }
    $('#claims-schedule-info', main).innerHTML = infoTip(details.join('\n'), '定时领取');
  }
  function paint() {
    if (!active()) return;
    const ready = visiblePreviews().filter(preview => claimablePlans(preview).length);
    const failures = previews.filter(preview => preview.error);
    const pagination = UI.paginate(ready, page, pageSize), { items, pages } = pagination;
    page = pagination.page;
    content.innerHTML = items.length ? items.map(preview => claimAccountMarkup(preview, accountFor(preview.account_id))).join('')
      : loading ? '<div class="skeleton-block"></div>'
      : '<div class="daily-claim-empty">' + icon('gift', 28) + `<p${discoveryError || failures.length ? ' role="alert"' : ''}>${discoveryError || failures.length ? '查询未完成，请刷新重试' : '暂无可领取的每日免费卡'}</p></div>`;
    const notices = [];
    const verificationCount = ready.reduce((count, preview) => count + claimablePlans(preview).filter(plan => pendingClaimVerification({ account_id: preview.account_id, plan_id: plan.plan_id })).length, 0);
    if (verificationCount) notices.push(`<span>${verificationCount} 张待人工验证</span>`);
    if (failures.length) notices.push(`<span class="warn-text">${failures.length} 个账号未更新</span>${infoTip(failures.map(preview => `${accountFor(preview.account_id).platform_name || preview.label || '账号'}：${preview.error}${preview.stale ? '（保留上次有效结果）' : ''}`).join('\n'), '账号查询失败')}`);
    if (discoveryError) notices.push(infoTip(discoveryError, '查询失败'));
    if (accountsError) notices.push(infoTip('官方账号信息暂不可用：' + accountsError, '账号信息'));
    $('#claims-notice', main).innerHTML = notices.join('');
    const pager = $('#claims-pages', main);
    pager.hidden = pages === 1;
    pager.innerHTML = pages > 1 ? `<button type="button" data-claim-page="-1" aria-label="上一页可领取账号" ${page === 1 ? 'disabled' : ''}>‹</button><span>${page} / ${pages}</span><button type="button" data-claim-page="1" aria-label="下一页可领取账号" ${page === pages ? 'disabled' : ''}>›</button>` : '';
    syncBusy();
  }
  async function load() {
    if (!active() || loading || batchRunning || pendingClaims.size || claimBrowserVerifications.size || scheduleSaving) return;
    loadController?.abort();
    const controller = new AbortController();
    loadController = controller;
    const current = () => active() && loadController === controller && !controller.signal.aborted;
    const request = async (path, timeout = 12000) => {
      const pending = new AbortController();
      const abort = () => pending.abort();
      controller.signal.addEventListener('abort', abort, { once: true });
      const timer = setTimeout(abort, timeout);
      try {
        return await api(path, { cache: 'no-store', signal: pending.signal });
      } finally {
        clearTimeout(timer);
        controller.signal.removeEventListener('abort', abort);
      }
    };
    const errorMessage = error => ['AbortError', 'TimeoutError'].includes(error.name) ? '查询超时，请刷新重试' : error.message;
    const validPreviousPlans = preview => claimablePlans(preview).filter(plan => Number(plan.ends_at) * 1000 > Date.now());
    discoveryError = '';
    loading = true; syncBusy(); content.setAttribute('aria-busy', 'true');
    $('#claims-update', main).textContent = '正在查询…';
    // Names and schedule settings enhance the page independently; neither
    // request may hold back a completed card query or keep refresh disabled.
    void request('/api/accounts').then(result => {
      if (!current()) return;
      if (!Array.isArray(result)) throw new Error('账号信息返回格式无效');
      accounts = result; accountsError = ''; paint();
    }).catch(error => {
      if (!current()) return;
      accountsError = errorMessage(error); paint();
    });
    void request('/api/claims/schedule').then(result => {
      if (!current() || scheduleSaving) return;
      schedule = result; scheduleError = ''; paintSchedule(); syncBusy();
    }).catch(error => {
      if (!current()) return;
      scheduleError = errorMessage(error); paintSchedule(); syncBusy();
    });
    try {
      const discovery = await request('/api/claims/discover?scope=daily', 35000);
      if (!current()) return;
      if (!Array.isArray(discovery.previews)) throw new Error('查询结果不完整，请刷新重试');
      const previous = new Map(previews.map(preview => [preview.account_id, preview]));
      previews = discovery.previews.map(preview => {
        // New servers already reconcile cached candidates against ownership.
        // Only older/error responses without that check need a local fallback.
        if (!preview.error || preview.stale !== undefined || !previous.has(preview.account_id)) return preview;
        const plans = validPreviousPlans(previous.get(preview.account_id));
        return { ...preview, plans, stale: plans.length > 0 };
      });
      const failed = previews.filter(preview => preview.error).length;
      const time = new Date(discovery.checked_at || Date.now()).toLocaleTimeString('zh-CN', { hour12: false });
      $('#claims-update', main).textContent = failed && failed === previews.length ? '查询未完成' : `${failed ? '部分更新' : '更新于'} ${time}`;
    } catch (error) {
      if (current()) {
        discoveryError = errorMessage(error);
        previews = previews.map(preview => {
          const plans = validPreviousPlans(preview);
          return { ...preview, plans, error: discoveryError, stale: plans.length > 0 };
        });
        $('#claims-update', main).textContent = jobs().length ? '未更新 · 保留上次结果' : '查询未完成';
      }
    } finally {
      if (loadController === controller) {
        loading = false;
        if (active()) { content.setAttribute('aria-busy', 'false'); paint(); }
      }
    }
  }
  function enqueueClaim(job) {
    if (!active() || loading || scheduleSaving) return Promise.resolve(null);
    const key = claimKey(job);
    if (pendingClaims.has(key)) return pendingClaims.get(key).promise;
    const entry = { job, key, running: false };
    entry.promise = new Promise((resolve, reject) => { entry.resolve = resolve; entry.reject = reject; });
    pendingClaims.set(key, entry);
    claimQueue.push(entry);
    pumpClaims();
    return entry.promise;
  }
  function pumpClaims() {
    if (!active()) {
      for (const entry of claimQueue.splice(0)) {
        pendingClaims.delete(entry.key);
        entry.resolve(null);
      }
      return;
    }
    while (runningAccounts.size < manualClaimConcurrency) {
      const index = claimQueue.findIndex(entry => !runningAccounts.has(entry.job.account_id));
      if (index < 0) break;
      const [entry] = claimQueue.splice(index, 1);
      entry.running = true;
      runningAccounts.add(entry.job.account_id);
      void runClaim(entry);
    }
    syncBusy();
  }
  async function runClaim(entry) {
    try {
      const result = await submitClaim(entry.job, accountFor(entry.job.account_id).platform_name || '当前账号', active);
      if (result?.ok) removeClaimed(entry.job);
      if (!result || (!result.ok && (result.requires_verification || result.retry_after_ms > 0 || ['auth', 'account_risk', 'network', 'rate_limit'].includes(result.failure_kind)))) {
        cancelAccountQueue(entry.job.account_id);
      }
      entry.resolve(result);
    } catch (error) {
      cancelAccountQueue(entry.job.account_id);
      entry.reject(error);
    }
    finally {
      runningAccounts.delete(entry.job.account_id);
      pendingClaims.delete(entry.key);
      pumpClaims();
    }
  }
  function cancelAccountQueue(accountID) {
    for (let index = claimQueue.length - 1; index >= 0; index--) {
      if (claimQueue[index].job.account_id !== accountID) continue;
      const [entry] = claimQueue.splice(index, 1);
      pendingClaims.delete(entry.key);
      entry.resolve(null);
    }
  }
  function removeClaimed(job) {
    const preview = previews.find(item => item.account_id === job.account_id);
    if (preview) preview.plans = (preview.plans || []).filter(plan => plan.plan_id !== job.plan_id);
    if (active()) paint();
  }
  const resultMessage = claimResultMessage;
  $('#claims-refresh', main).addEventListener('click', load);
  $('#claims-schedule', main).addEventListener('change', async event => {
    if (!schedule || scheduleSaving || batchRunning || pendingClaims.size) return;
    const enabled = event.target.checked;
    scheduleSaving = true; syncBusy();
    try {
      const result = await api('/api/settings', { method: 'PUT', body: { daily_claim_enabled: enabled } });
      schedule = result.claim_schedule;
      scheduleError = '';
      toast(enabled ? `已开启每日 ${schedule.time} 浏览器定时领取` : '已关闭定时领取', 'ok');
    } catch (error) { toast(error.message, 'err'); }
    finally { scheduleSaving = false; paintSchedule(); syncBusy(); }
  });
  const timeField = setupTimeField(main, {
    fallback: () => schedule?.time || '19:02',
    busy: () => scheduleSaving || batchRunning || pendingClaims.size > 0,
    save: async time => {
      scheduleSaving = true; syncBusy();
      try {
        const result = await api('/api/settings', { method: 'PUT', body: { daily_claim_time: time } });
        schedule = result.claim_schedule;
        scheduleError = '';
        toast(`浏览器定时领取已改为每日 ${time}`, 'ok');
      } catch (error) { toast(error.message, 'err'); throw error; }
      finally { scheduleSaving = false; paintSchedule(); syncBusy(); }
    },
  });
  $('#claims-pages', main).addEventListener('click', event => {
    const button = event.target.closest('[data-claim-page]');
    if (!button || button.disabled) return;
    page += Number(button.dataset.claimPage); paint();
    $('#claims-pages button:not(:disabled)', main)?.focus({ preventScroll: true });
  });
  content.addEventListener('click', event => {
    const accountLink = event.target.closest('[data-act="claim-account"]');
    if (accountLink) { viewAccount(accountLink.dataset.id); return; }
    const button = event.target.closest('[data-claim]');
    if (!button || button.disabled) return;
    const job = { account_id: button.dataset.claim, plan_id: button.dataset.plan };
    void enqueueClaim(job).then(result => {
      if (!active() || !result) return;
      toast(result.ok ? '每日免费卡已领取' : resultMessage(result), result.ok ? 'ok' : 'err');
    }).catch(error => { if (active()) toast(error.message, 'err'); });
  });
  $('#claims-all', main).addEventListener('click', async () => {
    if (batchRunning || pendingClaims.size || loading || scheduleSaving) return;
    const pending = jobs();
    batchRunning = true; syncBusy();
    try {
      const outcomes = await Promise.allSettled(pending.map(enqueueClaim));
      if (!active()) return;
      let claimed = 0, failed = 0, canceled = 0, verification = 0;
      for (const [index, outcome] of outcomes.entries()) {
        if (outcome.status === 'fulfilled' && !outcome.value) {
          if (pendingClaimVerification(pending[index])) verification++; else canceled++;
          continue;
        }
        if (outcome.status === 'fulfilled' && outcome.value.ok) { claimed++; continue; }
        if (outcome.status === 'fulfilled' && claimNeedsVerification(outcome.value)) { verification++; continue; }
        failed++;
        const message = outcome.status === 'rejected' ? outcome.reason.message : resultMessage(outcome.value);
        toast(`${accountFor(pending[index].account_id).platform_name || '账号'}：${message}`, 'err');
      }
      toast(`已领取 ${claimed} 张${verification ? `，${verification} 张待人工验证` : ''}${failed ? `，${failed} 张失败` : ''}${canceled ? `，${canceled} 张未继续` : ''}`, failed ? 'err' : verification ? '' : 'ok');
    } finally {
      batchRunning = false; syncBusy();
    }
  });
  const onVerificationChange = event => {
    if (event.detail?.resolved) removeClaimed(event.detail.job);
    else if (event.detail?.result) paint();
    else syncBusy();
  };
  window.addEventListener('claim-verification-change', onVerificationChange);
  const removalObserver = new MutationObserver(() => {
    if (active()) return;
    loadController?.abort();
    window.removeEventListener('claim-verification-change', onVerificationChange);
    removalObserver.disconnect();
  });
  removalObserver.observe(main, { childList: true, subtree: true });
  await load();
}
