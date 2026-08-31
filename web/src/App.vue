<script setup>
import { ref, reactive, computed, onMounted, onUnmounted, watch, nextTick } from 'vue'
import { message as antdMessage } from 'ant-design-vue'

const message = ref('加载中...')
const nginx = ref(null)
const available = ref(null)
const loading = ref(false)
const backendError = ref(false)

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

onMounted(async () => {
  try {
    const res = await fetch('/api/hello')
    const data = await res.json()
    message.value = data.message
  } catch {
    message.value = '后端连接失败'
  }
  await detect()
  await loadSites()
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
  { title: '端口', dataIndex: 'listen', width: 90, align: 'center' },
  { title: '根目录', dataIndex: 'root' },
  { title: '操作', dataIndex: 'action', width: 140, align: 'center' },
]

async function loadSites() {
  sites.value = null
  try {
    const res = await fetch('/api/nginx/sites')
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
const siteForm = reactive({ domain: '', listen: 80, root: '' })

function openAddSite() {
  editingFile.value = ''
  siteForm.domain = ''
  siteForm.listen = 80
  siteForm.root = ''
  siteModalVisible.value = true
}

function openEditSite(record) {
  editingFile.value = record.file
  siteForm.domain = record.domain || ''
  siteForm.listen = record.listen || 80
  siteForm.root = record.root || ''
  siteModalVisible.value = true
}

async function saveSite() {
  siteSaving.value = true
  try {
    const url = editingFile.value ? '/api/nginx/sites/update' : '/api/nginx/sites/create'
    const body = {
      domain: siteForm.domain.trim(),
      listen: Number(siteForm.listen) || 80,
      root: siteForm.root.trim(),
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
  <a-layout style="min-height: 100vh">
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
              <a-form-item label="网站根目录">
                <a-input
                  v-model:value="siteForm.root"
                  placeholder="例如 /var/www/example.com"
                />
              </a-form-item>
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

.record-panel {
  margin-top: 12px;
}
</style>
