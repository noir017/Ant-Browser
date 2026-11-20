// 紧凑快照与元素编号。
//
// 快照每个可交互元素一行：`e12 button "Submit"`。编号在会话内递增、不复用，
// 同一个元素再次出现时沿用旧编号。
//
// 编号表放在页面里一个不挂到 window 的对象上（evaluateHandle 返回的句柄持有它），
// 页面脚本拿不到。文档被替换时句柄随执行上下文一起失效，据此区分
// 「页面已跳转」和「元素被移除」；元素被框架重新渲染替换时，若它在快照里
// role+name 唯一、现在也唯一，就改绑到新元素上。

const DEFAULT_LIMIT = 200;
const MAX_LIMIT = 1000;
const MAX_FRAMES = 10;
const FRAME_TIMEOUT_MS = 3000;
const REF_PATTERN = /^(?:@|ref=)?(e\d+)$/;

function parseRef(value) {
  const match = REF_PATTERN.exec(String(value || '').trim());
  return match ? match[1] : '';
}

// 「a b|c」= (a 且 b) 或 c，不分大小写。
function parseQuery(query) {
  return String(query || '')
    .toLowerCase()
    .split('|')
    .map((alt) => alt.split(/\s+/).filter(Boolean))
    .filter((words) => words.length > 0);
}

function matchesQuery(text, alts) {
  if (!alts.length) {
    return true;
  }
  const lower = String(text).toLowerCase();
  return alts.some((words) => words.every((word) => lower.includes(word)));
}

function clampLimit(value) {
  const limit = Number(value);
  if (!Number.isFinite(limit) || limit <= 0) {
    return DEFAULT_LIMIT;
  }
  return Math.min(Math.round(limit), MAX_LIMIT);
}

function withTimeout(promise, ms) {
  let timer;
  return Promise.race([
    promise,
    new Promise((_, reject) => {
      timer = setTimeout(() => reject(new Error(`timed out after ${ms}ms`)), ms);
    }),
  ]).finally(() => clearTimeout(timer));
}

function shortFrameURL(url) {
  try {
    const parsed = new URL(url);
    return `${parsed.host}${parsed.pathname}`.slice(0, 80);
  } catch {
    return String(url || '').slice(0, 80);
  }
}

