// 登录鉴权逻辑静态校验：抽取 <script setup> 内容，在 mock 依赖下执行，
// 断言 checkAuth / login / logout / handleUnauthorized 的数据流正确。
const fs = require('fs')
const path = require('path')
const vueCompiler = require('@vue/compiler-sfc')

const file = path.join(__dirname, 'src', 'App.vue')
const source = fs.readFileSync(file, 'utf8')
const { descriptor } = vueCompiler.parse(source)
const script = descriptor.scriptSetup.content

// 去掉 import 行（依赖由 new Function 注入），其余按组件内声明执行
const body = script
  .split('\n')
  .filter((l) => !/^\s*import\s/.test(l))
  .join('\n')

const vueRef = (v) => ({ value: v })
const vueReactive = (o) => o
const vueComputed = (fn) => ({ get value() { return fn() } })
const noop = () => {}
const antdMessage = { success: noop, error: noop, warning: noop }
const UploadOutlined = 'UploadOutlined'
const LockOutlined = 'LockOutlined'

// 可控的 fetch 行为
let meAuthed = false
let loginResult = { ok: true }
const fetchMock = (url, opts) => {
  let data = {}
  if (url === '/api/me') data = { authenticated: meAuthed }
  else if (url === '/api/login') data = loginResult
  else if (url === '/api/hello') data = { message: 'hi' }
  else if (url === '/api/nginx') data = { supported: false, instances: [] }
  else if (url === '/api/nginx/available') data = { supported: false, versions: [] }
  else if (url === '/api/nginx/sites') data = { supported: true, sites: [] }
  return Promise.resolve({
    ok: true,
    status: 200,
    json: () => Promise.resolve(data),
  })
}

const factory = new Function(
  'ref',
  'reactive',
  'computed',
  'onMounted',
  'onUnmounted',
  'watch',
  'nextTick',
  'fetch',
  'antdMessage',
  'UploadOutlined',
  'LockOutlined',
  body +
    '\nreturn { loggedIn, loginForm, loginError, loginLoading, checkAuth, login, logout, handleUnauthorized, sites }',
)

const ctx = factory(
  vueRef,
  vueReactive,
  vueComputed,
  noop,
  noop,
  noop,
  noop,
  fetchMock,
  antdMessage,
  UploadOutlined,
  LockOutlined,
)

let failures = 0
function assert(cond, msg) {
  if (!cond) {
    console.error('FAIL:', msg)
    failures++
  } else {
    console.log('PASS:', msg)
  }
}

;(async () => {
  // 1) handleUnauthorized 判定
  assert(ctx.handleUnauthorized({ status: 401 }) === true, '401 应识别为未授权')
  assert(ctx.handleUnauthorized({ status: 200 }) === false, '200 不应视为未授权')

  // 2) checkAuth：未登录态后端返回 authenticated:false
  meAuthed = false
  await ctx.checkAuth()
  assert(ctx.loggedIn.value === false, 'checkAuth 未登录应保持 false')

  // 3) checkAuth：已登录态后端返回 authenticated:true，并加载数据
  meAuthed = true
  await ctx.checkAuth()
  assert(ctx.loggedIn.value === true, 'checkAuth 已登录应置为 true')
  assert(ctx.sites.value !== null, 'checkAuth 已登录应触发 loadSites')

  // 4) login：密码正确 -> 登录成功
  meAuthed = true // login 成功后 afterLoginLoad 会再查 /api/me，保持 true
  loginResult = { ok: true }
  ctx.loginForm.password = 'admin'
  await ctx.login()
  assert(ctx.loggedIn.value === true, 'login 成功应置 loggedIn=true')
  assert(ctx.loginForm.password === '', 'login 成功应清空密码框')
  assert(ctx.loginError.value === '', 'login 成功不应有错误')

  // 5) login：密码错误 -> 登录失败（保持未登录）
  ctx.loggedIn.value = false
  loginResult = { ok: false, error: '密码错误' }
  await ctx.login()
  assert(ctx.loggedIn.value === false, 'login 失败应保持 loggedIn=false')
  assert(ctx.loginError.value === '密码错误', 'login 失败应显示后端错误')

  // 6) logout：退出后回到未登录
  ctx.loggedIn.value = true
  await ctx.logout()
  assert(ctx.loggedIn.value === false, 'logout 应置 loggedIn=false')
  assert(ctx.sites.value === null, 'logout 应清空站点数据')

  console.log(failures === 0 ? '\nALL OK' : `\n${failures} FAILURES`)
  process.exit(failures === 0 ? 0 : 1)
})()
