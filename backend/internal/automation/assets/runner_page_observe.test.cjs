// 观测器的纯函数测试：node --test backend/internal/automation/assets/runner_page_observe.test.cjs
//
// 采集行为依赖真实浏览器，由端到端验证覆盖；这里只锁打码规则，
// 因为它直接决定凭据会不会流进模型上下文。
const test = require('node:test');
const assert = require('node:assert');
const { isSensitiveKey, maskHeaders, maskBody, maskURL } = require('./runner_page_observe.cjs');

test('敏感键：整词命中', () => {
  for (const key of ['authorization', 'authentication', 'auth', 'cookie', 'set-cookie', 'ticket', 'credentials', 'password', 'pwd', 'secret', 'x-csrf-token']) {
    assert.equal(isSensitiveKey(key), true, key);
  }
});

test('敏感键：驼峰与缩写边界', () => {
  for (const key of ['accessToken', 'privateKey', 'apiKey', 'sessionId', 'APIKey', 'CSRFToken', 'XAuthToken', 'OAuthToken', 'X-Ant-Api-Key']) {
    assert.equal(isSensitiveKey(key), true, key);
  }
});

test('敏感键：不误伤普通字段', () => {
  for (const key of ['author', 'authors', 'monkey', 'sort_key', 'session', 'APIUsage', 'HTTPSProxy', 'content-type']) {
    assert.equal(isSensitiveKey(key), false, key);
  }
});

test('请求头：敏感值打码，Location 走 URL 规则', () => {
  const masked = maskHeaders({ Authorization: 'Bearer abc', 'X-Token': 't', 'Content-Type': 'application/json', location: '/cb?ticket=abc&state=1' });
  assert.equal(masked.Authorization, '***');
  assert.equal(masked['X-Token'], '***');
  assert.equal(masked['Content-Type'], 'application/json');
  assert.equal(masked.location, '/cb?ticket=***&state=1');
});

test('请求体：JSON 与表单都按字段打码', () => {
  assert.equal(maskBody('{"user":"a","password":"p","nested":{"accessToken":"t"}}', 'application/json'), '{"user":"a","password":"***","nested":{"accessToken":"***"}}');
  assert.equal(maskBody('user=a&password=hunter 2&keep=1', ''), 'user=a&password=***&keep=1');
  assert.equal(maskBody('user=a&password=b', 'Application/X-WWW-Form-Urlencoded'), 'user=a&password=***');
  assert.equal(maskBody('plain text', 'text/plain'), 'plain text');
});

test('URL：绝对与相对地址都只打码敏感参数', () => {
  assert.equal(maskURL('https://a.test/cb?token=abc&state=1'), 'https://a.test/cb?token=***&state=1');
  assert.equal(maskURL('/cb?token=abc#frag'), '/cb?token=***#frag');
  assert.equal(maskURL('https://a.test/path'), 'https://a.test/path');
});
