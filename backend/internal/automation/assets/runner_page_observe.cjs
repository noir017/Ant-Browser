// 页面观测：网络请求与控制台消息的只读采集。
//
// 给会话里的每个 page（含之后新开的标签页）装上监听，用环形缓冲留住最近
// 若干条记录。agent 想查「刚才那次点击为什么没成」时直接读这两个动作，
// 不必再手写 fetch 钩子、翻 performance 或者自己重发一遍请求。
//
// 采集全在 Node 侧完成，Go 只负责翻译参数、解码结果。请求头、请求体、响应头
// 和 URL 查询串里的凭据一律打码后再回传，避免 token 顺着工具输出流进上下文。

const NETWORK_BUFFER_LIMIT = 100;
const CONSOLE_BUFFER_LIMIT = 100;

const DEFAULT_LIST_LIMIT = 20;
const MAX_LIST_LIMIT = 100;

const LIST_URL_LIMIT = 300;
const LIST_TEXT_LIMIT = 400;
const BODY_PREVIEW_LIMIT = 1000;

// 敏感键按「词」判断，不做子串匹配，避免 auth 命中 author 这类误伤。
// 键先按分隔符和驼峰拆词，再看整词或相邻词对。
const SENSITIVE_WORDS = new Set([
  'auth',
  'authorization',
  'authentication',
  'bearer',
  'cookie',
  'cookies',
  'credential',
  'credentials',
  'csrf',
  'jwt',
  'pass',
  'passwd',
  'password',
  'pwd',
  'secret',
  'secrets',
  'sessionid',
  'signature',
  'ticket',
  'tickets',
  'token',
  'tokens',
  'xsrf',
]);

const SENSITIVE_PAIRS = new Set([
  'access key',
  'api key',
  'client secret',
  'private key',
  'secret key',
  'secret token',
  'session id',
  'session key',
]);

const BINARY_CONTENT_TYPE = /^(image|audio|video|font)\/|^application\/(octet-stream|zip|pdf|wasm)/i;

const RESOURCE_TYPE_ALIASES = {
  js: 'script',
  img: 'image',
  css: 'stylesheet',
  doc: 'document',
};

const LEVEL_ALIASES = {
  warn: 'warning',
  err: 'error',
};

const FORM_PAIR = /^[^=&\s]+=/;

function keyWords(key) {
  return String(key == null ? '' : key)
    // 先拆缩写边界（APIKey → API_Key、CSRFToken → CSRF_Token），
    // 再拆普通驼峰（accessToken → access_Token）。
    .replace(/([A-Z]+)([A-Z][a-z])/g, '$1_$2')
    .replace(/([a-z0-9])([A-Z])/g, '$1_$2')
    .toLowerCase()
    .split(/[^a-z0-9]+/)
    .filter(Boolean);
}

function isSensitiveKey(key) {
  const words = keyWords(key);
  for (const word of words) {
    if (SENSITIVE_WORDS.has(word)) {
      return true;
    }
  }
  for (let index = 0; index + 1 < words.length; index += 1) {
    if (SENSITIVE_PAIRS.has(`${words[index]} ${words[index + 1]}`)) {
      return true;
    }
  }
  return false;
}

function headerValue(headers, name) {
  const target = String(name).toLowerCase();
  for (const [key, value] of Object.entries(headers || {})) {
    if (String(key).toLowerCase() === target) {
      return String(value == null ? '' : value);
    }
  }
  return '';
}

function truncate(value, limit) {
  const text = String(value == null ? '' : value);
  return text.length > limit ? `${text.slice(0, limit)}…` : text;
}

function maskHeaders(headers) {
  const result = {};
  for (const [key, value] of Object.entries(headers || {})) {
    if (isSensitiveKey(key)) {
      result[key] = '***';
    } else if (/^location$/i.test(key)) {
      // 重定向目标常带一次性 ticket，按 URL 规则打码。
      result[key] = maskURL(value);
    } else {
      result[key] = String(value);
    }
  }
  return result;
}

// maskJSONValue 递归打码 JSON 里的敏感字段。
function maskJSONValue(value, depth) {
  if (depth > 8 || value == null || typeof value !== 'object') {
    return value;
  }
  if (Array.isArray(value)) {
    return value.map((item) => maskJSONValue(item, depth + 1));
  }
  const result = {};
  for (const [key, entry] of Object.entries(value)) {
    result[key] = isSensitiveKey(key) ? '***' : maskJSONValue(entry, depth + 1);
  }
  return result;
}

function decodeKey(raw) {
  try {
    return decodeURIComponent(raw);
  } catch {
    return raw;
  }
}