// 页面内执行，必须自包含。返回的对象只通过句柄访问。
function installRegistry() {
  const byRef = new Map();
  const byEl = new WeakMap();

  const CONTROL = [
    'a',
    'button',
    'input:not([type=hidden])',
    'select',
    'textarea',
    'summary',
    '[contenteditable=""]',
    '[contenteditable=true]',
    '[onclick]',
    '[role=button]',
    '[role=link]',
    '[role=checkbox]',
    '[role=radio]',
    '[role=switch]',
    '[role=tab]',
    '[role=menuitem]',
    '[role=menuitemcheckbox]',
    '[role=menuitemradio]',
    '[role=option]',
    '[role=combobox]',
    '[role=textbox]',
    '[role=searchbox]',
    '[role=slider]',
    '[role=spinbutton]',
    '[role=treeitem]',
  ].join(',');
  const MAX_WALK = 30000;
  const NAME_MAX = 80;
  const VALUE_MAX = 40;
  const INPUT_ROLE = {
    button: 'button',
    submit: 'button',
    reset: 'button',
    image: 'button',
    checkbox: 'checkbox',
    radio: 'radio',
    range: 'slider',
    number: 'spinbutton',
    search: 'searchbox',
    file: 'file',
  };
  const NO_VALUE_TYPES = new Set(['button', 'submit', 'reset', 'image', 'checkbox', 'radio', 'file']);
  const TYPE_HINT = new Set(['password', 'email', 'tel', 'url', 'date', 'datetime-local', 'month', 'week', 'time', 'color']);

  const clean = (value) => String(value == null ? '' : value).replace(/\s+/g, ' ').trim();
  const clip = (value, max) => (value.length > max ? `${value.slice(0, max - 1)}…` : value);
  const inputType = (el) => (el.localName === 'input' ? String(el.type || 'text').toLowerCase() : '');
  const isToggle = (el) => {
    const type = inputType(el);
    return type === 'checkbox' || type === 'radio';
  };

  // 自定义复选框常把 input 设成 opacity:0 叠在样式元素下，仍然可点，不按透明度过滤。
  const isVisible = (el, ignoreOpacity) => {
    const rect = el.getBoundingClientRect();
    if (rect.width <= 0 || rect.height <= 0) {
      return false;
    }
    if (typeof el.checkVisibility === 'function') {
      // 后两个是 Chrome 121 之前的参数名。
      const checkOpacity = !ignoreOpacity;
      return el.checkVisibility({
        opacityProperty: checkOpacity,
        visibilityProperty: true,
        checkOpacity,
        checkVisibilityCSS: true,
      });
    }
    const style = getComputedStyle(el);
    return style.visibility !== 'hidden' && style.display !== 'none' && (ignoreOpacity || style.opacity !== '0');
  };

  const roleOf = (el) => {
    const explicit = clean(el.getAttribute('role')).split(' ')[0];
    if (explicit && explicit !== 'presentation' && explicit !== 'none' && explicit !== 'generic') {
      return explicit;
    }
    const tag = el.localName;
    if (tag === 'a') return 'link';
    if (tag === 'button' || tag === 'summary') return 'button';
    if (tag === 'select') return el.multiple || el.size > 1 ? 'listbox' : 'combobox';
    if (tag === 'textarea') return 'textbox';
    if (tag === 'input') return INPUT_ROLE[inputType(el)] || 'textbox';
    if (el.isContentEditable) return 'textbox';
    return 'clickable';
  };

  const nameOf = (el) => {
    const doc = el.ownerDocument;
    const sources = [];
    const labelledBy = clean(el.getAttribute('aria-labelledby'));
    if (labelledBy) {
      sources.push(
        labelledBy
          .split(' ')
          .map((id) => {
            const node = doc.getElementById(id);
            return node ? node.innerText || node.textContent : '';
          })
          .join(' ')
      );
    }
    sources.push(el.getAttribute('aria-label'));
    if (el.labels && el.labels.length) {
      sources.push(Array.from(el.labels, (label) => label.innerText || label.textContent).join(' '));
    }
    const tag = el.localName;
    const type = inputType(el);
    if (type === 'button' || type === 'submit' || type === 'reset') sources.push(el.value);
    if (type === 'image') sources.push(el.getAttribute('alt'));
    if (tag !== 'input' && tag !== 'select' && tag !== 'textarea') {
      sources.push(el.innerText === undefined ? el.textContent : el.innerText);
      // 图标按钮：名字常在子元素的 alt / aria-label / title 上。
      const icon = el.querySelector('img[alt],[aria-label],[title],svg title');
      if (icon) {
        sources.push(icon.getAttribute('alt') || icon.getAttribute('aria-label') || icon.getAttribute('title') || icon.textContent);
      }
    }
    sources.push(el.getAttribute('placeholder'), el.getAttribute('title'));
    if (type === 'submit') sources.push('Submit');
    sources.push(el.getAttribute('name'));
    if (tag === 'input' || tag === 'select' || tag === 'textarea') sources.push(el.id);
    for (const source of sources) {
      const text = clean(source);
      if (text) return clip(text, NAME_MAX);
    }
    return '';
  };

  const shortHref = (a) => {
    const raw = clean(a.getAttribute('href'));
    if (!raw || /^javascript:/i.test(raw)) return '';
    if (raw.startsWith('#')) return clip(raw, 80);
    let url;
    try {
      url = new URL(a.href, location.href);
    } catch {
      return '';
    }
    if (url.protocol !== 'http:' && url.protocol !== 'https:') return clip(raw, 80);
    const text =
      url.origin === location.origin
        ? `${url.pathname}${url.search}${url.hash}`
        : `${url.host}${url.pathname === '/' ? '' : url.pathname}${url.search}`;
    return clip(text, 80);
  };

  // el 是动作目标，control 提供状态。两者只在「隐藏的复选框由 label 代点」时不同。
  const describe = ({ el, control }) => {
    const role = roleOf(control);
    const name = nameOf(control) || (el !== control ? clip(clean(el.innerText), NAME_MAX) : '');
    const parts = [role];
    if (name) parts.push(JSON.stringify(name));
    const tag = control.localName;
    const type = inputType(control);
    if (TYPE_HINT.has(type)) parts.push(`type=${type}`);
    if (isToggle(control) ? control.checked : control.getAttribute('aria-checked') === 'true') parts.push('[checked]');
    if (control.getAttribute('aria-pressed') === 'true') parts.push('[pressed]');
    if (control.getAttribute('aria-selected') === 'true') parts.push('[selected]');
    if (control.getAttribute('aria-expanded') === 'true') parts.push('[expanded]');
    if (control.disabled || control.getAttribute('aria-disabled') === 'true') parts.push('[disabled]');
    if (tag === 'select') {
      const options = Array.from(control.options || []);
      const selected = Array.from(control.selectedOptions || [], (option) => clean(option.label || option.text)).join(', ');
      if (selected) parts.push(`value=${JSON.stringify(clip(selected, VALUE_MAX))}`);
      parts.push(
        options.length <= 10
          ? `options=${JSON.stringify(options.map((option) => clean(option.label || option.text)).join('|'))}`
          : `options=${options.length}`
      );
    } else if ((tag === 'input' && !NO_VALUE_TYPES.has(type)) || tag === 'textarea') {
      const value = String(control.value || '');
      if (value) parts.push(type === 'password' ? 'value=***' : `value=${JSON.stringify(clip(clean(value), VALUE_MAX))}`);
    }
    if (tag === 'a') {
      const href = shortHref(control);
      if (href) parts.push(`href=${href}`);
    }
    return { role, name, text: parts.join(' ') };
  };

  // cursor:pointer 的最外层元素：框架常用 div 绑点击事件，DOM 上看不出来。
  const isPointerRoot = (node) => {
    if (getComputedStyle(node).cursor !== 'pointer') return false;
    const parent = node.parentElement || (node.parentNode && node.parentNode.host);
    if (parent && getComputedStyle(parent).cursor === 'pointer') return false;
    if ((node.parentElement && node.parentElement.closest(CONTROL)) || node.querySelector(CONTROL)) return false;
    return isVisible(node, false) && nameOf(node) !== '';
  };

  const candidates = () => {
    const out = [];
    const seen = new Set();
    let walked = 0;
    const push = (el, control) => {
      if (!seen.has(el)) {
        seen.add(el);
        out.push({ el, control });
      }
    };
    const visit = (root) => {
      const walker = document.createTreeWalker(root, NodeFilter.SHOW_ELEMENT);
      for (let node = walker.nextNode(); node; node = walker.nextNode()) {
        walked += 1;
        if (walked > MAX_WALK) return;
        if (node.matches(CONTROL)) {
          if (isVisible(node, isToggle(node))) {
            push(node, node);
          } else if (isToggle(node)) {
            const label = Array.from(node.labels || []).find((item) => isVisible(item, false));
            if (label) push(label, node);
          }
        } else if (isPointerRoot(node)) {
          push(node, node);
        }
        if (node.shadowRoot) visit(node.shadowRoot);
      }
    };
    visit(document.body || document.documentElement);
    return out;
  };

  const keyOf = (item) => `${item.role}\n${item.name}`;

  return {
    ok: true,

    collect({ limit, alts, next }) {
      const items = candidates().map((candidate) => ({ ...candidate, ...describe(candidate) }));
      const counts = new Map();
      for (const item of items) {
        counts.set(keyOf(item), (counts.get(keyOf(item)) || 0) + 1);
      }
      const matches = (text) => {
        if (!alts.length) return true;
        const lower = text.toLowerCase();
        return alts.some((words) => words.every((word) => lower.includes(word)));
      };

      const lines = [];
      const issued = [];
      let total = 0;
      let counter = next;
      for (const item of items) {
        if (!matches(item.text)) continue;
        total += 1;
        if (lines.length >= limit) continue;
        let ref = byEl.get(item.el);
        if (!ref) {
          ref = `e${counter}`;
          counter += 1;
          byEl.set(item.el, ref);
          issued.push(ref);
        }
        byRef.set(ref, { el: new WeakRef(item.el), key: keyOf(item), unique: counts.get(keyOf(item)) === 1 });
        lines.push(`${ref} ${item.text}`);
      }
      return { lines, issued, total, next: counter };
    },

    resolve(ref) {
      const entry = byRef.get(ref);
      if (!entry) return 'unknown';
      const el = entry.el.deref();
      if (el && el.isConnected) return el;
      if (entry.unique) {
        const hits = candidates().filter((candidate) => keyOf(describe(candidate)) === entry.key);
        if (hits.length === 1) {
          entry.el = new WeakRef(hits[0].el);
          byEl.set(hits[0].el, ref);
          return hits[0].el;
        }
      }
      return 'gone';
    },
  };
}

