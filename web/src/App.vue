<script setup>
import { ref, reactive, computed, onMounted, onUnmounted, watch, nextTick } from 'vue'
import { message as antdMessage } from 'ant-design-vue'
import { UploadOutlined, LockOutlined } from '@ant-design/icons-vue'

const message = ref('加载中...')
const nginx = ref(null)
const available = ref(null)
const loading = ref(false)
const backendError = ref(false)

// ---------------------------------------------------------------- 登录鉴权
// 进入安装与配置页面前需先登录。登录态由后端会话 Cookie 维持，前端仅根据
// /api/me 判断展示登录页还是主应用；所有 /api/nginx* 接口由后端强制校验。
const loggedIn = ref(false)
const loginForm = reactive({ password: '' })
const loginError = ref('')
const loginLoading = ref(false)

// 检查当前登录态：已登录则拉取初始数据，否则停留在登录页
async function checkAuth() {
  try {
    const res = await fetch('/api/me', { credentials: 'same-origin' })
    const data = await res.json()
    if (data.authenticated) {
      loggedIn.value = true
      await afterLoginLoad()
    } else {
      loggedIn.value = false
    }
  } catch {
    loggedIn.value = false
  }
}

// 登录成功后执行的初始化加载（与 onMounted 原逻辑一致）
async function afterLoginLoad() {
  try {
    const res = await fetch('/api/hello')
    const data = await res.json()
    message.value = data.message
  } catch {
    message.value = '后端连接失败'
  }
  await detect()
  await loadSites()
}

async function login() {
  loginLoading.value = true
  loginError.value = ''
  try {
    const res = await fetch('/api/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      credentials: 'same-origin',
      body: JSON.stringify({ password: loginForm.password }),
    })
    const data = await res.json()
    if (!res.ok || data.error) {
      loginError.value = data.error || '登录失败'
      return
    }
    loggedIn.value = true
    loginForm.password = ''
    await afterLoginLoad()
  } catch (e) {
    loginError.value = '登录请求失败: ' + e.message
  } finally {
    loginLoading.value = false
  }
}

async function logout() {
  try {
    await fetch('/api/logout', { method: 'POST', credentials: 'same-origin' })
  } catch {
    // 忽略登出网络错误，前端直接回到登录页
  }
  loggedIn.value = false
  sites.value = null
}

// 接口返回 401 视为登录失效，自动退回登录页
function handleUnauthorized(res) {
  if (res && res.status === 401) {
    loggedIn.value = false
    return true
  }
  return false
}

// 版本号归一化：两个接口返回的版本格式并不一致，需要提取主版本段才能比对。
//   nginx -v           -> nginx/1.24.0        -> 1.24.0
//   apt-cache madison  -> 1.24.0-2ubuntu7.1   -> 1.24.0
//   dnf/yum list       -> 1.24.0-1.el8        -> 1.24.0
//   apk policy         -> 1.24.0-r6           -> 1.24.0
//   带 epoch 的版本    -> 1:1.24.0-1          -> 1.24.0
function normalizeVersion(raw) {
  const text = String(raw ?? '').trim()
  const matched = text.match(/\d[\w.:+-]*/)
  if (!matched) return ''
  const withoutEpoch = matched[0].replace(/^\d+:/, '')
  const core = withoutEpoch.match(/^\d+(?:\.\d+)*/)
  return core ? core[0] : withoutEpoch
}

// 已安装 nginx 的主版本集合，用于判断列表中每一行是否已安装
const installedSet = computed(() => {
  const set = new Set()
  for (const item of nginx.value?.instances ?? []) {
    const version = normalizeVersion(item.version)
    if (version) set.add(version)
  }
  return set
})

// 列表行：可安装版本 + 对应安装状态
const rows = computed(() => {
  const versions = available.value?.versions ?? []
  return versions.map((version) => {
    const core = normalizeVersion(version)
    return {
      key: version,
      version,
      installed: core ? installedSet.value.has(core) : false,
    }
  })
})

const installedCount = computed(() => rows.value.filter((row) => row.installed).length)

const unsupported = computed(
  () => nginx.value?.supported === false || available.value?.supported === false,
)

const columns = [
  { title: '版本号', dataIndex: 'version' },
  { title: '状态', dataIndex: 'installed', width: 190, align: 'center' },
]

// 点击「检测」重新拉取检测结果与可安装版本，刷新列表
async function detect() {
  loading.value = true
  backendError.value = false
  try {
    const [nginxRes, availableRes] = await Promise.all([
      fetch('/api/nginx'),
      fetch('/api/nginx/available'),
    ])
    if (handleUnauthorized(nginxRes) || handleUnauthorized(availableRes)) return
    if (!nginxRes.ok || !availableRes.ok) {
      throw new Error('请求失败')
    }
    nginx.value = await nginxRes.json()
    available.value = await availableRes.json()
  } catch {
    backendError.value = true
    nginx.value = { supported: false, installed: false, instances: [] }
    available.value = { supported: false, manager: '', versions: [] }
  } finally {
    loading.value = false
  }
  // 检测结果刷新后同步刷新安装记录，安装完成时也能立即看到新记录
  await refreshInstalls()
}