function maskFormBody(text) {
  return text
    .split('&')
    .map((pair) => {
      const index = pair.indexOf('=');
      if (index <= 0) {
        return pair;
      }
      const key = pair.slice(0, index);
      return isSensitiveKey(decodeKey(key)) ? `${key}=***` : pair;
    })
    .join('&');
}

// maskBody 打码请求体 / 响应体：JSON 按键递归，表单按参数名，其余原样返回。
function maskBody(text, contentType) {
  const raw = String(text == null ? '' : text);
  const trimmed = raw.trim();
  if (!trimmed) {
    return '';
  }

  if (trimmed.startsWith('{') || trimmed.startsWith('[')) {
    try {
      return JSON.stringify(maskJSONValue(JSON.parse(trimmed), 0));
    } catch {
      /* 不是合法 JSON，往下按文本处理 */
    }
  }

  // 表单只要求「每段都是 k=v」，值里带未编码空格也照常识别。
  const looksLikeForm = trimmed.split('&').every((pair) => FORM_PAIR.test(pair));
  if (looksLikeForm || /x-www-form-urlencoded/i.test(String(contentType || ''))) {
    return maskFormBody(trimmed);
  }

  return raw;
}

// maskURL 只打码查询串里的敏感参数，路径和主机保持不变。
// 相对地址（例如重定向的 Location）无法用 URL 解析，按同样的规则手工处理。
function maskURL(raw) {
  const text = String(raw == null ? '' : raw);
  try {
    const parsed = new URL(text);
    let masked = false;
    for (const key of Array.from(parsed.searchParams.keys())) {
      if (isSensitiveKey(key)) {
        parsed.searchParams.set(key, '***');
        masked = true;
      }
    }
    return masked ? parsed.toString() : text;
  } catch {
    /* 相对地址，走下面的手工分支 */
  }

  const queryStart = text.indexOf('?');
  if (queryStart < 0) {
    return text;
  }
  const head = text.slice(0, queryStart);
  const rest = text.slice(queryStart + 1);
  const hashStart = rest.indexOf('#');
  const query = hashStart >= 0 ? rest.slice(0, hashStart) : rest;
  const hash = hashStart >= 0 ? rest.slice(hashStart) : '';
  const maskedQuery = query
    .split('&')
    .map((pair) => {
      const index = pair.indexOf('=');
      if (index <= 0) {
        return pair;
      }
      return isSensitiveKey(decodeKey(pair.slice(0, index))) ? `${pair.slice(0, index)}=***` : pair;
    })
    .join('&');
  return maskedQuery === query ? text : `${head}?${maskedQuery}${hash}`;
}

function normalizeLimit(value) {
  const parsed = Number(value);
  if (!Number.isFinite(parsed) || parsed <= 0) {
    return DEFAULT_LIST_LIMIT;
  }
  return Math.min(Math.round(parsed), MAX_LIST_LIMIT);
}

function splitFilter(value) {
  if (Array.isArray(value)) {
    return value.map((item) => String(item == null ? '' : item).trim().toLowerCase()).filter(Boolean);
  }
  return String(value == null ? '' : value)
    .split(',')
    .map((item) => item.trim().toLowerCase())
    .filter(Boolean);
}

function normalizeTypeFilter(value) {
  return splitFilter(value).map((item) => RESOURCE_TYPE_ALIASES[item] || item);
}

function normalizeLevelFilter(value) {
  return splitFilter(value).map((item) => LEVEL_ALIASES[item] || item);
}

// parseStatusFilter 支持 404、>=400、<300、400-499、5xx 这几种写法。
function parseStatusFilter(value) {
  const text = String(value == null ? '' : value).trim().toLowerCase();
  if (!text) {
    return null;
  }

  let match = /^([<>]=?)\s*(\d{3})$/.exec(text);
  if (match) {
    const operator = match[1];
    const target = Number(match[2]);
    if (operator === '>') return (status) => status > target;
    if (operator === '>=') return (status) => status >= target;
    if (operator === '<') return (status) => status < target;
    return (status) => status <= target;
  }

  match = /^([1-5])xx$/.exec(text);
  if (match) {
    const base = Number(match[1]) * 100;
    return (status) => status >= base && status < base + 100;
  }

  match = /^(\d{3})\s*-\s*(\d{3})$/.exec(text);
  if (match) {
    const low = Number(match[1]);
    const high = Number(match[2]);
    return (status) => status >= low && status <= high;
  }

  if (/^\d{3}$/.test(text)) {
    const target = Number(text);
    return (status) => status === target;
  }

  throw new Error(`status 过滤不支持 ${text}；可用 404、>=400、400-499、5xx`);
}

