// 动作后等页面稳定。
//
// 顺序参照 chrome-devtools-mcp 的 WaitForHelper 与 playwright-mcp 的 waitForCompletion：
// 动作发出后稍等，看主框架有没有发起导航；有就等新文档提交和 domcontentloaded；
// 再等动作期间发出的 xhr/fetch 回来；最后等 DOM 静默一小段。
// 整体有上限，到点就返回并标 busy，不算失败。

const SETTLE_MAX_MS = 3000;
const NAV_GRACE_MS = 100;
const QUIET_MS = 200;
const POLL_MS = 50;
const TRACKED_TYPES = new Set(['xhr', 'fetch']);

const COUNTER_TIMEOUT_MS = 1000;

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
const leftUntil = (deadline) => Math.max(0, deadline - Date.now());

// 页面停在对话框上时 evaluate 会一直挂着，计数器拿不到就当不知道。
function orNull(promise, ms) {
  let timer;
  return Promise.race([promise, new Promise((resolve) => (timer = setTimeout(() => resolve(null), ms)))])
    .catch(() => null)
    .finally(() => clearTimeout(timer));
}

// 样式属性变化多来自动画，不算页面变化。
function installMutationCounter() {
  const box = { count: 0 };
  const observer = new MutationObserver((records) => {
    for (const record of records) {
      if (!(record.type === 'attributes' && record.attributeName === 'style')) {
        box.count += 1;
      }
    }
  });
  observer.observe(document, { subtree: true, childList: true, attributes: true, characterData: true });
  box.stop = () => {
    observer.disconnect();
    return box.count;
  };
  return box;
}

function waitQuietInPage({ quietMs, maxMs }) {
  return new Promise((resolve) => {
    let quietTimer = 0;
    let capTimer = 0;
    let observer = null;
    const finish = (quiet) => {
      if (observer) observer.disconnect();
      clearTimeout(quietTimer);
      clearTimeout(capTimer);
      resolve(quiet);
    };
    const arm = () => {
      clearTimeout(quietTimer);
      quietTimer = setTimeout(() => finish(true), quietMs);
    };
    observer = new MutationObserver((records) => {
      if (records.some((record) => !(record.type === 'attributes' && record.attributeName === 'style'))) {
        arm();
      }
    });
    observer.observe(document, { subtree: true, childList: true, attributes: true, characterData: true });
    capTimer = setTimeout(() => finish(false), maxMs);
    arm();
  });
}

async function until(predicate, deadline) {
  while (!predicate()) {
    if (Date.now() >= deadline) return false;
    await sleep(POLL_MS);
  }
  return true;
}

// 同样自己限时，理由见 orNull。
async function waitQuietDom(page, deadline) {
  while (!page.isClosed()) {
    const left = leftUntil(deadline);
    if (left <= 0) return false;
    let timer;
    try {
      return await Promise.race([
        page.evaluate(waitQuietInPage, { quietMs: QUIET_MS, maxMs: left }),
        new Promise((resolve) => {
          timer = setTimeout(() => resolve(false), left + 500);
        }),
      ]);
    } catch {
      // 执行上下文被导航销毁：等新文档就绪后接着等静默。
      await page.waitForLoadState('domcontentloaded', { timeout: Math.max(1, leftUntil(deadline)) }).catch(() => {});
    } finally {
      clearTimeout(timer);
    }
  }
  return true;
}