// ---------------------------------------------------------------- 安装记录
// 编译安装后文件分散在多个目录，后端会把关键路径持久化，这里读取并展示。
const installs = ref([])
const recordPath = ref('')

async function refreshInstalls() {
  try {
    const res = await fetch('/api/nginx/installs')
    const data = await res.json()
    installs.value = data.records ?? []
    recordPath.value = data.recordPath ?? ''
  } catch {
    installs.value = []
    recordPath.value = ''
  }
}

// 记录按安装时间倒序，首条即最近一次安装
const latestRecord = computed(() => installs.value[0] ?? null)

function formatTime(value) {
  if (!value) return '-'
  const d = new Date(value)
  return Number.isNaN(d.getTime()) ? value : d.toLocaleString('zh-CN')
}

// ---------------------------------------------------------------- 安装流程
// 编译安装耗时数分钟，后端做成异步任务：先拿 taskId，再轮询进度与日志。
const installVisible = ref(false)
const installStarting = ref(false)
const installTaskId = ref('')
const installStatus = ref('')
const installError = ref('')
const installLogs = ref([])
const installVersion = ref('')
const logBoxRef = ref(null)
const installRunning = computed(() => installStatus.value === 'running')

let pollTimer = null

function stopPolling() {
  if (pollTimer) {
    clearInterval(pollTimer)
    pollTimer = null
  }
}

async function pollInstall() {
  if (!installTaskId.value) return
  try {
    const res = await fetch(
      '/api/nginx/install/status?taskId=' + encodeURIComponent(installTaskId.value),
    )
    const data = await res.json()
    installStatus.value = data.status
    installLogs.value = data.logs ?? []
    installError.value = data.error ?? ''
    if (data.status !== 'running') {
      stopPolling()
      // 安装成功后重新检测，让列表里的「已安装」标记立即更新
      if (data.status === 'success') {
        await detect()
      }
    }
  } catch (e) {
    installError.value = '获取进度失败: ' + e.message
  }
}

async function startInstall(record) {
  installVersion.value = record.version
  installStarting.value = true
  installLogs.value = []
  installError.value = ''
  installTaskId.value = ''
  try {
    const res = await fetch('/api/nginx/install', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ version: record.version }),
    })
    const data = await res.json()
    if (data.error) {
      installError.value = data.error
      installStatus.value = 'failed'
      installVisible.value = true
      return
    }
    installTaskId.value = data.taskId
    installStatus.value = 'running'
    installVisible.value = true
    await pollInstall()
    if (installStatus.value === 'running' && !pollTimer) {
      pollTimer = setInterval(pollInstall, 2000)
    }
  } catch (e) {
    installError.value = '发起安装失败: ' + e.message
    installStatus.value = 'failed'
    installVisible.value = true
  } finally {
    installStarting.value = false
  }
}

// 安装中关闭窗口不会中断后端任务，仅停止前端轮询
function closeInstall() {
  stopPolling()
  installVisible.value = false
}

onUnmounted(stopPolling)

// ---------------------------------------------------------------- 卸载流程
// 卸载同样异步执行：先拿 taskId，复用 /api/nginx/install/status 轮询进度与日志。
const uninstallVisible = ref(false)
const uninstallStarting = ref(false)
const uninstallTaskId = ref('')
const uninstallStatus = ref('')
const uninstallError = ref('')
const uninstallLogs = ref([])
const uninstallVersion = ref('')
const uninstallLogBoxRef = ref(null)
const uninstallRunning = computed(() => uninstallStatus.value === 'running')

let uninstallTimer = null

function stopUninstallPolling() {
  if (uninstallTimer) {
    clearInterval(uninstallTimer)
    uninstallTimer = null
  }
}

async function pollUninstall() {
  if (!uninstallTaskId.value) return
  try {
    const res = await fetch(
      '/api/nginx/install/status?taskId=' + encodeURIComponent(uninstallTaskId.value),
    )
    const data = await res.json()
    uninstallStatus.value = data.status
    uninstallLogs.value = data.logs ?? []
    uninstallError.value = data.error ?? ''
    if (data.status !== 'running') {
      stopUninstallPolling()
      // 卸载成功后重新检测，让列表里的「已安装」标记与安装记录立即更新
      if (data.status === 'success') {
        await detect()
      }
    }
  } catch (e) {
    uninstallError.value = '获取进度失败: ' + e.message
  }
}

