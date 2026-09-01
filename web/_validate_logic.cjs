// 功能校验：抽出 <script setup> 内容，注入 mock 的依赖（vue 函数、antd 消息、fetch），
// 直接调用 openAddSite / openEditSite / saveSite，断言新字段（proxyScheme / proxyHost）
// 的写入、回填与重置逻辑正确。无需启动 dev server。
const fs = require('fs')
const path = require('path')
const { parse } = require('@vue/compiler-sfc')

const file = path.resolve(__dirname, 'src/App.vue')
const source = fs.readFileSync(file, 'utf-8')
const { descriptor } = parse(source, { filename: file })
let code = descriptor.scriptSetup.content

// 去掉 import 语句，改为由 new Function 注入
code = code
  .split('\n')
  .filter((l) => !/^\s*import\s/.test(l))
  .join('\n')

// 收集被保存的请求体
let lastBody = null
const fetchMock = async (url, opts) => {
  if (opts && opts.body) {
    try {
      lastBody = JSON.parse(opts.body)
    } catch (e) {
      lastBody = opts.body
    }
  }
  return {
    ok: true,
    json: async () => ({}),
  }
}
const antdMessage = { error() {}, success() {}, warning() {} }
const UploadOutlined = {}
const vueRef = (v) => ({ value: v })
const vueReactive = (o) => o
const vueComputed = (fn) => ({ get value() { return fn() } })
const noop = () => {}
const vueWatch = noop
const vueNextTick = async () => {}

const factory = new Function(
  'ref', 'reactive', 'computed', 'onMounted', 'onUnmounted', 'watch', 'nextTick',
  'fetch', 'antdMessage', 'UploadOutlined',
  code +
    '\n; return { siteForm, openAddSite, openEditSite, saveSite };',
)
const ctx = factory(
  vueRef, vueReactive, vueComputed, noop, noop, vueWatch, vueNextTick,
  fetchMock, antdMessage, UploadOutlined,
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

async function run() {
  // 1) openAddSite 重置新字段
  ctx.openAddSite()
  assert(ctx.siteForm.proxyScheme === 'http', 'openAddSite 重置 proxyScheme=http')
  assert(ctx.siteForm.proxyHost === '', 'openAddSite 重置 proxyHost 为空')

  // 2) openEditSite 回填新字段
  ctx.openEditSite({ file: 'a.conf', domain: 'svc', listen: 80, proxyPort: 9090, proxyScheme: 'https', proxyHost: '192.168.1.10', ssl: false, cert: '', key: '' })
  assert(ctx.siteForm.proxyScheme === 'https', 'openEditSite 回填 proxyScheme=https')
  assert(ctx.siteForm.proxyHost === '192.168.1.10', 'openEditSite 回填 proxyHost=192.168.1.10')
  assert(ctx.siteForm.proxyPort === 9090, 'openEditSite 回填 proxyPort=9090')

  // 3) saveSite 把新字段写入请求体（新增路径，editingFile 为空）
  ctx.openAddSite()
  ctx.siteForm.domain = 'new.example.com'
  ctx.siteForm.proxyPort = 8080
  ctx.siteForm.proxyScheme = 'http'
  ctx.siteForm.proxyHost = '10.0.0.5'
  await ctx.saveSite()
  assert(lastBody && lastBody.proxyPort === 8080, 'saveSite 发送 proxyPort=8080')
  assert(lastBody.proxyScheme === 'http', 'saveSite 发送 proxyScheme=http')
  assert(lastBody.proxyHost === '10.0.0.5', 'saveSite 发送 proxyHost=10.0.0.5')
  assert(lastBody.domain === 'new.example.com', 'saveSite 发送 domain')
  assert(!('file' in lastBody), '新增路径不应带 file 字段')

  // 4) 编辑路径应带 file
  ctx.openEditSite({ file: 'old.conf', domain: 'x', listen: 80, proxyPort: 3000, proxyScheme: 'https', proxyHost: '1.2.3.4' })
  await ctx.saveSite()
  assert(lastBody.file === 'old.conf', '编辑路径带 file 字段')
}

run().then(() => {
  console.log(failures === 0 ? '\nALL OK' : `\n${failures} FAILURES`)
  process.exit(failures === 0 ? 0 : 1)
})