function matchesLevel(type, levels) {
  if (levels.length === 0) {
    return true;
  }
  const normalized = LEVEL_ALIASES[type] || type;
  return levels.some((level) => level === normalized || (level === 'error' && type === 'pageerror'));
}

function safeText(value) {
  try {
    return String(value == null ? '' : value);
  } catch {
    return '';
  }
}

// createPageObserver 挂上监听并返回两个查询入口。
//
// 记录按会话维度收集（不区分标签页），每条都带 pageId 和 pageUrl：pageId 只用来
// 在页面关闭时清理引用，pageUrl 让多个标签页的记录能分清。
function createPageObserver(context) {
  const network = [];
  const messages = [];
  const inflight = new WeakMap();
  let nextPageID = 1;
  let nextNetworkID = 1;
  let nextMessageID = 1;

  // pageURLOf 取记录归属的页面地址。
  //
  // 顶层文档请求发出时 page.url() 还是旧地址（新标签页就是 about:blank），
  // 这种情况下用请求自身的 URL，否则「这条记录属于哪个页面」会看不出来。
  const pageURLOf = (page, request) => {
    let current = '';
    try {
      current = page && !page.isClosed() ? String(page.url() || '') : '';
    } catch {
      current = '';
    }
    if (current && current !== 'about:blank') {
      return current;
    }
    try {
      if (request && request.frame() === page.mainFrame()) {
        return String(request.url() || '') || current;
      }
    } catch {
      /* Service Worker 等没有 frame 的请求 */
    }
    return current;
  };

  const pushNetwork = (record) => {
    record.id = nextNetworkID;
    nextNetworkID += 1;
    network.push(record);
    if (network.length > NETWORK_BUFFER_LIMIT) {
      network.shift();
    }
  };

  const pushMessage = (record) => {
    record.id = nextMessageID;
    nextMessageID += 1;
    messages.push(record);
    if (messages.length > CONSOLE_BUFFER_LIMIT) {
      messages.shift();
    }
  };

  // releasePage 在页面关闭时清掉记录里的 Playwright 对象。
  //
  // 记录本身保留（刚关掉的标签页往往正是要查的），但 request / response 会
  // 拖住已关闭页面的上下文，必须断开；没结束的请求也不会再触发 requestfailed，
  // 这里一并标成已结束，免得永远挂在 pending。
  const releasePage = (pageID) => {
    for (const record of network) {
      if (record.pageID !== pageID) {
        continue;
      }
      if (record.pending) {
        record.pending = false;
        record.durationMs = Math.max(0, Date.now() - record.startedMs);
        record.failed = record.failed || '页面已关闭';
      }
      record.request = null;
      record.response = null;
    }
  };

  const attach = (page) => {
    const pageID = nextPageID;
    nextPageID += 1;

    page.on('request', (request) => {
      let postData = '';
      try {
        postData = request.postData() || '';
      } catch {
        postData = '';
      }
      let headers = {};
      try {
        // 列表阶段只取同步头，避免每个静态资源都多一次 CDP 往返。
        headers = request.headers() || {};
      } catch {
        headers = {};
      }

      const record = {
        id: 0,
        pageID,
        method: safeText(request.method()),
        url: safeText(request.url()),
        resourceType: safeText(request.resourceType()),
        startedAt: new Date().toISOString(),
        startedMs: Date.now(),
        status: 0,
        statusText: '',
        durationMs: 0,
        pending: true,
        failed: '',
        pageUrl: pageURLOf(page, request),
        headers,
        postData,
        responseHeaders: null,
        request,
        response: null,
      };
      inflight.set(request, record);
      pushNetwork(record);
    });

    page.on('response', (response) => {
      const record = inflight.get(response.request());
      if (!record) {
        return;
      }
      try {
        record.status = response.status();
        record.statusText = safeText(response.statusText());
        record.responseHeaders = response.headers();
      } catch {
        /* 导航中读响应头可能失败，保留已有字段 */
      }
      record.response = response;
    });

    const settle = (request, failure) => {
      const record = inflight.get(request);
      if (!record) {
        return;
      }
      record.pending = false;
      record.durationMs = Math.max(0, Date.now() - record.startedMs);
      if (failure) {
        record.failed = safeText(failure.errorText) || 'request failed';
      }
      inflight.delete(request);
    };

    page.on('requestfinished', (request) => settle(request, null));
    page.on('requestfailed', (request) => settle(request, request.failure()));

    page.on('console', (message) => {
      let source = '';
      try {
        const location = message.location() || {};
        if (location.url) {
          source = `${location.url}:${Number(location.lineNumber) || 0}:${Number(location.columnNumber) || 0}`;
        }
      } catch {
        source = '';
      }
      pushMessage({
        id: 0,
        type: safeText(message.type()) || 'log',
        text: safeText(message.text()),
        source,
        pageUrl: pageURLOf(page),
      });
    });

    page.on('pageerror', (error) => {
      pushMessage({
        id: 0,
        type: 'pageerror',
        text: safeText(error && error.message ? error.message : error),
        source: '',
        pageUrl: pageURLOf(page),
      });
    });

    page.on('close', () => releasePage(pageID));
  };

  for (const page of context.pages()) {
    attach(page);
  }
  context.on('page', attach);

  const summarize = (record) => {
    const item = {
      id: record.id,
      method: record.method,
      url: truncate(maskURL(record.url), LIST_URL_LIMIT),
      status: record.status,
      resourceType: record.resourceType,
      durationMs: record.durationMs,
      pageUrl: truncate(maskURL(record.pageUrl), LIST_URL_LIMIT),
    };
    if (record.pending) {
      item.pending = true;
    }
    if (record.failed) {
      item.failed = record.failed;
    }
    return item;
  };

  const detailOf = async (record) => {
    let requestHeaders = record.headers || {};
    if (record.request) {
      // 详情才取全部头（含 Cookie），列表阶段不做这次 CDP 查询。
      try {
        requestHeaders = (await record.request.allHeaders()) || requestHeaders;
      } catch {
        /* 请求已随页面销毁，退回同步头 */
      }
    }

    const requestContentType = headerValue(requestHeaders, 'content-type');
    const responseContentType = headerValue(record.responseHeaders, 'content-type');
    const detail = {
      id: record.id,
      method: record.method,
      url: maskURL(record.url),
      status: record.status,
      statusText: record.statusText,
      resourceType: record.resourceType,
      durationMs: record.durationMs,
      pending: record.pending,
      failed: record.failed,
      pageUrl: maskURL(record.pageUrl),
      startedAt: record.startedAt,
      requestHeaders: maskHeaders(requestHeaders),
      requestBody: truncate(maskBody(record.postData, requestContentType), BODY_PREVIEW_LIMIT),
      responseHeaders: maskHeaders(record.responseHeaders || {}),
    };

    if (!record.response) {
      return detail;
    }
    if (BINARY_CONTENT_TYPE.test(responseContentType)) {
      detail.responseBody = `[${responseContentType || 'binary'} 内容未读取]`;
      return detail;
    }
    try {
      detail.responseBody = truncate(
        maskBody(await record.response.text(), responseContentType),
        BODY_PREVIEW_LIMIT
      );
      detail.responseType = responseContentType;
    } catch (error) {
      detail.responseBody = `[响应体不可读：${error && error.message ? error.message : String(error)}]`;
    }
    return detail;
  };

  const listNetwork = async (args) => {
    const input = args && typeof args === 'object' ? args : {};

    const id = Number(input.id);
    if (Number.isFinite(id) && id > 0) {
      const record = network.find((item) => item.id === id);
      if (!record) {
        throw new Error(`没有编号为 ${id} 的请求；缓冲区里现有 ${network.length} 条`);
      }
      return { detail: await detailOf(record) };
    }

    const limit = normalizeLimit(input.limit);
    const types = normalizeTypeFilter(input.type);
    const keyword = String(input.url == null ? '' : input.url).trim().toLowerCase();
    const matchStatus = parseStatusFilter(input.status);

    const matched = network.filter((record) => {
      if (types.length > 0 && !types.includes(record.resourceType.toLowerCase())) {
        return false;
      }
      if (keyword && !record.url.toLowerCase().includes(keyword)) {
        return false;
      }
      // 没有响应（未结束或已失败）的记录没有状态码，不参与状态码过滤，
      // 否则 0 会被 <300 这类条件误判为命中。
      return !matchStatus || (record.status > 0 && matchStatus(record.status));
    });

    const items = matched.slice(-limit).map(summarize);
    return { total: network.length, matched: matched.length, returned: items.length, items };
  };

  const listConsole = async (args) => {
    const input = args && typeof args === 'object' ? args : {};
    const limit = normalizeLimit(input.limit);
    const levels = normalizeLevelFilter(input.level);

    const matched = messages.filter((record) => matchesLevel(record.type, levels));
    const items = matched.slice(-limit).map((record) => ({
      id: record.id,
      type: record.type,
      text: truncate(record.text, LIST_TEXT_LIMIT),
      source: record.source,
      pageUrl: truncate(maskURL(record.pageUrl), LIST_URL_LIMIT),
    }));
    return { total: messages.length, matched: matched.length, returned: items.length, items };
  };

  return {
    network: listNetwork,
    console: listConsole,
  };
}

module.exports = {
  createPageObserver,
  isSensitiveKey,
  maskHeaders,
  maskBody,
  maskURL,
};