async function startUninstall(record) {
  uninstallVersion.value = record.version
  uninstallStarting.value = true
  uninstallLogs.value = []
  uninstallError.value = ''
  uninstallTaskId.value = ''
  try {
    const res = await fetch('/api/nginx/uninstall', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ version: record.version }),
    })
    const data = await res.json()
    if (data.error) {
      uninstallError.value = data.error
      uninstallStatus.value = 'failed'
      uninstallVisible.value = true
      return
    }
    uninstallTaskId.value = data.taskId
    uninstallStatus.value = 'running'
    uninstallVisible.value = true
    await pollUninstall()
    if (uninstallStatus.value === 'running' && !uninstallTimer) {
      uninstallTimer = setInterval(pollUninstall, 2000)
    }
  } catch (e) {
    uninstallError.value = '发起卸载失败: ' + e.message
    uninstallStatus.value = 'failed'
    uninstallVisible.value = true
  } finally {
    uninstallStarting.value = false
  }
}

// 卸载中关闭窗口不会中断后端任务，仅停止前端轮询
function closeUninstall() {
  stopUninstallPolling()
  uninstallVisible.value = false
}

onUnmounted(stopUninstallPolling)

// 卸载日志追加后自动滚动到底部
watch(uninstallLogs, () => {
  nextTick(() => {
    const el = uninstallLogBoxRef.value
    if (el) el.scrollTop = el.scrollHeight
  })
})

// 日志追加后自动滚动到底部
watch(installLogs, () => {
  nextTick(() => {
    const el = logBoxRef.value
    if (el) el.scrollTop = el.scrollHeight
  })
})

onMounted(() => {
  checkAuth()
})

// ---------------------------------------------------------------- 站点配置
// 左侧「站点配置」视图：列出 config.d 下的站点，支持新增/编辑/删除与重启 Nginx。
const currentMenu = ref('install')

const sites = ref(null)
const sitesSupported = ref(true)
const nginxInstalled = ref(true)
const configDirPath = ref('')

const siteColumns = [
  { title: '配置文件', dataIndex: 'file' },
  { title: '域名 / IP', dataIndex: 'domain' },
  { title: '端口', dataIndex: 'listen', width: 80, align: 'center' },
  { title: '根目录', dataIndex: 'root' },
  { title: '反代目标', dataIndex: 'proxyPort', width: 200, align: 'center' },
  { title: 'HTTPS', dataIndex: 'ssl', width: 80, align: 'center' },
  { title: '操作', dataIndex: 'action', width: 140, align: 'center' },
]

async function loadSites() {
  sites.value = null
  try {
    const res = await fetch('/api/nginx/sites')
    if (handleUnauthorized(res)) return
    const data = await res.json()
    sitesSupported.value = data.supported !== false
    nginxInstalled.value = data.nginxInstalled === true
    configDirPath.value = data.configDir ?? ''
    sites.value = data.sites ?? []
  } catch {
    sitesSupported.value = true
    nginxInstalled.value = true
    sites.value = []
  }
}

// 新增/编辑弹窗
const siteModalVisible = ref(false)
const siteSaving = ref(false)
const editingFile = ref('')
const siteForm = reactive({
  domain: '',
  listen: 80,
  root: '',
  proxyPort: null, // 反向代理端口；null 表示未启用
  proxyScheme: 'http', // 反代协议：http / https，留空按 http
  proxyHost: '', // 反代上游地址；空表示本机 127.0.0.1
  ssl: false,
  cert: '',
  key: '',
})

// 证书获取方式：upload = 上传证书包，manual = 手动填写路径
const certMode = ref('manual')
const certUploading = ref(false)
const certDir = ref('') // 上传后后端返回的归置目录
const certFiles = ref([]) // 解压出的文件清单

function openAddSite() {
  editingFile.value = ''
  siteForm.domain = ''
  siteForm.listen = 80
  siteForm.root = ''
  siteForm.proxyPort = null
  siteForm.proxyScheme = 'http'
  siteForm.proxyHost = ''
  siteForm.ssl = false
  siteForm.cert = ''
  siteForm.key = ''
  certMode.value = 'manual'
  certDir.value = ''
  certFiles.value = []
  siteModalVisible.value = true
}

function openEditSite(record) {
  editingFile.value = record.file
  siteForm.domain = record.domain || ''
  siteForm.listen = record.listen || 80
  siteForm.root = record.root || ''
  siteForm.proxyPort = record.proxyPort || null
  siteForm.proxyScheme = record.proxyScheme || 'http'
  siteForm.proxyHost = record.proxyHost || ''
  siteForm.ssl = !!record.ssl
  siteForm.cert = record.cert || ''
  siteForm.key = record.key || ''
  // 已填路径时默认展示手动模式，避免误以为证书丢失
  certMode.value = 'manual'
  certDir.value = ''
  certFiles.value = []
  siteModalVisible.value = true
}