function watchPage(page) {
  const main = page.mainFrame();
  const watch = {
    navRequest: null,
    navDone: false,
    navCommitted: false,
    committed: false,
    navCount: 0,
    inflight: new Set(),
    popups: [],
  };
  const onRequest = (request) => {
    let frame = null;
    try {
      frame = request.frame();
    } catch {
      /* service worker 发出的请求没有 frame */
    }
    if (frame === main && request.isNavigationRequest()) {
      watch.navRequest = request;
      watch.navDone = false;
      watch.navCommitted = false;
      watch.navCount += 1;
    } else if (TRACKED_TYPES.has(request.resourceType())) {
      watch.inflight.add(request);
    }
  };
  const onRequestEnd = (request) => {
    watch.inflight.delete(request);
    if (request === watch.navRequest) watch.navDone = true;
  };
  const onFrameNavigated = (frame) => {
    if (frame !== main) return;
    watch.committed = true;
    if (watch.navRequest) watch.navCommitted = true;
  };
  const onPopup = (popup) => watch.popups.push(popup);

  page.on('request', onRequest);
  page.on('requestfinished', onRequestEnd);
  page.on('requestfailed', onRequestEnd);
  page.on('framenavigated', onFrameNavigated);
  page.on('popup', onPopup);
  watch.dispose = () => {
    page.off('request', onRequest);
    page.off('requestfinished', onRequestEnd);
    page.off('requestfailed', onRequestEnd);
    page.off('framenavigated', onFrameNavigated);
    page.off('popup', onPopup);
  };
  return watch;
}

// 执行 action 并等页面稳定。返回 { value, effect }：
//   effect.navigated  主框架换了文档或 URL 变了
//   effect.changed    DOM 有变动（读不到计数时省略）
//   effect.busy       到上限仍在加载或变化
//   effect.newTabs    动作新开的标签页
async function settleAfter(page, action, options) {
  const maxMs = Number(options && options.maxMs) > 0 ? Number(options.maxMs) : SETTLE_MAX_MS;
  const startURL = page.url();
  const watch = watchPage(page);
  const pendingCounter = page.evaluateHandle(installMutationCounter);
  const counter = await orNull(pendingCounter, COUNTER_TIMEOUT_MS);
  if (!counter) {
    // 超时后才装上的计数器要拆掉，否则观察器会一直挂在页面里。
    pendingCounter
      .then((late) => late.evaluate((box) => box.stop()).finally(() => late.dispose()))
      .catch(() => {});
  }

  let value;
  try {
    value = await action();
  } catch (error) {
    watch.dispose();
    if (counter) counter.dispose().catch(() => {});
    throw error;
  }

  const deadline = Date.now() + maxMs;
  let quiet = false;
  try {
    await sleep(NAV_GRACE_MS);
    // 只在又起了新导航时再来一轮（典型：fetch 登录成功后脚本跳转）。
    // 轮询类请求不在这里重来，否则吵闹的页面每次都要等满上限。
    for (;;) {
      const seen = watch.navCount;
      if (watch.navRequest && !watch.navCommitted) {
        await until(() => watch.navCommitted || watch.navDone, deadline);
      }
      if (watch.navCommitted) {
        await page.waitForLoadState('domcontentloaded', { timeout: Math.max(1, leftUntil(deadline)) }).catch(() => {});
      }
      await until(() => watch.inflight.size === 0, deadline);
      quiet = await waitQuietDom(page, deadline);
      if (watch.navCount === seen || Date.now() >= deadline) break;
    }
  } finally {
    watch.dispose();
  }

  let mutations = null;
  if (counter) {
    mutations = await orNull(
      counter.evaluate((box) => box.stop()),
      COUNTER_TIMEOUT_MS
    );
    counter.dispose().catch(() => {});
  }

  const navigated = watch.committed || page.url() !== startURL;
  const effect = { navigated };
  if (navigated) {
    effect.changed = true;
  } else if (typeof mutations === 'number') {
    effect.changed = mutations > 0;
  }
  const pendingNav = watch.navRequest && !watch.navCommitted && !watch.navDone;
  if (!quiet || watch.inflight.size > 0 || pendingNav) {
    effect.busy = true;
  }
  const newTabs = watch.popups.filter((popup) => !popup.isClosed()).map((popup) => popup.url());
  if (newTabs.length) {
    effect.newTabs = newTabs;
  }
  return { value, effect };
}

module.exports = {
  settleAfter,
  SETTLE_MAX_MS,
};