function refNotFound(ref, why) {
  return new Error(`ref not found: ${ref} (${why}; take a new snapshot)`);
}

// 每个页面会话一个。编号表按 frame 存句柄，ref → 句柄记录它属于哪份文档。
function createRefStore() {
  let nextRef = 1;
  const registries = new WeakMap();
  const owners = new Map();
  let held = [];

  const registryFor = async (frame) => {
    const cached = registries.get(frame);
    if (cached) return cached;
    const handle = await frame.evaluateHandle(installRegistry);
    registries.set(frame, handle);
    return handle;
  };

  // 句柄失效（文档已替换）时换一份新编号表重试一次。
  const collectIn = async (frame, options) => {
    for (let attempt = 0; ; attempt += 1) {
      const registry = await registryFor(frame);
      try {
        const result = await registry.evaluate((reg, args) => reg.collect(args), { ...options, next: nextRef });
        nextRef = result.next;
        for (const ref of result.issued) owners.set(ref, registry);
        return result;
      } catch (error) {
        registries.delete(frame);
        if (attempt >= 1) throw error;
      }
    }
  };

  return {
    async snapshot(page, options) {
      const limit = clampLimit(options && options.limit);
      const query = String((options && options.query) || '').trim();
      const alts = parseQuery(query);
      const main = page.mainFrame();
      const frames = [main, ...page.frames().filter((frame) => frame !== main && !frame.isDetached()).slice(0, MAX_FRAMES)];

      const lines = [];
      let shown = 0;
      let total = 0;
      for (const frame of frames) {
        let result;
        if (frame === main) {
          result = await collectIn(frame, { limit: limit - shown, alts });
        } else {
          // 跨域广告 iframe 可能卡住，单独限时，失败就跳过。
          result = await withTimeout(collectIn(frame, { limit: limit - shown, alts }), FRAME_TIMEOUT_MS).catch(() => null);
          if (!result) continue;
          if (result.lines.length) lines.push(`-- iframe ${shortFrameURL(frame.url())}`);
        }
        total += result.total;
        shown += result.lines.length;
        lines.push(...result.lines);
      }

      if (total === 0) {
        lines.push(query ? `(no element matches ${JSON.stringify(query)})` : '(no interactive elements)');
      } else if (total > shown) {
        lines.push(`(${total - shown} more not shown; narrow with query or raise limit)`);
      }
      return { text: lines.join('\n'), shown, total };
    },

    async resolve(ref) {
      const registry = owners.get(ref);
      if (!registry) {
        throw refNotFound(ref, 'unknown ref');
      }
      let handle;
      try {
        handle = await registry.evaluateHandle((reg, target) => reg.resolve(target), ref);
      } catch {
        throw refNotFound(ref, 'page navigated or reloaded');
      }
      const element = handle.asElement();
      if (!element) {
        await handle.dispose().catch(() => {});
        throw refNotFound(ref, 'element was removed');
      }
      held.push(element);
      return element;
    },

    // 每条指令结束后释放本次解析出的元素句柄，免得页面里的对象一直被钉住。
    async release() {
      const handles = held;
      held = [];
      await Promise.all(handles.map((handle) => handle.dispose().catch(() => {})));
    },
  };
}

// includeText 的正文：去掉空行，按 query 过滤，超长截断并注明。
function compactText(raw, options) {
  const alts = parseQuery(options && options.query);
  const limit = Number(options && options.textLimit) > 0 ? Number(options.textLimit) : 5000;
  const text = String(raw || '')
    .split('\n')
    .map((line) => line.replace(/\s+/g, ' ').trim())
    .filter((line) => line && matchesQuery(line, alts))
    .join('\n');
  if (text.length <= limit) {
    return text;
  }
  return `${text.slice(0, limit)}\n(text truncated, ${text.length - limit} more chars)`;
}

module.exports = {
  parseRef,
  createRefStore,
  compactText,
  installRegistry,
};