// customUpload 接管 a-upload 的默认提交：需带上域名，并自行解析后端响应回填路径
function customUpload({ file, onSuccess, onError }) {
  if (!siteForm.domain.trim()) {
    antdMessage.warning('请先填写域名 / IP，证书需按域名归置')
    onError(new Error('域名为空'))
    return
  }
  const form = new FormData()
  form.append('domain', siteForm.domain.trim())
  form.append('file', file)
  certUploading.value = true
  // 返回 Promise 便于调用方等待（a-upload 本身会忽略返回值）
  return fetch('/api/nginx/sites/cert', { method: 'POST', body: form })
    .then((r) => r.json())
    .then((data) => {
      certUploading.value = false
      if (data.error) {
        antdMessage.error(data.error)
        onError(new Error(data.error))
        return
      }
      siteForm.cert = data.cert || ''
      siteForm.key = data.key || ''
      certDir.value = data.dir || ''
      certFiles.value = data.files || []
      if (!data.cert || !data.key) {
        antdMessage.warning('已解压，但未能自动识别证书或私钥，请检查包内文件或手动填写路径')
      } else {
        antdMessage.success('证书已上传并自动填好路径')
      }
      onSuccess(data)
    })
    .catch((e) => {
      certUploading.value = false
      antdMessage.error('上传失败: ' + e.message)
      onError(e)
    })
}

// beforeCertUpload 上传前拦截超大文件，避免白跑一趟
function beforeCertUpload(file) {
  const tooBig = file.size > 32 * 1024 * 1024
  if (tooBig) {
    antdMessage.error('证书包不能超过 32MB')
  }
  return !tooBig
}

async function saveSite() {
  siteSaving.value = true
  try {
    const url = editingFile.value ? '/api/nginx/sites/update' : '/api/nginx/sites/create'
    const body = {
      domain: siteForm.domain.trim(),
      listen: Number(siteForm.listen) || 80,
      root: siteForm.root.trim(),
      proxyPort: Number(siteForm.proxyPort) || 0,
      proxyScheme: siteForm.proxyScheme || 'http',
      proxyHost: siteForm.proxyHost.trim(),
      ssl: !!siteForm.ssl,
      cert: siteForm.cert.trim(),
      key: siteForm.key.trim(),
    }
    if (editingFile.value) body.file = editingFile.value
    const res = await fetch(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    })
    const data = await res.json()
    if (data.error) {
      antdMessage.error(data.error)
      return
    }
    antdMessage.success(editingFile.value ? '已保存站点配置' : '已新增站点')
    siteModalVisible.value = false
    await loadSites()
  } finally {
    siteSaving.value = false
  }
}

async function deleteSite(record) {
  try {
    const res = await fetch('/api/nginx/sites/delete', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ file: record.file }),
    })
    const data = await res.json()
    if (data.error) {
      antdMessage.error(data.error)
      return
    }
    antdMessage.success('已删除站点 ' + record.file)
    await loadSites()
  } catch (e) {
    antdMessage.error('删除失败: ' + e.message)
  }
}

const reloading = ref(false)

async function reloadNginx() {
  reloading.value = true
  try {
    const res = await fetch('/api/nginx/reload', { method: 'POST' })
    const data = await res.json()
    if (data.ok) {
      antdMessage.success(data.action === 'start' ? 'Nginx 已启动' : 'Nginx 已重载')
      await loadSites()
    } else {
      antdMessage.error(data.error || '重启失败')
      if (data.output) console.error(data.output)
    }
  } catch (e) {
    antdMessage.error('重启请求失败: ' + e.message)
  } finally {
    reloading.value = false
  }
}

function onMenuClick({ key }) {
  currentMenu.value = key
}

watch(currentMenu, (m) => {
  if (m === 'sites') loadSites()
})
</script>

<template>
  <!-- 未登录：展示登录页，登录后才能进入安装与配置页面 -->
  <div v-if="!loggedIn" class="login-wrap">
    <a-card class="login-card" :bordered="false">
      <div class="login-logo">nginx_web</div>
      <div class="login-sub">Nginx 可视化管理控制台</div>
      <a-form layout="vertical" @submit.prevent="login">
        <a-form-item>
          <a-input
            v-model:value="loginForm.password"
            type="password"
            placeholder="请输入登录密码"
            :disabled="loginLoading"
            @press-enter="login"
          >
            <template #prefix><LockOutlined /></template>
          </a-input>
        </a-form-item>
        <a-form-item>
          <a-button type="primary" block :loading="loginLoading" @click="login">
            登录
          </a-button>
        </a-form-item>
        <a-alert v-if="loginError" type="error" show-icon :message="loginError" />
      </a-form>
    </a-card>
  </div>

  <!-- 已登录：主应用 -->
  <a-layout v-else style="min-height: 100vh">
    <a-layout-sider theme="light" :width="200" class="sider">
      <div class="logo">nginx_web</div>
      <a-menu :selected-keys="[currentMenu]" mode="inline" @click="onMenuClick">
        <a-menu-item key="install">安装</a-menu-item>
        <a-menu-item key="sites">站点配置</a-menu-item>
      </a-menu>
    </a-layout-sider>

    <a-layout>
      <a-layout-header class="header">
        <span class="page-title">{{ currentMenu === 'install' ? '安装' : '站点配置' }}</span>
        <span class="message">{{ message }}</span>
        <a-button size="small" @click="logout">退出登录</a-button>
      </a-layout-header>

      <a-layout-content class="content">
        <template v-if="currentMenu === 'install'">
        <a-card title="可安装版本">
        <template #extra>
          <a-button type="primary" :loading="loading" @click="detect">检测</a-button>
        </template>

        <a-spin v-if="available === null" tip="检测中..." />

        <template v-else>
          <a-alert
            v-if="backendError"
            type="error"
            show-icon
            message="后端连接失败，请确认后端服务已启动（./start.sh）"
          />

          <a-alert
            v-else-if="unsupported"
            type="warning"
            show-icon
            message="该功能仅支持 Linux（含 Ubuntu）系统"
          />

          <template v-else>
            <div class="summary">
              <a-space :size="8" wrap>
                <a-tag v-if="available.manager" color="blue">
                  包管理器 {{ available.manager }}
                </a-tag>
                <a-tag v-if="nginx.installed" color="green">
                  已安装 {{ nginx.instances.length }} 个
                </a-tag>
                <a-tag v-else>未检测到 Nginx 安装</a-tag>
                <a-tag v-if="installedCount > 0" color="green">
                  列表中已安装 {{ installedCount }} 个
                </a-tag>
              </a-space>
            </div>

            <div v-if="nginx.installed" class="instances">
              <div v-for="item in nginx.instances" :key="item.path" class="instance">
                <span class="version">{{ item.version }}</span>
                <span class="path">{{ item.path }}</span>
              </div>
            </div>

            <a-alert
              v-if="nginx.installed && rows.length > 0 && installedCount === 0"
              class="notice"
              type="info"
              show-icon
              message="已安装的 Nginx 版本不在可安装列表中，可能是手动编译安装或来自其他软件源"
            />

            <a-alert
              v-if="!available.manager"
              type="info"
              show-icon
              message="当前系统未检测到支持的包管理器（支持 apt / dnf / yum / apk）"
            />
            <a-alert
              v-else-if="rows.length === 0"
              type="info"
              show-icon
              message="未查询到可安装版本"
            />
            <a-table
              v-else
              :columns="columns"
              :data-source="rows"
              :loading="loading"
              row-key="key"
              size="middle"
              :pagination="false"
            >
                <template #bodyCell="{ column, record }">
                <template v-if="column.dataIndex === 'installed'">
                  <a-space :size="4">
                    <a-tag :color="record.installed ? 'green' : 'default'">
                      {{ record.installed ? '已安装' : '未安装' }}
                    </a-tag>
                    <a-popconfirm
                      v-if="!record.installed"
                      title="确认安装该版本？"
                      ok-text="确认安装"
                      cancel-text="取消"
                      @confirm="startInstall(record)"
                    >
                      <template #description>
                        <div class="confirm-text">
                          将下载 nginx 源码并编译安装到
                          <code>/usr/local/nginx</code>
                          ，同时安装编译依赖、启动服务并创建
                          <code>/usr/bin/nginx</code>
                          软链。
                        </div>
                        <div class="confirm-warn">
                          全程约需 5~15 分钟，且需要 root 权限，请勿中途中断。
                        </div>
                      </template>
                      <a-button type="link" size="small" :disabled="installRunning">
                        立即安装
                      </a-button>
                    </a-popconfirm>
                    <a-popconfirm
                      v-if="record.installed"
                      title="确认卸载该版本？"
                      ok-text="确认卸载"
                      cancel-text="取消"
                      :ok-button-props="{ danger: true }"
                      @confirm="startUninstall(record)"
                    >
                      <template #description>
                        <div class="confirm-text">
                          将停止 nginx 服务，并删除
                          <code>/usr/local/nginx</code>
                          安装目录、
                          <code>/usr/bin/nginx</code>
                          软链及源码目录。
                        </div>
                        <div class="confirm-warn">
                          该操作不可恢复，且只卸载本工具编译安装的版本。
                        </div>
                      </template>
                      <a-button
                        type="link"
                        size="small"
                        danger
                        :disabled="installRunning || uninstallRunning"
                      >
                        卸载
                      </a-button>
                    </a-popconfirm>
                  </a-space>
                </template>
              </template>
            </a-table>

            <div v-if="installs.length" class="records">
              <a-divider orientation="left">安装记录</a-divider>
              <a-descriptions
                v-for="rec in installs"
                :key="rec.version"
                :title="'nginx ' + rec.version"
                bordered
                size="small"
                :column="2"
                class="record-item"
              >
                <a-descriptions-item label="安装时间">
                  {{ formatTime(rec.installedAt) }}
                </a-descriptions-item>
                <a-descriptions-item label="安装前缀">{{ rec.prefix }}</a-descriptions-item>
                <a-descriptions-item label="配置文件">{{ rec.configPath }}</a-descriptions-item>
                <a-descriptions-item label="站点配置目录">{{ rec.configDir }}</a-descriptions-item>
                <a-descriptions-item label="执行文件">{{ rec.binPath }}</a-descriptions-item>
                <a-descriptions-item label="全局软链">{{ rec.symlinkPath }}</a-descriptions-item>
                <a-descriptions-item label="日志目录">{{ rec.logDir }}</a-descriptions-item>
                <a-descriptions-item label="源码目录">{{ rec.sourceDir }}</a-descriptions-item>
                <a-descriptions-item label="PID 文件">{{ rec.pidPath }}</a-descriptions-item>
              </a-descriptions>
              <div class="record-path">记录文件：{{ recordPath }}</div>
            </div>
          </template>
        </template>
      </a-card>

      <a-modal
        v-model:open="installVisible"
        :title="'安装 nginx ' + installVersion"
        width="760px"
        :mask-closable="false"
        :closable="!installRunning"
        @cancel="closeInstall"
      >
        <a-alert
          v-if="installStatus === 'running'"
          type="info"
          show-icon
          message="正在安装，可关闭本窗口，任务会在后台继续。"
        />
        <a-alert
          v-else-if="installStatus === 'success'"
          type="success"
          show-icon
          message="安装完成"
        />
        <a-alert
          v-else-if="installStatus === 'failed'"
          type="error"
          show-icon
          message="安装失败"
          :description="installError"
        />

        <a-descriptions
          v-if="installStatus === 'success' && latestRecord"
          bordered
          size="small"
          :column="1"
          class="record-panel"
        >
          <a-descriptions-item label="配置文件">
            {{ latestRecord.configPath }}
          </a-descriptions-item>
          <a-descriptions-item label="站点配置目录">
            {{ latestRecord.configDir }}
          </a-descriptions-item>
          <a-descriptions-item label="执行文件">{{ latestRecord.binPath }}</a-descriptions-item>
          <a-descriptions-item label="全局软链">{{ latestRecord.symlinkPath }}</a-descriptions-item>
          <a-descriptions-item label="日志目录">{{ latestRecord.logDir }}</a-descriptions-item>
        </a-descriptions>

        <div ref="logBoxRef" class="log-view">
          <div v-for="(line, index) in installLogs" :key="index" class="log-line">
            {{ line }}
          </div>
          <div v-if="installLogs.length === 0" class="log-empty">等待日志输出...</div>
        </div>

        <template #footer>
          <a-button @click="closeInstall">
            {{ installRunning ? '后台继续' : '关闭' }}
          </a-button>
        </template>
      </a-modal>

      <a-modal
        v-model:open="uninstallVisible"
        :title="'卸载 nginx ' + uninstallVersion"
        width="760px"
        :mask-closable="false"
        :closable="!uninstallRunning"
        @cancel="closeUninstall"
      >
        <a-alert
          v-if="uninstallStatus === 'running'"
          type="info"
          show-icon
          message="正在卸载，可关闭本窗口，任务会在后台继续。"
        />
        <a-alert
          v-else-if="uninstallStatus === 'success'"
          type="success"
          show-icon
          message="卸载完成"
        />
        <a-alert
          v-else-if="uninstallStatus === 'failed'"
          type="error"
          show-icon
          message="卸载失败"
          :description="uninstallError"
        />

        <div ref="uninstallLogBoxRef" class="log-view">
          <div v-for="(line, index) in uninstallLogs" :key="index" class="log-line">
            {{ line }}
          </div>
          <div v-if="uninstallLogs.length === 0" class="log-empty">等待日志输出...</div>
        </div>

        <template #footer>
          <a-button @click="closeUninstall">
            {{ uninstallRunning ? '后台继续' : '关闭' }}
          </a-button>
        </template>
      </a-modal>
        </template>

        <template v-else-if="currentMenu === 'sites'">
          <a-card title="站点配置">
            <template #extra>
              <a-space>
                <a-button :loading="reloading" @click="reloadNginx">重启 Nginx</a-button>
                <a-button type="primary" @click="openAddSite">新增站点</a-button>
              </a-space>
            </template>

            <a-spin v-if="sites === null" tip="加载中..." />

            <template v-else>
              <a-alert
                v-if="!sitesSupported"
                type="warning"
                show-icon
                message="该功能仅支持 Linux（含 Ubuntu）系统"
              />
              <a-alert
                v-else-if="!nginxInstalled"
                type="info"
                show-icon
                message="尚未安装 Nginx，请先到「安装」页完成安装，站点配置目录生成后此处才会显示站点"
              />

              <template v-else>
                <a-table
                  :columns="siteColumns"
                  :data-source="sites"
                  row-key="file"
                  size="middle"
                  :pagination="false"
                >
                  <template #bodyCell="{ column, record }">
                    <template v-if="column.dataIndex === 'domain'">
                      <span>{{ record.domain || '(默认 _)' }}</span>
                    </template>
                    <template v-else-if="column.dataIndex === 'listen'">
                      {{ record.listen || '-' }}
                    </template>
                    <template v-else-if="column.dataIndex === 'proxyPort'">
                      <span v-if="record.proxyPort">
                        {{ (record.proxyScheme || 'http') }}://{{ record.proxyHost || '127.0.0.1' }}:{{ record.proxyPort }}
                      </span>
                      <span v-else class="muted">—</span>
                    </template>
                    <template v-else-if="column.dataIndex === 'ssl'">
                      <a-tag v-if="record.ssl" color="green">HTTPS</a-tag>
                      <span v-else class="muted">—</span>
                    </template>
                    <template v-else-if="column.dataIndex === 'action'">
                      <a-space :size="4">
                        <a-button type="link" size="small" @click="openEditSite(record)">
                          编辑
                        </a-button>
                        <a-popconfirm
                          title="确认删除该站点配置？"
                          ok-text="删除"
                          cancel-text="取消"
                          :ok-button-props="{ danger: true }"
                          @confirm="deleteSite(record)"
                        >
                          <a-button type="link" size="small" danger>删除</a-button>
                        </a-popconfirm>
                      </a-space>
                    </template>
                  </template>
                </a-table>

                <div v-if="sites.length === 0" class="empty-tip">
                  暂无站点配置，点击右上角「新增站点」添加第一个站点（域名或 IP）。
                </div>
                <div v-else class="record-path">配置目录：{{ configDirPath }}</div>
              </template>
            </template>
          </a-card>

          <a-modal
            v-model:open="siteModalVisible"
            :title="editingFile ? '编辑站点' : '新增站点'"
            :confirm-loading="siteSaving"
            @ok="saveSite"
          >
            <a-form layout="vertical">
              <a-form-item label="域名 / IP (server_name)">
                <a-input
                  v-model:value="siteForm.domain"
                  placeholder="例如 example.com、*.example.com 或 192.168.1.1"
                />
              </a-form-item>
              <a-form-item label="监听端口">
                <a-input-number
                  v-model:value="siteForm.listen"
                  :min="1"
                  :max="65535"
                  style="width: 100%"
                />
              </a-form-item>
              <a-form-item label="反向代理端口" tooltip="填写后将把请求转发到上游（反向代理模式），无需填写网站根目录。上游地址可在下方自定义，留空默认本机 127.0.0.1">
                <a-input-number
                  v-model:value="siteForm.proxyPort"
                  :min="1"
                  :max="65535"
                  :placeholder="siteForm.ssl || siteForm.root ? '留空则静态托管' : '例如 3000'"
                  style="width: 100%"
                />
              </a-form-item>
              <template v-if="siteForm.proxyPort">
                <a-form-item label="反代协议">
                  <a-select v-model:value="siteForm.proxyScheme" style="width: 100%">
                    <a-select-option value="http">http</a-select-option>
                    <a-select-option value="https">https</a-select-option>
                  </a-select>
                </a-form-item>
                <a-form-item label="反代地址" tooltip="上游服务器地址（域名 / IPv4 / IPv6）。留空表示本机 127.0.0.1">
                  <a-input
                    v-model:value="siteForm.proxyHost"
                    placeholder="例如 192.168.1.10 或 [2001:db8::1]，留空则 127.0.0.1"
                  />
                </a-form-item>
              </template>
              <a-form-item label="网站根目录" tooltip="反向代理模式下可留空">
                <a-input
                  v-model:value="siteForm.root"
                  placeholder="例如 /var/www/example.com"
                />
              </a-form-item>
              <a-form-item label="启用 HTTPS">
                <a-switch v-model:checked="siteForm.ssl" />
                <span class="form-hint">启用后需提供证书与私钥</span>
              </a-form-item>
              <template v-if="siteForm.ssl">
                <a-form-item label="证书来源">
                  <a-radio-group v-model:value="certMode" size="small">
                    <a-radio-button value="upload">上传证书</a-radio-button>
                    <a-radio-button value="manual">手动填写路径</a-radio-button>
                  </a-radio-group>
                </a-form-item>

                <template v-if="certMode === 'upload'">
                  <a-form-item
                    label="证书包"
                    tooltip="支持 zip / tar.gz / tgz / tar 压缩包，以及 pem / crt / cer / key 单文件。将按域名解压到 nginx 的 conf/ssl 目录下"
                  >
                    <a-upload-dragger
                      :max-count="1"
                      :show-upload-list="false"
                      :custom-request="customUpload"
                      :before-upload="beforeCertUpload"
                      accept=".zip,.tar.gz,.tgz,.tar,.pem,.crt,.cer,.key"
                    >
                      <p class="upload-icon">
                        <UploadOutlined />
                      </p>
                      <p class="upload-text">
                        {{ certUploading ? '正在上传并解压...' : '点击或拖拽证书包到此处上传' }}
                      </p>
                      <p class="upload-hint">
                        支持 zip / tar.gz / tgz / tar 压缩包；单文件 pem / crt / cer / key 也可直接上传
                      </p>
                    </a-upload-dragger>
                  </a-form-item>
                  <a-form-item v-if="certDir" label="解压结果">
                    <div class="cert-dir">{{ certDir }}</div>
                    <div v-if="certFiles.length" class="cert-files">
                      <a-tag v-for="f in certFiles" :key="f">{{ f }}</a-tag>
                    </div>
                  </a-form-item>
                </template>

                <a-form-item label="证书路径 (ssl_certificate)">
                  <a-input
                    v-model:value="siteForm.cert"
                    placeholder="上传后自动填充，例如 /usr/local/nginx/conf/ssl/a.com/fullchain.pem"
                  />
                </a-form-item>
                <a-form-item label="私钥路径 (ssl_certificate_key)">
                  <a-input
                    v-model:value="siteForm.key"
                    placeholder="上传后自动填充，例如 /usr/local/nginx/conf/ssl/a.com/privkey.pem"
                  />
                </a-form-item>
                <div v-if="certMode === 'upload'" class="form-hint">
                  上传成功后上方两个路径会自动填好，确认无误后点「保存」写入 nginx 配置。
                </div>
              </template>
            </a-form>
          </a-modal>
        </template>
      </a-layout-content>
    </a-layout>
  </a-layout>
</template>

<style scoped>
.header {
  background: #fff;
  display: flex;
  align-items: center;
  gap: 16px;
  padding: 0 24px;
  border-bottom: 1px solid #f0f0f0;
}

.sider {
  border-right: 1px solid #f0f0f0;
}

.logo {
  height: 56px;
  display: flex;
  align-items: center;
  padding: 0 20px;
  font-size: 16px;
  font-weight: 600;
  color: #262626;
  border-bottom: 1px solid #f0f0f0;
}

.page-title {
  font-size: 16px;
  font-weight: 600;
}

.message {
  color: #8c8c8c;
  font-size: 13px;
}

.header > .ant-btn {
  margin-left: auto;
}

.login-wrap {
  min-height: 100vh;
  display: flex;
  align-items: center;
  justify-content: center;
  background: linear-gradient(135deg, #f0f5ff 0%, #f6ffed 100%);
}

.login-card {
  width: 360px;
  border-radius: 10px;
  box-shadow: 0 8px 24px rgba(0, 0, 0, 0.08);
}

.login-logo {
  text-align: center;
  font-size: 22px;
  font-weight: 700;
  color: #262626;
}

.login-sub {
  text-align: center;
  margin: 6px 0 20px;
  color: #8c8c8c;
  font-size: 13px;
}

.content {
  padding: 24px;
}

.summary {
  margin-bottom: 12px;
}

.notice {
  margin-bottom: 16px;
}

.instances {
  margin-bottom: 16px;
  padding: 10px 16px;
  background: #fafafa;
  border-radius: 6px;
}

.instance {
  display: flex;
  gap: 16px;
  align-items: baseline;
  font-size: 13px;
  line-height: 1.9;
}

.version {
  font-family: ui-monospace, Consolas, monospace;
  color: #262626;
}

.path {
  font-family: ui-monospace, Consolas, monospace;
  color: #8c8c8c;
}

.confirm-text {
  max-width: 320px;
  line-height: 1.7;
}

.confirm-warn {
  margin-top: 6px;
  color: #d4380d;
}

.log-view {
  margin-top: 12px;
  max-height: 320px;
  overflow-y: auto;
  padding: 12px;
  background: #fafafa;
  border: 1px solid #f0f0f0;
  border-radius: 6px;
  font-family: ui-monospace, Consolas, monospace;
  font-size: 12px;
  line-height: 1.7;
  white-space: pre-wrap;
  word-break: break-all;
}

.log-line {
  color: #262626;
}

.log-empty {
  color: #bfbfbf;
}

.records {
  margin-top: 20px;
}

.record-item {
  margin-bottom: 12px;
}

.record-path {
  margin-top: 8px;
  font-size: 12px;
  color: #8c8c8c;
}

.empty-tip {
  margin-top: 16px;
  color: #8c8c8c;
  font-size: 13px;
}

.muted {
  color: #bfbfbf;
}

.form-hint {
  margin-left: 8px;
  color: #8c8c8c;
  font-size: 12px;
}

.upload-icon {
  margin: 0 0 8px;
  color: #1677ff;
  font-size: 26px;
  line-height: 1;
}

.upload-text {
  margin: 0;
  color: #262626;
}

.upload-hint {
  margin: 4px 0 0;
  color: #8c8c8c;
  font-size: 12px;
}

.cert-dir {
  color: #595959;
  font-family: monospace;
  font-size: 12px;
  word-break: break-all;
}

.cert-files {
  margin-top: 6px;
}

.record-panel {
  margin-top: 12px;
}
</style>
